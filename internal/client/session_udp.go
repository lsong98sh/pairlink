package client

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lsong98sh/pairlink/internal/pairing"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// localSession 是本地 TCP 连接与 UDP 会话的公共视图（用于 relay 错误的近似匹配）。
type localSession interface {
	serviceID() string
	displayKey() string
}

// udpSession 表示一个「本地 UDP 数据源 + 远端服务」的会话。
//
// 两种承载方式共用本结构：
//   - native：nativeConn 是独立 DialUDP 到 relay udp_listen 的 socket；
//   - tcp_tunnel：carrier 指向共享的 UDP over TCP 承载连接，key 是隧道帧会话标识。
type udpSession struct {
	svcID     string
	connID    string
	src       *net.UDPAddr
	srcKey    string
	listener  *localListener
	startedAt time.Time

	// lastActive 是最近一次收发包的 UnixNano（热路径避免加锁）。
	lastActive atomic.Int64
	// toRelay / toLocal 是双向字节数。
	toRelay atomic.Int64
	toLocal atomic.Int64

	mu       sync.Mutex
	state    sessionState
	key      string // 配对键（收到 PairResult 后填）
	proto    string
	firstPkt []byte // 会话建立前缓存的首个数据报，配对成功后补发

	nativeConn *net.UDPConn
	carrier    *udpTunnelCarrier
}

// displayKey 返回展示用配对键。
func (s *udpSession) displayKey() string {
	s.mu.Lock()
	key := s.key
	s.mu.Unlock()
	if key != "" {
		return key
	}
	return pairing.PairingKey{ServiceID: s.svcID, ClientConnID: s.connID}.String()
}

// serviceID 实现 localSession 接口。
func (s *udpSession) serviceID() string { return s.svcID }

// remoteAddr 返回第三方数据源地址。
func (s *udpSession) remoteAddr() string { return s.srcKey }

// touch 刷新活跃时间。
func (s *udpSession) touch() { s.lastActive.Store(time.Now().UnixNano()) }

// isWaiting 报告会话是否在等待/建立配对。
func (s *udpSession) isWaiting() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state == stateWaitingPair || s.state == stateBinding
}

// isForwarding 报告会话是否已在转发。
func (s *udpSession) isForwarding() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state == stateForwarding
}

// isClosed 报告会话是否已清理。
func (s *udpSession) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state == stateClosed
}

// pairExpired 报告「等待 PairResult」是否已超过 pair_timeout。
func (s *udpSession) pairExpired(now time.Time, timeout time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state == stateWaitingPair && now.Sub(s.startedAt) > timeout
}

// idleExpired 报告转发中的会话是否已空闲超过 timeout。
func (s *udpSession) idleExpired(now time.Time, timeout time.Duration) bool {
	s.mu.Lock()
	state := s.state
	s.mu.Unlock()
	if state != stateForwarding {
		return false
	}
	last := time.Unix(0, s.lastActive.Load())
	return now.Sub(last) > timeout
}

// resolve 记录配对结果并进入绑定阶段；已结束的会话返回 false。
func (s *udpSession) resolve(key pairing.PairingKey, proto string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != stateWaitingPair {
		return false
	}
	s.key = key.String()
	s.proto = proto
	s.state = stateBinding
	return true
}

// setForwarding 把绑定阶段推进到转发阶段；已结束的会话返回 false。
func (s *udpSession) setForwarding() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == stateClosed {
		return false
	}
	s.state = stateForwarding
	return true
}

// setNativeConn 登记原生 UDP socket；会话已关闭时返回 false。
func (s *udpSession) setNativeConn(conn *net.UDPConn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == stateClosed {
		return false
	}
	s.nativeConn = conn
	return true
}

// setCarrier 登记 UDP over TCP 承载连接；会话已关闭时返回 false。
func (s *udpSession) setCarrier(cc *udpTunnelCarrier) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == stateClosed {
		return false
	}
	s.carrier = cc
	return true
}

