package agent

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/pairing"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// errCarrierClosed 表示承载连接已不可用。
var errCarrierClosed = errors.New("agent: UDP over TCP 承载连接已关闭")

// carrierManager 维护到 relay tcp_listen 的**一条** UDP over TCP 承载连接。
//
// 语义（docs/contract.md §4.6）：
//   - 懒创建：第一个 UDP 会话到来时才拨号；
//   - 断开后其上会话全部失效；新会话到来时允许懒建新连接，但**不做会话迁移/重建**；
//   - 所有写经 writeMu 串行，读循环只分发、不因写阻塞。
type carrierManager struct {
	a   *agent
	mu  sync.Mutex
	cur *carrier
}

// newCarrierManager 创建承载连接管理器。
func newCarrierManager(a *agent) *carrierManager {
	return &carrierManager{a: a}
}

// current 返回当前承载连接（可能为 nil 或已关闭）。
func (m *carrierManager) current() *carrier {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cur
}

// closeAll 关闭承载连接及其上全部会话（关停时调用）。
func (m *carrierManager) closeAll() {
	m.mu.Lock()
	c := m.cur
	m.cur = nil
	m.mu.Unlock()
	if c != nil {
		c.fail(errCarrierClosed)
	}
}

// openSession 把一个 UDP 会话挂到承载连接上，必要时先创建承载连接。
//
// 返回承载连接与其对端地址；错误表示本次会话无法建立。
func (m *carrierManager) openSession(sess *localSession, key pairing.PairingKey, token string, localConn *net.UDPConn) (*carrier, string, error) {
	if m.a.tcpDataAddr == "" {
		return nil, "", errors.New("TCP 数据面地址不可用")
	}
	bind := &protocol.Bind{
		PairingKey:   key.String(),
		PairingToken: token,
		Role:         protocol.RoleAgent,
		Carrier:      protocol.CarrierUDPOverTCP,
	}

	m.mu.Lock()
	c := m.cur
	if c != nil && c.isClosed() {
		c = nil
		m.cur = nil
	}
	if c == nil {
		conn, err := m.a.dialTCP(m.a.ctx, m.a.tcpDataAddr, m.a.dialTimeout())
		if err != nil {
			m.mu.Unlock()
			return nil, "", fmt.Errorf("连接 relay TCP 数据面（UDP 承载）: %w", err)
		}
		c = newCarrier(m.a, conn, m.a.tcpDataAddr)
		m.cur = c
		m.a.wg.Add(1)
		go func() {
			defer m.a.wg.Done()
			c.readLoop()
		}()
		// 首帧必须在任何后续会话帧之前写出：持管理器锁完成这次写以保证顺序。
		if err := c.writeFrame(protocol.MsgBind, bind); err != nil {
			m.cur = nil
			m.mu.Unlock()
			c.fail(err)
			return nil, "", fmt.Errorf("发送承载首帧 MsgBind: %w", err)
		}
		if !c.addSession(key, sess, localConn) {
			m.mu.Unlock()
			return nil, "", errCarrierClosed
		}
		m.mu.Unlock()

		// 等承载连接就绪（relay 回 session="" 的 MsgBindOK 控制帧）。
		if err := c.waitReady(m.a.pairTimeout()); err != nil {
			c.removeSession(key)
			c.fail(err)
			return nil, "", err
		}
		m.a.log.Info("UDP over TCP 承载连接已就绪", "event", "bind",
			"pairing_key", key.String(), "carrier", carrierUDPOverTCP, "remote", c.addr)
		return c, c.addr, nil
	}
	m.mu.Unlock()

	// 后续会话：隧道帧 session=<配对键>，payload 是完整的 MsgBind 帧。
	frame, err := protocol.EncodeFrame(protocol.MsgBind, bind)
	if err != nil {
		return nil, "", fmt.Errorf("编码承载会话报到帧: %w", err)
	}
	if !c.addSession(key, sess, localConn) {
		return nil, "", errCarrierClosed
	}
	if err := c.writeTunnelFrame(key.String(), frame); err != nil {
		c.removeSession(key)
		return nil, "", fmt.Errorf("发送承载会话报到帧: %w", err)
	}
	m.a.log.Info("UDP over TCP 会话已挂到既有承载连接", "event", "bind",
		"pairing_key", key.String(), "carrier", carrierUDPOverTCP, "remote", c.addr)
	return c, c.addr, nil
}

