package relay

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lsong98sh/pairlink/internal/logx"
	"github.com/lsong98sh/pairlink/internal/pairing"
	"github.com/lsong98sh/pairlink/internal/protocol"
	"github.com/lsong98sh/pairlink/internal/tunnelqueue"
)

// errCarrierSideTaken 表示该配对键的某一侧已经由其它承载连接登记。
var errCarrierSideTaken = errors.New("relay: 该会话侧已由其它承载连接登记")

// errCarrierQueueClosed 表示承载连接的发送队列已关闭（连接正在收尾）。
var errCarrierQueueClosed = errors.New("relay: 承载连接发送队列已关闭")

// carrierTable 是 UDP over TCP 的会话表（冻结包不提供，由 relay 维护）。
//
// 会话以配对键为索引，每条承载 TCP 连接可以复用多个会话；写对端承载连接
// 必须串行（carrierConn.writeMu），避免隧道帧交错。
type carrierTable struct {
	mu       sync.Mutex
	sessions map[pairing.PairingKey]*carrierSession
}

// carrierSession 是一条 UDP over TCP 会话。
type carrierSession struct {
	key       pairing.PairingKey
	agent     *carrierConn
	client    *carrierConn
	full      bool
	agentID   string
	createdAt time.Time
	lastSeen  time.Time
}

// carrierEntry 是管理页展示用的会话快照。
type carrierEntry struct {
	Key          pairing.PairingKey
	AgentID      string
	AgentRemote  string
	ClientRemote string
	Full         bool
	CreatedAt    time.Time
	LastSeen     time.Time
}

// carrierConn 是一条 UDP over TCP 承载 TCP 连接。
//
// 写侧是单 writer goroutine + 有界队列（tunnelqueue）：转发路径（对端读循环）
// 只入队、绝不因慢对端阻塞；控制帧走优先队列，不会被数据洪泛饿死。
type carrierConn struct {
	srv    *server
	conn   net.Conn
	log    *slog.Logger
	role   string
	remote string

	out       atomic.Pointer[tunnelqueue.Queue]
	closeOnce sync.Once
	closed    chan struct{}
}

// newCarrierTable 创建空承载会话表。
func newCarrierTable() *carrierTable {
	return &carrierTable{sessions: make(map[pairing.PairingKey]*carrierSession)}
}

// handleCarrierBind 处理承载连接的首帧（普通 PairLink 帧形式的 MsgBind）。
//
// 首帧同时是承载连接的鉴权凭据：校验失败直接关闭连接，不写任何字节；
// 成功后回一个 session="" 的控制帧（payload 为 MsgBindOK），随后进入
// UDP 隧道帧解析循环。
func (s *server) handleCarrierBind(conn net.Conn, bind *protocol.Bind, log *slog.Logger) {
	side, ok := sideFromRole(bind.Role)
	if !ok {
		log.Warn("承载连接首帧角色非法", "event", "bind_fail", "role", bind.Role)
		return
	}
	key, err := pairing.ParsePairingKey(bind.PairingKey)
	if err != nil {
		log.Warn("承载连接首帧配对键非法", "event", "bind_fail", "err", err)
		return
	}

	c := &carrierConn{
		srv:    s,
		conn:   conn,
		log:    logx.Component(s.log, "carrier").With("remote", conn.RemoteAddr().String()),
		role:   bind.Role,
		remote: conn.RemoteAddr().String(),
		closed: make(chan struct{}),
	}
	// 先建发送队列再登记：登记后对端读循环可能立刻向本连接写帧，
	// out 必须在此前就绪（且 out 本身是 atomic.Pointer，读写无竞争）。
	c.startWriter()
	if _, err := s.carriers.register(c, key, side, bind.PairingToken); err != nil {
		c.log.Warn("承载连接首帧鉴权失败",
			"event", "bind_fail",
			"pairing_key", key.String(),
			"role", bind.Role,
			"err", err,
		)
		c.close()
		return
	}

	bindOK, err := protocol.EncodeFrame(protocol.MsgBindOK, &protocol.BindOK{PairingKey: key.String()})
	if err != nil {
		c.log.Warn("编码 MsgBindOK 失败", "event", "internal", "err", err)
		c.close()
		return
	}
	if !c.writeFrame("", bindOK) {
		c.log.Warn("写承载连接就绪帧失败", "event", "bind_fail", "err", errCarrierQueueClosed)
		c.close()
		return
	}

	_ = conn.SetDeadline(time.Time{})
	c.log.Info("承载连接就绪",
		"event", "bind_ok",
		"carrier", protocol.CarrierUDPOverTCP,
		"pairing_key", key.String(),
		"role", bind.Role,
		"service_id", key.ServiceID,
	)
	c.readLoop()
}