// snapshot 返回管理页需要的字段快照。
func (s *udpSession) snapshot() (key, proto string, state sessionState, started time.Time, remote string, toRelay, toLocal int64) {
	s.mu.Lock()
	key, proto, state = s.key, s.proto, s.state
	s.mu.Unlock()
	if key == "" {
		key = pairing.PairingKey{ServiceID: s.svcID, ClientConnID: s.connID}.String()
	}
	return key, proto, state, s.startedAt, s.srcKey, s.toRelay.Load(), s.toLocal.Load()
}

// udpSessionByConnID 返回指定 client_conn_id 的 UDP 会话（没有返回 nil）。
func (c *client) udpSessionByConnID(connID string) *udpSession {
	c.udpMu.Lock()
	defer c.udpMu.Unlock()
	return c.udpByConn[connID]
}

// udpBindResendInterval 是原生 UDP 报到帧的重发间隔（约 300ms）。
const udpBindResendInterval = 300 * time.Millisecond

// udpBindMaxResends 是 pair_timeout 内最多重发报到帧的次数。
const udpBindMaxResends = 3

// forwardUDP 建立 UDP 数据面（native 或 tcp_tunnel）并开始双向转发。
func (c *client) forwardUDP(s *udpSession, key pairing.PairingKey, token string) {
	neg := c.negotiated()

	var err error
	if neg.udpMode == protocol.UDPModeTCPTunnel {
		err = c.attachUDPTunnel(s, key, token)
	} else {
		err = c.attachUDPNative(s, key, token)
	}
	if err != nil {
		if c.ctx.Err() != nil {
			c.closeUDPSession(s, "shutdown", nil)
			return
		}
		c.log.Warn("UDP 数据面绑定失败，关闭会话", "event", "bind",
			"service_id", key.ServiceID, "pairing_key", key.String(),
			"detail", "udp_mode="+neg.udpMode, "err", err)
		c.closeUDPSession(s, "bind_fail", err)
		return
	}
	if !s.setForwarding() {
		c.closeUDPSession(s, "closed_before_forward", nil)
		return
	}
	c.log.Info("UDP 数据面已打开", "event", "data_open", "service_id", key.ServiceID,
		"pairing_key", key.String(), "detail", "proto=udp udp_mode="+neg.udpMode)
	c.flushUDPFirstPacket(s)
}

// attachUDPNative 走原生 UDP 数据面：DialUDP → Bind 数据报 → 等 BindOK（丢包重发）。
func (c *client) attachUDPNative(s *udpSession, key pairing.PairingKey, token string) error {
	neg := c.negotiated()
	if neg.udpDataErr != nil {
		return neg.udpDataErr
	}
	if neg.udpDataUDPAddr == nil {
		return errors.New("client: 原生 UDP 数据面地址未解析")
	}
	conn, err := net.DialUDP("udp", nil, neg.udpDataUDPAddr)
	if err != nil {
		return fmt.Errorf("client: 连接 relay UDP 数据面 %s: %w", neg.udpDataAddr, err)
	}
	if !s.setNativeConn(conn) {
		_ = conn.Close()
		return errors.New("client: 会话已关闭")
	}

	frame, err := protocol.EncodeFrame(protocol.MsgBind, &protocol.Bind{
		PairingKey:   key.String(),
		PairingToken: token,
		Role:         protocol.RoleClient,
	})
	if err != nil {
		return err
	}
	if d := c.dialTimeout(); d > 0 {
		_ = conn.SetWriteDeadline(time.Now().Add(d))
	}
	if _, err := conn.Write(frame); err != nil {
		return fmt.Errorf("client: 发送 UDP 报到数据报: %w", err)
	}
	_ = conn.SetWriteDeadline(time.Time{})
	c.log.Info("已发送原生 UDP 报到数据报", "event", "bind",
		"service_id", key.ServiceID, "pairing_key", key.String(),
		"remote", neg.udpDataAddr, "detail", "udp_mode=native carrier=")

	if err := c.awaitBindOKNative(s, key, conn, frame); err != nil {
		return err
	}
	c.tasks.Go(func() { c.readNativeUDP(s, conn) })
	return nil
}