// tunnelSession 是承载连接上的一个 UDP 会话。
type tunnelSession struct {
	sess *localSession
	conn *net.UDPConn
}

// carrier 是一条 UDP over TCP 承载连接。
type carrier struct {
	a    *agent
	conn net.Conn
	addr string

	// writeMu 保证同一连接上的隧道帧串行写出，不会交错。
	writeMu sync.Mutex

	mu       sync.Mutex
	sessions map[pairing.PairingKey]*tunnelSession
	closed   bool

	readyOnce sync.Once
	readyCh   chan struct{}
	readyErr  error
}

// newCarrier 包装一条已拨通的承载连接。
func newCarrier(a *agent, conn net.Conn, addr string) *carrier {
	return &carrier{
		a:        a,
		conn:     conn,
		addr:     addr,
		sessions: make(map[pairing.PairingKey]*tunnelSession),
		readyCh:  make(chan struct{}),
	}
}

// writeFrame 串行写一个普通 PairLink 帧（承载连接首帧 MsgBind）。
func (c *carrier) writeFrame(typ protocol.MessageType, payload any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.isClosed() {
		return errCarrierClosed
	}
	return protocol.WriteFrame(c.conn, typ, payload)
}

// writeTunnelFrame 串行写一个 UDP 隧道帧。
func (c *carrier) writeTunnelFrame(session string, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.isClosed() {
		return errCarrierClosed
	}
	return protocol.WriteUDPTunnelFrame(c.conn, session, payload)
}

// isClosed 报告承载连接是否已失效。
func (c *carrier) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// addSession 登记一个会话；承载连接已关闭时返回 false。
func (c *carrier) addSession(key pairing.PairingKey, sess *localSession, localConn *net.UDPConn) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	if _, ok := c.sessions[key]; ok {
		return true
	}
	c.sessions[key] = &tunnelSession{sess: sess, conn: localConn}
	return true
}

// removeSession 删除会话登记（幂等）。
func (c *carrier) removeSession(key pairing.PairingKey) {
	c.mu.Lock()
	delete(c.sessions, key)
	c.mu.Unlock()
}

// lookup 返回会话。
func (c *carrier) lookup(key pairing.PairingKey) *tunnelSession {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessions[key]
}

// sessionCount 返回承载连接上的会话数量。
func (c *carrier) sessionCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sessions)
}

// markReady 标记承载连接就绪（只生效一次）。
func (c *carrier) markReady(err error) {
	c.readyOnce.Do(func() {
		c.readyErr = err
		close(c.readyCh)
	})
}

// waitReady 在 timeout 内等待承载连接就绪。
func (c *carrier) waitReady(timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-c.readyCh:
		return c.readyErr
	case <-timer.C:
		return fmt.Errorf("等待承载连接 MsgBindOK 超时（%s）", timeout)
	case <-c.a.ctx.Done():
		return c.a.ctx.Err()
	}
}

// isReady 报告承载连接是否已就绪且无错误。
func (c *carrier) isReady() bool {
	select {
	case <-c.readyCh:
		return c.readyErr == nil
	default:
		return false
	}
}

// fail 使承载连接失效：关闭连接、关闭其上全部会话（表项由各会话自身 goroutine 删除）。
func (c *carrier) fail(cause error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	sessions := make([]*tunnelSession, 0, len(c.sessions))
	for _, ts := range c.sessions {
		sessions = append(sessions, ts)
	}
	c.sessions = make(map[pairing.PairingKey]*tunnelSession)
	c.mu.Unlock()

	c.markReady(errCarrierClosed)
	_ = c.conn.Close()
	for _, ts := range sessions {
		ts.sess.closeAll()
	}

	args := []any{"event", "data_close", "carrier", carrierUDPOverTCP, "remote", c.addr,
		"detail", fmt.Sprintf("承载连接失效，%d 个会话全部关闭", len(sessions)), "err", cause}
	if c.a.ctx.Err() != nil {
		c.a.log.Debug("承载连接已关闭", args...)
		return
	}
	c.a.log.Warn("承载连接已断开，其上 UDP 会话全部失效", args...)
}