// readLoop 是承载连接的唯一读循环：按 session 分发控制帧/会话登记帧/数据帧。
func (c *carrierConn) readLoop() {
	defer c.close()
	for {
		session, payload, err := protocol.ReadUDPTunnelFrame(c.conn)
		if err != nil {
			if c.srv.ctx.Err() == nil && !c.isClosed() {
				c.log.Warn("承载连接读取失败", "event", "data_close", "err", err)
			}
			return
		}
		if session == "" {
			c.handleControlPayload(payload)
			continue
		}
		key, err := pairing.ParsePairingKey(session)
		if err != nil {
			c.log.Debug("丢弃非法会话标识", "event", "udp_evict", "session", session)
			continue
		}
		c.srv.handleCarrierFrame(c, key, payload)
	}
}

// handleControlPayload 处理承载连接上的控制帧（session=""）。
func (c *carrierConn) handleControlPayload(payload []byte) {
	typ, raw, err := protocol.ParseFrame(payload)
	if err != nil {
		c.log.Debug("承载控制帧结构非法", "event", "udp_evict", "err", err)
		return
	}
	switch typ {
	case protocol.MsgError:
		msg, decErr := protocol.Decode(typ, raw)
		if decErr != nil {
			return
		}
		e, _ := msg.(*protocol.Error)
		if e != nil {
			c.log.Warn("对端承载返回错误", "event", "peer_error", "code", e.Code, "detail", e.Message)
		}
	default:
		c.log.Debug("忽略承载控制帧", "event", "message_ignored", "type", typ.String())
	}
}

// handleCarrierFrame 处理一个 session != "" 的隧道帧。
//
// 已登记在本连接上的会话按数据帧转发；否则尝试把 payload 解析为完整的
// MsgBind 帧完成会话登记。单会话错误用控制帧回报，不关闭承载连接。
func (s *server) handleCarrierFrame(c *carrierConn, key pairing.PairingKey, payload []byte) {
	if s.carriers.owns(c, key) {
		s.deliverCarrierData(c, key, payload)
		return
	}

	typ, raw, err := protocol.ParseFrame(payload)
	if err != nil || typ != protocol.MsgBind {
		// 未登记会话的数据帧：UDP 允许丢包，直接丢弃。
		c.log.Debug("丢弃未知承载会话的数据帧",
			"event", "udp_evict",
			"pairing_key", key.String(),
			"err", err,
		)
		return
	}
	msg, err := protocol.Decode(typ, raw)
	if err != nil {
		c.writeControlError(protocol.ErrCodeBadRequest, "会话登记帧结构非法", key.String())
		return
	}
	bind, ok := msg.(*protocol.Bind)
	if !ok {
		c.writeControlError(protocol.ErrCodeBadRequest, "会话登记帧结构非法", key.String())
		return
	}
	if bind.Role != c.role {
		c.writeControlError(protocol.ErrCodeBadRequest, "承载连接上的会话角色不一致", key.String())
		return
	}
	if bind.PairingKey != "" && bind.PairingKey != key.String() {
		c.writeControlError(protocol.ErrCodeBadRequest, "会话标识与 MsgBind 不一致", key.String())
		return
	}
	side, ok := sideFromRole(bind.Role)
	if !ok {
		c.writeControlError(protocol.ErrCodeBadRequest, "会话角色非法", key.String())
		return
	}

	full, err := s.carriers.register(c, key, side, bind.PairingToken)
	if err != nil {
		code := protocol.ErrCodeInvalidToken
		if errors.Is(err, errCarrierSideTaken) {
			code = protocol.ErrCodeAlreadyPaired
		}
		c.log.Warn("承载会话登记失败",
			"event", "bind_fail",
			"pairing_key", key.String(),
			"role", bind.Role,
			"err", err,
		)
		c.writeControlError(code, "承载会话登记失败", key.String())
		return
	}
	c.log.Info("承载会话登记",
		"event", "udp_session_open",
		"pairing_key", key.String(),
		"role", bind.Role,
		"full", full,
		"service_id", key.ServiceID,
	)
}