// awaitBindOKNative 在 pair_timeout 内等待 relay 的 MsgBindOK 数据报。
//
// 丢包处理（阶段 6C，docs/protocol.md §5.2.1）：每约 300ms 未收到应答就重发报到帧，
// 最多重发 udpBindMaxResends 次；收到 MsgBindOK 或**首个业务数据报**即视为就绪
// （relay 半开会重复回 BindOK，因此重发是幂等的）。
func (c *client) awaitBindOKNative(s *udpSession, key pairing.PairingKey, conn *net.UDPConn, frame []byte) error {
	timeout := c.pairTimeout()
	deadline := time.Now().Add(timeout)
	buf := make([]byte, protocol.MaxUDPDatagram+1)
	resends := 0
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("client: 等待 relay UDP 报到应答（%s）: 超时", timeout)
		}
		wait := remaining
		if resends < udpBindMaxResends && wait > udpBindResendInterval {
			wait = udpBindResendInterval
		}
		if err := conn.SetReadDeadline(time.Now().Add(wait)); err != nil {
			return err
		}
		n, rerr := conn.Read(buf)
		if rerr != nil {
			if !isTimeoutErr(rerr) {
				return fmt.Errorf("client: 等待 relay UDP 报到应答（%s）: %w", timeout, rerr)
			}
			if resends < udpBindMaxResends {
				resends++
				if _, werr := conn.Write(frame); werr != nil {
					return fmt.Errorf("client: 重发 UDP 报到数据报: %w", werr)
				}
				c.log.Debug("未收到 UDP 报到应答，重发报到数据报", "event", "bind",
					"service_id", key.ServiceID, "pairing_key", key.String(), "attempt", resends)
			}
			continue
		}
		typ, payload, perr := protocol.ParseFrame(buf[:n])
		if perr != nil {
			// 不像 PairLink 帧的数据报：视为业务数据，交给本地目标。
			c.deliverUDPToLocal(s, buf[:n])
			_ = conn.SetReadDeadline(time.Time{})
			return nil
		}
		switch typ {
		case protocol.MsgBindOK:
			_ = conn.SetReadDeadline(time.Time{})
			return nil
		case protocol.MsgError:
			msg, decErr := protocol.Decode(typ, payload)
			if decErr == nil {
				if e, ok := msg.(*protocol.Error); ok {
					return fmt.Errorf("relay 拒绝 UDP 报到: %w", e)
				}
			}
			return errors.New("client: relay 拒绝 UDP 报到")
		default:
			// 业务数据先到：按就绪处理并回写本地。
			c.deliverUDPToLocal(s, buf[:n])
			_ = conn.SetReadDeadline(time.Time{})
			return nil
		}
	}
}

// isTimeoutErr 判断 err 是否是网络超时。
func isTimeoutErr(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// readNativeUDP 读取原生 UDP 数据面回包并回写到本地第三方源地址。
func (c *client) readNativeUDP(s *udpSession, conn *net.UDPConn) {
	buf := make([]byte, protocol.MaxUDPDatagram+1)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			if c.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			c.log.Warn("relay UDP 数据面读取失败，关闭会话", "event", "data_close",
				"service_id", s.svcID, "pairing_key", s.displayKey(), "err", err)
			c.closeUDPSession(s, "relay_read_error", err)
			return
		}
		c.deliverUDPToLocal(s, buf[:n])
	}
}

// sendUDPToRelay 把本地数据报发往 relay（native 直接写 socket，tunnel 写隧道帧）。
func (c *client) sendUDPToRelay(s *udpSession, data []byte) {
	s.mu.Lock()
	state, native, carrier, key := s.state, s.nativeConn, s.carrier, s.key
	s.mu.Unlock()
	if state != stateForwarding {
		return
	}

	var err error
	switch {
	case native != nil:
		var n int
		n, err = native.Write(data)
		if n > 0 {
			s.toRelay.Add(int64(n))
		}
	case carrier != nil:
		if key == "" {
			return
		}
		if !carrier.writeFrame(key, data) {
			if carrier.isClosed() {
				return
			}
			// 队列满导致丢弃：UDP 允许丢包，不关闭会话。
			return
		}
		s.toRelay.Add(int64(len(data)))
	default:
		return
	}
	if err != nil {
		if c.ctx.Err() != nil {
			return
		}
		c.log.Warn("发送 UDP 数据到 relay 失败，关闭会话", "event", "data_close",
			"service_id", s.svcID, "pairing_key", s.displayKey(), "err", err)
		c.closeUDPSession(s, "relay_write_error", err)
	}
}

