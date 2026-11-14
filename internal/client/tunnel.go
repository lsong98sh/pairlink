package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lsong98sh/pairlink/internal/pairing"
	"github.com/lsong98sh/pairlink/internal/protocol"
	"github.com/lsong98sh/pairlink/internal/tunnelqueue"
)

// udpTunnelCarrier 是一条到 relay tcp_listen 的 UDP over TCP 承载连接
// （udp_mode = tcp_tunnel）。
//
// 一条承载连接上复用多个 UDP 会话：
//   - 首帧是普通 PairLink 帧 MsgBind{carrier:"udp-over-tcp"}，对应「第一个会话」；
//   - 之后新增会话用隧道帧 session=<配对键> + 完整 MsgBind 帧；
//   - 数据帧 session=<配对键> + 数据报原始字节；
//   - 控制帧 session="" 的 payload 是完整 PairLink 帧（MsgBindOK / MsgError）。
//
// 写侧由单 writer goroutine + 有界队列承担（tunnelqueue）：业务 goroutine 只入队、
// 绝不阻塞；读循环只有一条 goroutine（ReadUDPTunnelFrame）。
type udpTunnelCarrier struct {
	owner *client
	conn  net.Conn

	// out 是发送队列（start 之后非 nil）。
	out atomic.Pointer[tunnelqueue.Queue]

	mu       sync.Mutex
	sessions map[string]*udpSession
	closed   bool

	closedCh chan struct{}
	ready    chan struct{}

	readyOnce sync.Once
	readyErr  error
}

// newUDPTunnelCarrier 构造承载连接（尚未开始读循环与发送队列）。
func newUDPTunnelCarrier(owner *client, conn net.Conn) *udpTunnelCarrier {
	return &udpTunnelCarrier{
		owner:    owner,
		conn:     conn,
		sessions: make(map[string]*udpSession),
		closedCh: make(chan struct{}),
		ready:    make(chan struct{}),
	}
}

// start 启动发送队列与读循环。
func (cc *udpTunnelCarrier) start() {
	q := tunnelqueue.New(cc.conn, tunnelqueue.Options{
		OnDrop: func(kind string, dropped int64, queued int) {
			cc.owner.log.Warn("UDP 隧道发送队列已满，按策略丢弃", "event", "tunnel_drop",
				"remote", cc.conn.RemoteAddr().String(),
				"detail", fmt.Sprintf("kind=%s dropped=%d queued=%d", kind, dropped, queued))
		},
	})
	cc.out.Store(q)
	cc.owner.tasks.Go(cc.readLoop)
}

// isClosed 报告承载连接是否已关闭。
func (cc *udpTunnelCarrier) isClosed() bool {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.closed
}

// register 登记一个会话（会话已关闭则返回错误）。
func (cc *udpTunnelCarrier) register(s *udpSession) error {
	cc.mu.Lock()
	if cc.closed {
		cc.mu.Unlock()
		return errCarrierClosed
	}
	cc.sessions[s.displayKey()] = s
	cc.mu.Unlock()
	if !s.setCarrier(cc) {
		cc.unregister(s.displayKey())
		return errors.New("client: 会话已关闭")
	}
	return nil
}

// unregister 移除一个会话（不存在时无副作用）。
func (cc *udpTunnelCarrier) unregister(key string) {
	if key == "" {
		return
	}
	cc.mu.Lock()
	delete(cc.sessions, key)
	cc.mu.Unlock()
}

// lookup 按配对键找会话。
func (cc *udpTunnelCarrier) lookup(key string) *udpSession {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.sessions[key]
}

// markReady 记录承载连接就绪（首个 MsgBindOK 到达）并唤醒等待者。
func (cc *udpTunnelCarrier) markReady(err error) {
	cc.mu.Lock()
	if cc.readyErr == nil {
		cc.readyErr = err
	}
	cc.mu.Unlock()
	cc.readyOnce.Do(func() { close(cc.ready) })
}

// waitReady 等待承载连接就绪（首个会话的 BindOK），超时或关闭返回错误。
func (cc *udpTunnelCarrier) waitReady(ctx context.Context, timeout time.Duration) error {
	if cc.isClosed() {
		return errCarrierClosed
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-cc.ready:
		cc.mu.Lock()
		err := cc.readyErr
		cc.mu.Unlock()
		return err
	case <-cc.closedCh:
		return errCarrierClosed
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("client: 等待 UDP 承载连接 BindOK 超时（%s）", timeout)
	}
}

// writeFrame 把一帧隧道数据放入发送队列；返回是否成功入队。
//
// 非阻塞：队列满时按丢弃策略处理（计数 + 限速 WARN）。
func (cc *udpTunnelCarrier) writeFrame(session string, payload []byte) bool {
	q := cc.out.Load()
	if q == nil {
		return false
	}
	return q.SendData(session, payload)
}