// readLoop 是承载连接唯一的读循环：按 session 分发数据帧，session="" 走控制帧。
//
// 读循环只做分发（写本地 UDP socket），绝不写承载连接，因此不会被写阻塞。
func (c *carrier) readLoop() {
	for {
		session, payload, err := protocol.ReadUDPTunnelFrame(c.conn)
		if err != nil {
			if c.isClosed() || c.a.ctx.Err() != nil {
				return
			}
			c.fail(fmt.Errorf("读取 UDP 隧道帧: %w", err))
			return
		}
		if session == "" {
			c.handleControlFrame(payload)
			continue
		}
		key, kerr := pairing.ParsePairingKey(session)
		if kerr != nil {
			c.a.log.Debug("丢弃非法会话标识的 UDP 隧道帧", "event", "data_close",
				"detail", "session="+session, "err", kerr)
			continue
		}
		ts := c.lookup(key)
		if ts == nil {
			c.a.log.Debug("丢弃未知会话的 UDP 隧道帧", "event", "data_close", "pairing_key", session)
			continue
		}
		ts.sess.touch()
		ts.sess.bytesIn.Add(int64(len(payload)))
		if _, werr := ts.conn.Write(payload); werr != nil {
			if ts.sess.isClosed() || c.a.ctx.Err() != nil {
				continue
			}
			c.a.log.Warn("写入本地 UDP 目标失败", "event", "data_close", "pairing_key", session, "err", werr)
		}
	}
}

// handleControlFrame 处理 session="" 的控制帧（MsgBindOK / MsgError）。
//
// 控制帧**不得关闭整条承载连接**：MsgError 只影响对应会话（协议未带配对键，
// 因此优先用 detail 里的配对键归属，无法归属时只记日志并保留全部会话）。
func (c *carrier) handleControlFrame(payload []byte) {
	typ, body, perr := protocol.ParseFrame(payload)
	if perr != nil {
		c.a.log.Warn("承载连接控制帧负载非法，已忽略", "event", "bind_fail",
			"remote", c.addr, "err", perr)
		return
	}
	switch typ {
	case protocol.MsgBindOK:
		decoded, derr := protocol.Decode(typ, body)
		if derr != nil {
			c.a.log.Warn("解析承载连接 MsgBindOK 失败", "event", "bind_fail", "remote", c.addr, "err", derr)
			return
		}
		ok, isOK := decoded.(*protocol.BindOK)
		if !isOK {
			return
		}
		c.markReady(nil)
		if ok.PairingKey != "" {
			if key, kerr := pairing.ParsePairingKey(ok.PairingKey); kerr == nil {
				if ts := c.lookup(key); ts != nil {
					ts.sess.markReady()
					ts.sess.touch()
				}
			}
		}
		c.a.log.Info("承载连接收到 MsgBindOK", "event", "bind_ok",
			"pairing_key", ok.PairingKey, "carrier", carrierUDPOverTCP, "remote", c.addr)
	case protocol.MsgError:
		code, reason, detail := decodeErrorParts(body)
		c.a.log.Warn("承载连接收到 relay 错误", "event", "bind_fail",
			"remote", c.addr, "detail", reason)
		c.failSession(code, detail)
	default:
		c.a.log.Debug("忽略承载连接上的未知控制帧", "event", "control_ignore", "detail", typ.String())
	}
}