// deliverUDPToLocal 把 relay 回包回写给对应的本地第三方源地址。
func (c *client) deliverUDPToLocal(s *udpSession, data []byte) {
	if s.isClosed() {
		return
	}
	s.touch()
	l := s.listener
	if l == nil || l.udp == nil {
		return
	}
	n, err := l.udp.WriteToUDP(data, s.src)
	if n > 0 {
		s.toLocal.Add(int64(n))
	}
	if err != nil {
		if c.ctx.Err() != nil {
			// 关停过程中本地监听已关闭，不当作错误处理。
			return
		}
		c.log.Warn("回写本地 UDP 失败，关闭会话", "event", "data_close",
			"service_id", s.svcID, "pairing_key", s.displayKey(), "err", err)
		c.closeUDPSession(s, "local_write_error", err)
	}
}

// flushUDPFirstPacket 在数据面就绪后补发配对期间缓存的首个数据报。
func (c *client) flushUDPFirstPacket(s *udpSession) {
	s.mu.Lock()
	pkt := s.firstPkt
	s.firstPkt = nil
	s.mu.Unlock()
	if len(pkt) > 0 {
		c.sendUDPToRelay(s, pkt)
	}
}

// closeUDPSession 关闭并移除一个本地 UDP 会话（幂等）。
func (c *client) closeUDPSession(s *udpSession, reason string, cause error) {
	s.mu.Lock()
	if s.state == stateClosed {
		s.mu.Unlock()
		return
	}
	s.state = stateClosed
	key := s.key
	proto := s.proto
	native := s.nativeConn
	carrier := s.carrier
	s.firstPkt = nil
	s.mu.Unlock()

	if native != nil {
		_ = native.Close()
	}
	if carrier != nil {
		carrier.unregister(key)
	}

	c.udpMu.Lock()
	delete(c.udpSessions, udpSessionKey{serviceID: s.svcID, src: s.srcKey})
	delete(c.udpByConn, s.connID)
	c.udpMu.Unlock()

	if key == "" {
		key = pairing.PairingKey{ServiceID: s.svcID, ClientConnID: s.connID}.String()
	}
	attrs := []any{
		"event", "udp_session_close",
		"service_id", s.svcID,
		"pairing_key", key,
		"remote", s.srcKey,
		"detail", fmt.Sprintf("reason=%s proto=%s udp_mode=%s to_relay=%d to_local=%d",
			reason, proto, c.negotiated().udpMode, s.toRelay.Load(), s.toLocal.Load()),
	}
	if cause != nil {
		attrs = append(attrs, "err", cause)
	}
	c.log.Info("本地 UDP 会话已清理", attrs...)
}

// oldestWaitingSession 返回最早进入等待状态的本地连接/会话，用于 MsgError 的
// 近似匹配（协议里 MsgError 没有关联 ID）。没有等待中的连接时返回 nil。
func (c *client) oldestWaitingSession() localSession {
	var (
		oldest   localSession
		oldestAt time.Time
	)

	c.tcpMu.Lock()
	for _, s := range c.tcpSessions {
		if !s.isWaiting() {
			continue
		}
		if oldest == nil || s.startedAt.Before(oldestAt) {
			oldest, oldestAt = s, s.startedAt
		}
	}
	c.tcpMu.Unlock()

	c.udpMu.Lock()
	for _, s := range c.udpSessions {
		if !s.isWaiting() {
			continue
		}
		if oldest == nil || s.startedAt.Before(oldestAt) {
			oldest, oldestAt = s, s.startedAt
		}
	}
	c.udpMu.Unlock()

	return oldest
}