// sendSessionBind 为一个复用会话发送 MsgBind 隧道帧（承载连接已就绪时使用）。
func (cc *udpTunnelCarrier) sendSessionBind(key, token string) error {
	frame, err := protocol.EncodeFrame(protocol.MsgBind, &protocol.Bind{
		PairingKey:   key,
		PairingToken: token,
		Role:         protocol.RoleClient,
		Carrier:      protocol.CarrierUDPOverTCP,
	})
	if err != nil {
		return err
	}
	if !cc.writeFrame(key, frame) {
		if cc.isClosed() {
			return errCarrierClosed
		}
		return errors.New("client: UDP 承载连接发送队列已满，会话登记被丢弃")
	}
	return nil
}

// attachSession 在既有承载连接上登记并报到一个新会话。
func (cc *udpTunnelCarrier) attachSession(s *udpSession, token string) error {
	key := s.displayKey()
	if err := cc.register(s); err != nil {
		return err
	}
	if err := cc.sendSessionBind(key, token); err != nil {
		cc.unregister(key)
		return err
	}
	cc.owner.log.Info("已发送 UDP 隧道会话报到帧（复用承载连接）", "event", "bind",
		"service_id", s.svcID, "pairing_key", key, "detail", "carrier=udp-over-tcp")
	return nil
}

// readLoop 是承载连接唯一的读循环：按 session 分发数据帧，单独处理控制帧。
func (cc *udpTunnelCarrier) readLoop() {
	for {
		session, payload, err := protocol.ReadUDPTunnelFrame(cc.conn)
		if err != nil {
			if cc.owner.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				cc.close("shutdown", nil)
				return
			}
			cc.owner.log.Warn("UDP over TCP 承载连接读取失败，关闭其全部会话", "event", "data_close",
				"detail", "carrier_read_error", "err", err)
			cc.close("carrier_read_error", err)
			return
		}
		if session == "" {
			cc.handleControlFrame(payload)
			continue
		}
		s := cc.lookup(session)
		if s == nil {
			cc.owner.log.Debug("丢弃未知会话的 UDP 隧道数据帧", "event", "udp_frame_drop",
				"pairing_key", session)
			continue
		}
		cc.owner.deliverUDPToLocal(s, payload)
	}
}

// handleControlFrame 处理 session="" 的控制帧（MsgBindOK / MsgError）。
func (cc *udpTunnelCarrier) handleControlFrame(payload []byte) {
	typ, body, err := protocol.ParseFrame(payload)
	if err != nil {
		cc.owner.log.Warn("控制帧不是合法的 PairLink 帧，忽略", "event", "udp_frame_drop",
			"detail", "carrier_control_frame_invalid", "err", err)
		return
	}
	msg, err := protocol.Decode(typ, body)
	if err != nil {
		cc.owner.log.Warn("控制帧无法解析，忽略", "event", "udp_frame_drop", "err", err)
		return
	}
	switch m := msg.(type) {
	case *protocol.BindOK:
		cc.owner.log.Debug("UDP 承载连接已就绪", "event", "bind_ok", "pairing_key", m.PairingKey)
		cc.markReady(nil)
	case *protocol.Error:
		cc.handleSessionError(m)
	default:
		cc.owner.log.Debug("忽略非预期承载控制帧", "event", "udp_frame_drop", "detail", typ.String())
	}
}

// handleSessionError 处理承载连接上的单会话错误：只关闭对应会话，不关承载连接。
//
// protocol.Error 没有 pairing_key 字段，阶段 2 三模式统一约定：控制帧
// MsgError 的 Detail 就是配对键。解析成功 → 只关闭该会话；解析失败 → 仅 WARN，
// 不关闭任何会话（避免误伤同一条承载连接上的其它会话）。
func (cc *udpTunnelCarrier) handleSessionError(e *protocol.Error) {
	key, err := pairing.ParsePairingKey(strings.TrimSpace(e.Detail))
	if err != nil {
		cc.owner.log.Warn("承载连接收到会话级错误，但 Detail 不是可用配对键，忽略（不关闭任何会话）",
			"event", "data_close", "detail", "carrier_session_error", "err", e)
		return
	}
	target := cc.lookup(key.String())
	if target == nil {
		cc.owner.log.Warn("承载连接收到未知会话的错误帧，忽略", "event", "data_close",
			"pairing_key", key.String(), "err", e)
		return
	}
	cc.owner.log.Warn("relay 拒绝了该 UDP 会话，仅关闭本会话", "event", "data_close",
		"pairing_key", key.String(), "err", e)
	cc.owner.closeUDPSession(target, "relay_error", e)
}

