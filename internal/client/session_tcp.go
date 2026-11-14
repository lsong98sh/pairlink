package client

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lsong98sh/pairlink/internal/forward"
	"github.com/lsong98sh/pairlink/internal/pairing"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// sessionState 是本地连接/会话的配对状态。
type sessionState int

const (
	// stateWaitingPair：已发 ConnectRequest，等待 MsgPairResult。
	stateWaitingPair sessionState = iota
	// stateBinding：已收到 PairResult，正在建立数据面（等待 BindOK / 拨号）。
	stateBinding
	// stateForwarding：数据面已就绪，正在双向转发。
	stateForwarding
	// stateClosed：已清理。
	stateClosed
)

// statusText 返回管理页展示用的中文状态。
func (s sessionState) statusText() string {
	switch s {
	case stateWaitingPair, stateBinding:
		return "等待配对"
	case stateForwarding:
		return "转发中"
	default:
		return "已关闭"
	}
}

// tcpSession 表示一个本地 TCP 接入连接及其配对状态。
type tcpSession struct {
	svcID     string
	connID    string
	remote    string
	local     net.Conn
	startedAt time.Time

	mu    sync.Mutex
	state sessionState
	key   string // 配对键（收到 PairResult 后填）
	proto string

	// toRelay 是本地→relay 的字节数，toLocal 是 relay→本地 的字节数。
	toRelay atomic.Int64
	toLocal atomic.Int64
}

// displayKey 返回展示用配对键：未收到 PairResult 时按 (服务ID, connID) 推导。
func (s *tcpSession) displayKey() string {
	s.mu.Lock()
	key := s.key
	s.mu.Unlock()
	if key != "" {
		return key
	}
	return pairing.PairingKey{ServiceID: s.svcID, ClientConnID: s.connID}.String()
}

// serviceID 实现 localSession 接口。
func (s *tcpSession) serviceID() string { return s.svcID }

// isWaiting 报告该连接是否还在等待/建立配对（用于 MsgError 的近似匹配）。
func (s *tcpSession) isWaiting() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state == stateWaitingPair || s.state == stateBinding
}

// isForwarding 报告是否已在转发。
func (s *tcpSession) isForwarding() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state == stateForwarding
}

// pairExpired 报告「等待 PairResult」是否已超过 pair_timeout。
func (s *tcpSession) pairExpired(now time.Time, timeout time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state == stateWaitingPair && now.Sub(s.startedAt) > timeout
}

// resolve 记录配对结果并进入绑定阶段；已结束的连接返回 false。
func (s *tcpSession) resolve(key pairing.PairingKey, proto string) bool {
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

// setForwarding 把绑定阶段推进到转发阶段；已结束的连接返回 false。
func (s *tcpSession) setForwarding() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == stateClosed {
		return false
	}
	s.state = stateForwarding
	return true
}

// snapshot 返回管理页需要的字段快照。
func (s *tcpSession) snapshot() (key, proto string, state sessionState, started time.Time, remote string, toRelay, toLocal int64) {
	s.mu.Lock()
	key, proto, state = s.key, s.proto, s.state
	s.mu.Unlock()
	if key == "" {
		key = pairing.PairingKey{ServiceID: s.svcID, ClientConnID: s.connID}.String()
	}
	return key, proto, state, s.startedAt, s.remote, s.toRelay.Load(), s.toLocal.Load()
}

// tcpSession 返回指定 client_conn_id 的本地 TCP 连接（没有返回 nil）。
func (c *client) tcpSession(connID string) *tcpSession {
	c.tcpMu.Lock()
	defer c.tcpMu.Unlock()
	return c.tcpSessions[connID]
}

