package relay

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/lsong98sh/pairlink/internal/forward"
	"github.com/lsong98sh/pairlink/internal/logx"
	"github.com/lsong98sh/pairlink/internal/pairing"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// TCPDataListener 是 TCP 数据面监听器（tcp_listen）。
//
// 同一端口同时服务两类连接：
//   - carrier 为空 / "tcp"：读取一帧 MsgBind 后进入纯字节透传；
//   - carrier = "udp-over-tcp"：读取一帧 MsgBind 后转为 UDP 隧道帧解析循环。
type TCPDataListener struct {
	srv *server
	ln  net.Listener
}

// pairWait 是配对占位的等待信号。
//
// 同一配对键的两侧共享一个 pairWait：先到者 Claim 返回 ErrWaiting 后等待，
// 后到者 Claim 成功时 claim 并接管两条连接；超时/踢出/agent 下线时 cancel，
// 由等待方关闭自己的连接。
type pairWait struct {
	mu        sync.Mutex
	claimed   bool
	cancelled bool
	done      chan struct{}
	// users 由 server.waitMu 保护（不能在这个 mutex 内修改）。
	users int
}

// newPairWait 创建等待信号。
func newPairWait() *pairWait { return &pairWait{done: make(chan struct{})} }

// claim 声明配对已由对端完成；返回 false 表示已被取消。
func (w *pairWait) claim() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cancelled {
		return false
	}
	if w.claimed {
		return true
	}
	w.claimed = true
	close(w.done)
	return true
}

// cancel 取消等待；返回 false 表示已经 claim 或已经取消。
func (w *pairWait) cancel() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.claimed || w.cancelled {
		return false
	}
	w.cancelled = true
	close(w.done)
	return true
}

// outcome 返回当前状态。
func (w *pairWait) outcome() (claimed, cancelled bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.claimed, w.cancelled
}

// activePair 是一组正在双向转发的数据连接，供管理页踢出使用。
type activePair struct {
	key    pairing.PairingKey
	agent  net.Conn
	client net.Conn
}

// acceptLoop 接受 TCP 数据面连接。
func (l *TCPDataListener) acceptLoop() {
	defer l.srv.wg.Done()
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			if l.srv.closing.Load() || l.srv.ctx.Err() != nil {
				return
			}
			l.srv.fail(fmt.Errorf("relay: TCP 数据面 accept: %w", err))
			return
		}
		l.srv.wg.Add(1)
		go func() {
			defer l.srv.wg.Done()
			l.srv.handleDataConn(conn)
		}()
	}
}

// handleDataConn 处理一条 TCP 数据面连接。
func (s *server) handleDataConn(conn net.Conn) {
	remote := ""
	if conn.RemoteAddr() != nil {
		remote = conn.RemoteAddr().String()
	}
	log := logx.Component(s.log, "tcp-data").With("remote", remote)

	// 连接所有权：正常路径由本函数（或 forward.Bidirectional）关闭；
	// 先到的一侧完成配对后把连接移交给对端 goroutine，此时不再关闭。
	owned := true
	defer func() {
		s.dataConns.remove(conn)
		if owned {
			_ = conn.Close()
		}
	}()

	if !s.dataConns.add(conn) {
		return
	}

	if err := conn.SetReadDeadline(time.Now().Add(s.cfg.Relay.PairTimeout)); err != nil {
		log.Debug("设置数据面读超时失败", "event", "bind_fail", "err", err)
		return
	}
	typ, payload, err := protocol.ReadFrame(conn)
	if err != nil {
		log.Debug("数据面报到读取失败",
			"event", "bind_fail",
			"err", err,
			"detail", "读一帧 MsgBind 失败或超时",
		)
		return
	}
	if typ != protocol.MsgBind {
		log.Warn("数据面首帧不是 MsgBind", "event", "bind_fail", "type", typ.String())
		return
	}
	raw, err := protocol.Decode(typ, payload)
	if err != nil {
		log.Warn("MsgBind 解析失败", "event", "bind_fail", "err", err)
		return
	}
	bind, ok := raw.(*protocol.Bind)
	if !ok {
		log.Warn("MsgBind 类型断言失败", "event", "bind_fail")
		return
	}

	releaseOwnership := s.handleBind(conn, bind, log)
	owned = !releaseOwnership
}