// close 关闭承载连接并清理其上全部会话（幂等）。
func (cc *udpTunnelCarrier) close(reason string, cause error) {
	cc.mu.Lock()
	if cc.closed {
		cc.mu.Unlock()
		return
	}
	cc.closed = true
	close(cc.closedCh)
	if cc.readyErr == nil {
		cc.readyErr = errCarrierClosed
	}
	sessions := make([]*udpSession, 0, len(cc.sessions))
	for _, s := range cc.sessions {
		sessions = append(sessions, s)
	}
	cc.sessions = make(map[string]*udpSession)
	cc.mu.Unlock()

	cc.readyOnce.Do(func() { close(cc.ready) })
	if q := cc.out.Load(); q != nil {
		_ = q.Close()
	} else {
		_ = cc.conn.Close()
	}
	cc.owner.dropCarrier(cc)

	if len(sessions) > 0 {
		cc.owner.log.Info("UDP over TCP 承载连接已关闭，其上会话全部失效", "event", "data_close",
			"detail", fmt.Sprintf("reason=%s sessions=%d", reason, len(sessions)))
	}
	for _, s := range sessions {
		cc.owner.closeUDPSession(s, reason, cause)
	}
}

// dropCarrier 在指针仍是该承载连接时清空 client 上的引用。
func (c *client) dropCarrier(cc *udpTunnelCarrier) {
	c.carrierMu.Lock()
	if c.carrier == cc {
		c.carrier = nil
	}
	c.carrierMu.Unlock()
}

// attachUDPTunnel 走 UDP over TCP 数据面：复用（或懒创建）一条承载连接。
func (c *client) attachUDPTunnel(s *udpSession, key pairing.PairingKey, token string) error {
	carrier, first, err := c.acquireCarrier(s, token)
	if err != nil {
		return err
	}
	// 首会话需要等 relay 的 MsgBindOK 确认承载连接就绪；后续会话直接复用。
	if err := carrier.waitReady(c.ctx, c.pairTimeout()); err != nil {
		if first {
			carrier.close("carrier_bind_failed", err)
		}
		return err
	}
	return nil
}

// acquireCarrier 返回可用的承载连接：存在则登记并报到新会话，否则新建。
//
// 返回的 first 表示本会话是否作为新承载连接的首帧会话（新建时唯一为 true）。
func (c *client) acquireCarrier(s *udpSession, token string) (*udpTunnelCarrier, bool, error) {
	key := s.displayKey()

	// 快路径：已有可用承载连接。
	c.carrierMu.Lock()
	existing := c.carrier
	c.carrierMu.Unlock()
	if existing != nil && !existing.isClosed() {
		if err := existing.attachSession(s, token); err != nil {
			return nil, false, err
		}
		return existing, false, nil
	}

	// 串行化创建，避免并发会话各建一条承载连接。
	c.carrierDialMu.Lock()
	defer c.carrierDialMu.Unlock()

	c.carrierMu.Lock()
	existing = c.carrier
	c.carrierMu.Unlock()
	if existing != nil && !existing.isClosed() {
		if err := existing.attachSession(s, token); err != nil {
			return nil, false, err
		}
		return existing, false, nil
	}

	conn, err := c.dialTCPData()
	if err != nil {
		return nil, false, err
	}
	carrier := newUDPTunnelCarrier(c, conn)
	if err := carrier.register(s); err != nil {
		carrier.close("register_failed", err)
		return nil, false, err
	}
	// 首帧是普通 PairLink 帧（不是隧道帧），用于把连接切换到帧解析模式。
	bind := &protocol.Bind{
		PairingKey:   key,
		PairingToken: token,
		Role:         protocol.RoleClient,
		Carrier:      protocol.CarrierUDPOverTCP,
	}
	if d := c.dialTimeout(); d > 0 {
		_ = conn.SetWriteDeadline(time.Now().Add(d))
	}
	if err := protocol.WriteFrame(conn, protocol.MsgBind, bind); err != nil {
		carrier.close("first_frame_failed", err)
		return nil, false, fmt.Errorf("client: 发送 UDP 承载首帧: %w", err)
	}
	_ = conn.SetWriteDeadline(time.Time{})
	c.log.Info("已发送 UDP 承载连接首帧", "event", "bind",
		"service_id", s.svcID, "pairing_key", key,
		"remote", conn.RemoteAddr().String(), "detail", "carrier=udp-over-tcp")

	// 先启动发送队列/读循环，再发布指针：并发会话通过 c.carrier 拿到它时，
	// 队列必然已就绪（不会因为 out 为 nil 导致登记帧被丢弃）。
	carrier.start()
	c.carrierMu.Lock()
	c.carrier = carrier
	c.carrierMu.Unlock()
	return carrier, true, nil
}