// deliverCarrierData 把数据帧转发给同会话的对端承载连接。
func (s *server) deliverCarrierData(from *carrierConn, key pairing.PairingKey, payload []byte) {
	t := s.carriers
	t.mu.Lock()
	sess := t.sessions[key]
	if sess == nil || !sess.full {
		t.mu.Unlock()
		return
	}
	var target *carrierConn
	switch from {
	case sess.agent:
		target = sess.client
	case sess.client:
		target = sess.agent
	default:
		t.mu.Unlock()
		return
	}
	sess.lastSeen = time.Now()
	t.mu.Unlock()

	if target == nil || target == from {
		return
	}
	if !target.writeFrame(key.String(), payload) {
		// 队列满导致丢弃（慢对端）：计数 + 限速 WARN 已由队列回调完成，
		// 读循环绝不因对端慢而阻塞，也不因此关闭承载连接。
		if target.isClosed() {
			return
		}
		target.log.Debug("对端承载连接发送队列已满，丢弃数据帧",
			"event", "tunnel_drop", "pairing_key", key.String())
	}
}

// startWriter 为该承载连接创建发送队列（单 writer goroutine）。
func (c *carrierConn) startWriter() {
	c.out.Store(tunnelqueue.New(c.conn, tunnelqueue.Options{
		OnDrop: func(kind string, dropped int64, queued int) {
			c.log.Warn("承载连接发送队列已满，按策略丢弃", "event", "tunnel_drop",
				"detail", fmt.Sprintf("kind=%s dropped=%d queued=%d", kind, dropped, queued))
		},
	}))
}

// writeFrame 把一帧放入发送队列；返回是否成功入队（非阻塞）。
func (c *carrierConn) writeFrame(session string, payload []byte) bool {
	q := c.out.Load()
	if q == nil {
		return false
	}
	if session == "" {
		return q.SendControl(payload)
	}
	return q.SendData(session, payload)
}

// writeControlError 用 session="" + MsgError 控制帧报告单会话错误。
//
// Detail 只放配对键字符串：peer 据此只关闭对应会话，不关闭整条承载连接。
func (c *carrierConn) writeControlError(code, message, pairingKey string) {
	frame, err := protocol.EncodeFrame(protocol.MsgError, &protocol.Error{
		Code:    code,
		Message: message,
		Detail:  pairingKey,
	})
	if err != nil {
		return
	}
	if !c.writeFrame("", frame) {
		c.log.Warn("写承载控制帧失败", "event", "data_close", "detail", "队列已关闭或已满")
	}
}

// close 关闭承载连接并删除其上的全部会话（幂等）。
//
// 关闭顺序：停止入队 → 关闭发送队列（writer 排空后退出并关闭连接）→ 删除会话表项。
func (c *carrierConn) close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		if q := c.out.Load(); q != nil {
			_ = q.Close()
		} else {
			_ = c.conn.Close()
		}
		for _, key := range c.srv.carriers.removeCarrier(c) {
			c.log.Info("承载连接断开，移除会话",
				"event", "udp_session_close",
				"pairing_key", key.String(),
				"service_id", key.ServiceID,
			)
		}
	})
}

// isClosed 报告连接是否已关闭。
func (c *carrierConn) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