// handleBind 按 carrier 分流；返回 true 表示把连接所有权移交给了对端。
func (s *server) handleBind(conn net.Conn, bind *protocol.Bind, log *slog.Logger) bool {
	switch bind.Carrier {
	case "", protocol.CarrierTCP:
		return s.handleTCPBind(conn, bind, log)
	case protocol.CarrierUDPOverTCP:
		s.handleCarrierBind(conn, bind, log)
		return false
	default:
		log.Warn("未知的 carrier", "event", "bind_fail", "carrier", bind.Carrier)
		return false
	}
}

// handleTCPBind 处理纯透传的数据面报到：校验 → Claim → 等待/转发。
//
// 返回 true 表示本连接已移交给对端（先到的一侧成功配对），调用方不得关闭。
func (s *server) handleTCPBind(conn net.Conn, bind *protocol.Bind, log *slog.Logger) bool {
	side, ok := sideFromRole(bind.Role)
	if !ok {
		log.Warn("MsgBind 角色非法", "event", "bind_fail", "role", bind.Role)
		return false
	}
	key, err := pairing.ParsePairingKey(bind.PairingKey)
	if err != nil {
		log.Warn("配对键非法", "event", "bind_fail", "pairing_key", bind.PairingKey, "err", err)
		return false
	}
	log = log.With("pairing_key", key.String(), "role", bind.Role, "service_id", key.ServiceID)

	if err := s.tokens.Validate(key, bind.PairingToken); err != nil {
		log.Warn("数据面令牌校验失败", "event", "bind_fail", "err", err)
		return false
	}

	// 记录 agent 侧数据连接，agent 控制连接断开时统一关闭。
	agentID := ""
	if side == pairing.SideAgent {
		agentID = s.agentIDForService(key.ServiceID)
		s.trackAgentDataConn(agentID, conn)
		defer s.untrackAgentDataConn(agentID, conn)
	}

	w, _ := s.acquireWait(key)
	pair, claimErr := s.pairs.Claim(key, side, conn)
	switch {
	case claimErr == nil:
		// 对端先到，本条连接完成配对；由本 goroutine 负责双向转发。
		if !w.claim() {
			// 等待方已超时/被取消：它已经或即将关闭自己的连接，这里直接放弃。
			s.releaseWait(key, w)
			closePairPeers(pair)
			return false
		}
		s.releaseWait(key, w)
		s.finishPair(key, pair, log)
		return false

	case errors.Is(claimErr, pairing.ErrWaiting):
		// 先到：保持连接静默等待，绝对不写任何字节。
		timer := time.NewTimer(s.cfg.Relay.PairTimeout)
		defer timer.Stop()

		select {
		case <-w.done:
		case <-timer.C:
		case <-s.ctx.Done():
		}
		claimed, _ := w.outcome()
		if !claimed {
			// 若既非 claim 也非 cancel（定时器到期），补一次 cancel；
			// 若已被其它路径取消，cancel 返回 false，但清理仍需进行。
			if !w.cancel() {
				claimed, _ = w.outcome()
			}
		}
		if claimed {
			// 对端接管了本连接，交给对端 goroutine 转发。
			s.releaseWait(key, w)
			return true
		}
		s.releaseWait(key, w)
		s.pairs.Remove(key)
		log.Warn("配对等待超时或取消",
			"event", "pair_timeout",
			"agent_id", agentID,
		)
		return false

	default:
		s.releaseWait(key, w)
		event := "bind_fail"
		switch {
		case errors.Is(claimErr, pairing.ErrAlreadyPaired):
			event = "pair_replay"
		case errors.Is(claimErr, pairing.ErrTimeout):
			event = "pair_timeout"
		}
		log.Warn("数据面报到被拒绝", "event", event, "err", claimErr)
		return false
	}
}