// failSession 按 relay 错误关闭"对应"的会话，绝不关闭承载连接本身。
//
// 协议里 MsgError 没有配对键（docs/protocol.md §4.12），阶段 2 的约定是：
//   - detail 里能解析出配对键且该会话存在 → 只关闭该会话；
//   - 否则若承载连接尚未就绪（首会话报到失败）→ 唤醒等待者，让首会话失败；
//   - 否则只记日志并保留全部会话（UDP 允许丢包，最终由超时收敛）。
func (c *carrier) failSession(code, detail string) {
	if key, err := pairing.ParsePairingKey(detail); err == nil {
		c.mu.Lock()
		ts := c.sessions[key]
		delete(c.sessions, key)
		c.mu.Unlock()
		if ts != nil {
			c.a.log.Warn("relay 报告单个会话错误，仅关闭该会话", "event", "data_close",
				"pairing_key", key.String(), "detail", code)
			ts.sess.closeAll()
			return
		}
	}
	if !c.isReady() {
		c.markReady(errRelayRejected{reason: code})
		return
	}
	c.a.log.Debug("relay 错误无法归属到具体会话，保留全部会话", "event", "bind_fail",
		"remote", c.addr, "detail", code)
}

// decodeErrorParts 从 MsgError 负载解析 code、可读文本与 detail 字段。
func decodeErrorParts(payload []byte) (code, text, detail string) {
	decoded, err := protocol.Decode(protocol.MsgError, payload)
	if err != nil {
		return "", "无法解析的错误响应", ""
	}
	e, ok := decoded.(*protocol.Error)
	if !ok || e == nil {
		return "", "错误响应类型异常", ""
	}
	text = e.Code
	if e.Message != "" {
		text = e.Code + ": " + e.Message
	}
	return e.Code, text, e.Detail
}

// handleUDPTunnel 处理 udp_mode=tcp_tunnel 的配对：把会话挂到承载连接上，
// 然后在本地 UDP 目标与承载连接之间双向搬运数据报。
func (a *agent) handleUDPTunnel(sess *localSession, key pairing.PairingKey, svc config.ServerService, token string) {
	defer a.endSession(sess)

	localConn, err := a.dialUDP(svc.Addr)
	if err != nil {
		a.log.Warn("连接本地 UDP 目标失败，仅关闭本次配对", "event", "upstream_dial_failed",
			"pairing_key", key.String(), "service_id", svc.ID, "detail", "local="+svc.Addr, "err", err)
		return
	}
	if !sess.track(localConn) {
		return
	}
	sess.setCarrier(carrierUDPOverTCP)

	c, caddr, err := a.carrier.openSession(sess, key, token, localConn)
	if err != nil {
		if sess.isClosed() || a.ctx.Err() != nil {
			return
		}
		a.log.Warn("建立 UDP over TCP 会话失败，放弃本次配对", "event", "data_open",
			"pairing_key", key.String(), "carrier", carrierUDPOverTCP,
			"remote", a.tcpDataAddr, "err", err)
		return
	}
	sess.setRemote(caddr)
	sess.markReady()
	sess.touch()
	a.log.Info("UDP over TCP 会话已建立", "event", "data_open",
		"pairing_key", key.String(),
		"service_id", svc.ID,
		"proto", "udp",
		"carrier", carrierUDPOverTCP,
		"remote", caddr,
		"detail", "local="+svc.Addr,
	)

	a.tunnelLocalToRelay(c, sess, key, localConn)
}

// tunnelLocalToRelay 读本地 UDP 目标的回包并写回承载连接；返回时注销该会话。
func (a *agent) tunnelLocalToRelay(c *carrier, sess *localSession, key pairing.PairingKey, localConn *net.UDPConn) {
	defer c.removeSession(key)
	buf := make([]byte, udpDatagramSize)
	for {
		n, err := localConn.Read(buf)
		if err != nil {
			if sess.isClosed() || a.ctx.Err() != nil || isClosedErr(err) {
				return
			}
			a.log.Warn("从本地 UDP 目标读取失败，结束本次会话", "event", "data_close",
				"pairing_key", key.String(), "err", err)
			return
		}
		sess.touch()
		sess.bytesOut.Add(int64(n))
		if werr := c.writeTunnelFrame(key.String(), buf[:n]); werr != nil {
			if sess.isClosed() || a.ctx.Err() != nil || errors.Is(werr, errCarrierClosed) {
				return
			}
			a.log.Warn("写 UDP 隧道帧失败，结束本次会话", "event", "data_close",
				"pairing_key", key.String(), "err", werr)
			return
		}
	}
}