// closeTCPSession 关闭并移除一个本地 TCP 连接（幂等）。
//
// 清理顺序遵循 docs/architecture.md §5：先关连接，再删表项。
func (c *client) closeTCPSession(s *tcpSession, reason string, cause error) {
	s.mu.Lock()
	if s.state == stateClosed {
		s.mu.Unlock()
		return
	}
	s.state = stateClosed
	proto := s.proto
	s.mu.Unlock()

	_ = s.local.Close()

	c.tcpMu.Lock()
	delete(c.tcpSessions, s.connID)
	c.tcpMu.Unlock()

	key := s.displayKey()
	attrs := []any{
		"event", "data_close",
		"service_id", s.svcID,
		"pairing_key", key,
		"remote", s.remote,
		"detail", fmt.Sprintf("reason=%s proto=%s to_relay=%d to_local=%d",
			reason, proto, s.toRelay.Load(), s.toLocal.Load()),
	}
	if cause != nil {
		attrs = append(attrs, "err", cause)
	}
	c.log.Info("本地 TCP 连接已清理", attrs...)
}

// forwardTCP 建立 relay TCP 数据面连接、发送 MsgBind（不等应答）并双向转发。
func (c *client) forwardTCP(s *tcpSession, key pairing.PairingKey, token string) {
	defer c.closeTCPSession(s, "forward_done", nil)

	dataConn, err := c.dialTCPData()
	if err != nil {
		c.log.Warn("连接 relay TCP 数据面失败，关闭本地连接", "event", "data_open",
			"service_id", key.ServiceID, "pairing_key", key.String(), "err", err)
		return
	}

	bind := &protocol.Bind{
		PairingKey:   key.String(),
		PairingToken: token,
		Role:         protocol.RoleClient,
		Carrier:      protocol.CarrierTCP,
	}
	if d := c.dialTimeout(); d > 0 {
		_ = dataConn.SetWriteDeadline(time.Now().Add(d))
	}
	if err := protocol.WriteFrame(dataConn, protocol.MsgBind, bind); err != nil {
		_ = dataConn.Close()
		c.log.Warn("发送数据面绑定帧失败，关闭本地连接", "event", "bind",
			"service_id", key.ServiceID, "pairing_key", key.String(), "err", err)
		return
	}
	// TCP 数据面报到后 relay 不回任何字节，之后是纯透传；清除 deadline。
	_ = dataConn.SetWriteDeadline(time.Time{})
	_ = dataConn.SetReadDeadline(time.Time{})

	c.log.Info("已发送 TCP 数据面绑定帧（不等应答）", "event", "bind",
		"service_id", key.ServiceID, "pairing_key", key.String(),
		"remote", dataConn.RemoteAddr().String(), "detail", "carrier=tcp")

	if !s.setForwarding() {
		_ = dataConn.Close()
		return
	}

	c.log.Info("数据面已打开，开始双向转发", "event", "data_open",
		"service_id", key.ServiceID, "pairing_key", key.String(),
		"remote", dataConn.RemoteAddr().String(), "detail", "proto=tcp carrier=tcp")

	counted := &countingConn{Conn: s.local, toRelay: &s.toRelay, toLocal: &s.toLocal}
	forward.Bidirectional(c.ctx, counted, dataConn)

	c.log.Info("数据面转发结束", "event", "data_close",
		"service_id", key.ServiceID, "pairing_key", key.String(),
		"detail", fmt.Sprintf("to_relay=%d to_local=%d", s.toRelay.Load(), s.toLocal.Load()))
}

// countingConn 统计本地连接两个方向的字节数，供管理页实时展示。
//
// 保留 CloseWrite，让 forward.Bidirectional 的半关闭语义不受影响。
type countingConn struct {
	net.Conn
	toRelay *atomic.Int64
	toLocal *atomic.Int64
}

// Read 统计「本地→relay」方向。
func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 && c.toRelay != nil {
		c.toRelay.Add(int64(n))
	}
	return n, err
}

// Write 统计「relay→本地」方向。
func (c *countingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 && c.toLocal != nil {
		c.toLocal.Add(int64(n))
	}
	return n, err
}

// CloseWrite 透传半关闭能力。
func (c *countingConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Conn.Close()
}