// finishPair 完成配对后的收尾：清 deadline、登记活动表、双向转发、清理。
func (s *server) finishPair(key pairing.PairingKey, pair pairing.Pair, log *slog.Logger) {
	agentConn, _ := pair.Agent.(net.Conn)
	clientConn, _ := pair.Client.(net.Conn)
	if agentConn == nil || clientConn == nil {
		closePairPeers(pair)
		s.pairs.Remove(key)
		return
	}

	s.tokens.Consume(key)
	_ = agentConn.SetDeadline(time.Time{})
	_ = clientConn.SetDeadline(time.Time{})

	s.addActive(key, agentConn, clientConn)
	defer s.removeActive(key)

	log.Info("配对完成",
		"event", "pair_complete",
		"agent_id", s.agentIDForService(key.ServiceID),
		"proto", "tcp",
	)

	agentWrapped := forward.WithIdleTimeout(agentConn, s.cfg.Relay.IdleTimeout)
	clientWrapped := forward.WithIdleTimeout(clientConn, s.cfg.Relay.IdleTimeout)
	outcome := forward.BidirectionalWithOutcome(s.ctx, agentWrapped, clientWrapped)

	// 空闲超时与异常结束要能在日志里区分：Bidirectional 只给字节数时，
	// 运维无法看出连接是被 idle_timeout 关掉的，还是被对端正常关闭的。
	switch {
	case outcome.TimedOut():
		log.Warn("数据面空闲超时，关闭配对",
			"event", "idle_timeout",
			"agent_id", s.agentIDForService(key.ServiceID),
			"detail", "idle_timeout="+s.cfg.Relay.IdleTimeout.String(),
		)
	case outcome.Err() != nil:
		log.Warn("数据面异常结束，关闭配对",
			"event", "data_close",
			"agent_id", s.agentIDForService(key.ServiceID),
			"err", outcome.Err(),
		)
	}

	s.pairs.Remove(key)
	log.Info("数据连接关闭",
		"event", "data_close",
		"agent_id", s.agentIDForService(key.ServiceID),
		"bytes_agent_to_client", outcome.AToB,
		"bytes_client_to_agent", outcome.BToA,
	)
}

// sideFromRole 把 Bind.Role 转换为 pairing.Side。
func sideFromRole(role string) (pairing.Side, bool) {
	switch role {
	case protocol.RoleAgent:
		return pairing.SideAgent, true
	case protocol.RoleClient:
		return pairing.SideClient, true
	default:
		return pairing.SideUnknown, false
	}
}

// closePairPeers 关闭配对结果中的两条连接。
func closePairPeers(pair pairing.Pair) {
	if c, ok := pair.Agent.(net.Conn); ok && c != nil {
		_ = c.Close()
	}
	if c, ok := pair.Client.(net.Conn); ok && c != nil {
		_ = c.Close()
	}
}

// acquireWait 获取配对键对应的等待信号（必要时创建），并增加引用计数。
func (s *server) acquireWait(key pairing.PairingKey) (*pairWait, bool) {
	s.waitMu.Lock()
	defer s.waitMu.Unlock()
	w, ok := s.waits[key]
	if !ok {
		w = newPairWait()
		s.waits[key] = w
	}
	w.users++
	return w, !ok
}

// releaseWait 减少等待信号引用计数；无人使用后从表中删除。
func (s *server) releaseWait(key pairing.PairingKey, w *pairWait) {
	if w == nil {
		return
	}
	s.waitMu.Lock()
	w.users--
	if w.users <= 0 {
		if s.waits[key] == w {
			delete(s.waits, key)
		}
	}
	s.waitMu.Unlock()
}

// lookupWait 返回等待信号（不增加引用计数），用于踢出/agent 下线。
func (s *server) lookupWait(key pairing.PairingKey) *pairWait {
	s.waitMu.Lock()
	defer s.waitMu.Unlock()
	return s.waits[key]
}

// addActive 登记一组正在转发的数据连接。
func (s *server) addActive(key pairing.PairingKey, agent, client net.Conn) {
	s.activeMu.Lock()
	s.active[key] = activePair{key: key, agent: agent, client: client}
	s.activeMu.Unlock()
}

// removeActive 移除活动配对记录。
func (s *server) removeActive(key pairing.PairingKey) {
	s.activeMu.Lock()
	delete(s.active, key)
	s.activeMu.Unlock()
}

// takeActive 取出并移除活动配对记录（管理页踢出用）。
func (s *server) takeActive(key pairing.PairingKey) (activePair, bool) {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	ap, ok := s.active[key]
	if ok {
		delete(s.active, key)
	}
	return ap, ok
}

// activeSnapshot 返回当前活动配对快照。
func (s *server) activeSnapshot() []activePair {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	out := make([]activePair, 0, len(s.active))
	for _, ap := range s.active {
		out = append(out, ap)
	}
	return out
}