// register 登记一侧会话；返回 full 表示两侧都已登记（会话转为 Full）。
//
// 幂等：同一承载连接重复登记同一侧不改变状态、不重复校验令牌。
func (t *carrierTable) register(c *carrierConn, key pairing.PairingKey, side pairing.Side, pairingToken string) (full bool, err error) {
	t.mu.Lock()

	sess, ok := t.sessions[key]
	created := false
	if !ok {
		now := time.Now()
		sess = &carrierSession{key: key, createdAt: now, lastSeen: now}
		t.sessions[key] = sess
		created = true
	}

	if side == pairing.SideAgent {
		if sess.agent == c {
			full := sess.full
			t.mu.Unlock()
			return full, nil
		}
		if sess.agent != nil {
			t.mu.Unlock()
			return false, errCarrierSideTaken
		}
	} else {
		if sess.client == c {
			full := sess.full
			t.mu.Unlock()
			return full, nil
		}
		if sess.client != nil {
			t.mu.Unlock()
			return false, errCarrierSideTaken
		}
	}

	if err := c.srv.tokens.Validate(key, pairingToken); err != nil {
		if created {
			delete(t.sessions, key)
		}
		t.mu.Unlock()
		return false, err
	}

	now := time.Now()
	if side == pairing.SideAgent {
		sess.agent = c
		sess.agentID = c.srv.agentIDForService(key.ServiceID)
	} else {
		sess.client = c
	}
	sess.lastSeen = now
	sess.full = sess.agent != nil && sess.client != nil
	full = sess.full
	t.mu.Unlock()

	if full {
		c.srv.tokens.Consume(key)
	}
	return full, nil
}

// owns 报告某承载连接是否已登记该会话的某一侧。
func (t *carrierTable) owns(c *carrierConn, key pairing.PairingKey) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	sess, ok := t.sessions[key]
	if !ok {
		return false
	}
	return sess.agent == c || sess.client == c
}

// carriersFor 返回会话两侧的承载连接（供管理页踢出使用）。
func (t *carrierTable) carriersFor(key pairing.PairingKey) (agent, client *carrierConn, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	sess, found := t.sessions[key]
	if !found {
		return nil, nil, false
	}
	return sess.agent, sess.client, true
}

// removeCarrier 删除某承载连接上的全部会话，返回被删除的配对键。
func (t *carrierTable) removeCarrier(c *carrierConn) []pairing.PairingKey {
	t.mu.Lock()
	removed := make([]pairing.PairingKey, 0, 4)
	for key, sess := range t.sessions {
		if sess.agent == c || sess.client == c {
			delete(t.sessions, key)
			removed = append(removed, key)
		}
	}
	t.mu.Unlock()
	sortPairingKeys(removed)
	return removed
}

// removeAgent 删除归属于某 agent 的全部承载会话，返回被删除的配对键。
func (t *carrierTable) removeAgent(agentID string) []pairing.PairingKey {
	if agentID == "" {
		return nil
	}
	t.mu.Lock()
	removed := make([]pairing.PairingKey, 0, 4)
	for key, sess := range t.sessions {
		if sess.agentID == agentID {
			delete(t.sessions, key)
			removed = append(removed, key)
		}
	}
	t.mu.Unlock()
	sortPairingKeys(removed)
	return removed
}

// sweep 清理半会话（pair_timeout）与完整会话（udp_session_timeout）。
func (t *carrierTable) sweep(now time.Time, pairTimeout, idleTimeout time.Duration) (half, full []pairing.PairingKey) {
	t.mu.Lock()
	for key, sess := range t.sessions {
		switch {
		case !sess.full && now.Sub(sess.createdAt) >= pairTimeout:
			delete(t.sessions, key)
			half = append(half, key)
		case sess.full && now.Sub(sess.lastSeen) >= idleTimeout:
			delete(t.sessions, key)
			full = append(full, key)
		}
	}
	t.mu.Unlock()
	sortPairingKeys(half)
	sortPairingKeys(full)
	return half, full
}

// snapshot 返回承载会话快照（按配对键排序）。
func (t *carrierTable) snapshot() []carrierEntry {
	t.mu.Lock()
	out := make([]carrierEntry, 0, len(t.sessions))
	for _, sess := range t.sessions {
		entry := carrierEntry{
			Key:       sess.key,
			AgentID:   sess.agentID,
			Full:      sess.full,
			CreatedAt: sess.createdAt,
			LastSeen:  sess.lastSeen,
		}
		if sess.agent != nil {
			entry.AgentRemote = sess.agent.remote
		}
		if sess.client != nil {
			entry.ClientRemote = sess.client.remote
		}
		out = append(out, entry)
	}
	t.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Key.String() < out[j].Key.String() })
	return out
}

// len 返回会话数量。
func (t *carrierTable) len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.sessions)
}

// sortPairingKeys 按配对键字符串排序。
func sortPairingKeys(keys []pairing.PairingKey) {
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
}
