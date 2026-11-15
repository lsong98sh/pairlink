package relay

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lsong98sh/pairlink/internal/logx"
	"github.com/lsong98sh/pairlink/internal/pairing"
	"github.com/lsong98sh/pairlink/internal/protocol"
	"github.com/lsong98sh/pairlink/internal/registry"
)

// TunnelListener 是控制面监听器（隧道端口）。
//
// 每个控制连接一个读循环 goroutine；所有写帧都经过 tunnelConn.write 的
// 互斥锁串行化，订阅推送由独立的 pushLoop goroutine 负责，避免读循环与推送
// 交错写同一连接。
type TunnelListener struct {
	srv *server
	ln  net.Listener

	mu     sync.Mutex
	conns  map[*tunnelConn]struct{}
	agents map[string]*tunnelConn
}

// tunnelConn 是一条控制面连接。
type tunnelConn struct {
	srv  *server
	conn net.Conn
	log  *slog.Logger

	role     string
	agentID  string
	clientID string
	// ready 在 Hello 握手完成后置位；心跳判死等外部 goroutine 只在 ready 后
	// 读取上面的身份字段（原子变量提供 happens-before）。
	ready atomic.Bool

	lastSeen atomic.Int64 // UnixNano

	writeMu sync.Mutex

	// pushMu 串行化"计算并推送服务视图"的动作（单条事件与全量重同步），
	// 保证订阅基线一定先于其后的事件写出，不会出现"事件先于基线"导致
	// client 用旧快照覆盖新视图。
	pushMu sync.Mutex

	subMu     sync.Mutex
	subCancel func()
	subCh     chan registry.Event
	lastRev   uint64

	closeOnce sync.Once
	done      chan struct{}

	// pending 记录本连接已签发但尚未完成配对的配对键（SEC-D2 单连接配额）。
	// 只在 readLoop goroutine 内使用；用独立锁是为了将来允许异步写。
	pendingMu sync.Mutex
	pending   map[string]struct{}
}

// reservePending 登记一条"已签发、待配对"的请求；超过 maxPendingPerConn 返回 false。
//
// 登记前先剔除已经完成配对（令牌被消费）或已过期的键，避免计数只增不减。
func (tc *tunnelConn) reservePending(key pairing.PairingKey) bool {
	id := key.String()
	tc.pendingMu.Lock()
	defer tc.pendingMu.Unlock()
	if tc.pending == nil {
		tc.pending = make(map[string]struct{}, maxPendingPerConn)
	}
	for old := range tc.pending {
		if !tc.srv.tokens.PendingID(old) {
			delete(tc.pending, old)
		}
	}
	if _, ok := tc.pending[id]; ok {
		return true
	}
	if len(tc.pending) >= maxPendingPerConn {
		return false
	}
	tc.pending[id] = struct{}{}
	return true
}

// releasePending 撤销一条登记（签发失败时回滚）。
func (tc *tunnelConn) releasePending(key pairing.PairingKey) {
	tc.pendingMu.Lock()
	delete(tc.pending, key.String())
	tc.pendingMu.Unlock()
}

// acceptLoop 接受控制连接，每条连接一个 goroutine。
func (l *TunnelListener) acceptLoop() {
	defer l.srv.wg.Done()
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			if l.srv.closing.Load() || l.srv.ctx.Err() != nil {
				return
			}
			l.srv.fail(fmt.Errorf("relay: 隧道 accept: %w", err))
			return
		}
		l.srv.log.Debug("控制连接接入",
			"event", "tunnel_accept",
			"remote", conn.RemoteAddr().String(),
		)
		host, ok, reason := l.srv.conns.acquire(conn.RemoteAddr())
		if !ok {
			// SEC-D2：超过 max_conns / max_conns_per_ip 时立即关闭新连接，
			// WARN 由 limiter 限速（每 5 秒最多一条）。
			if !l.srv.conns.warnLimited(l.srv.log, conn.RemoteAddr(), reason) {
				l.srv.log.Debug("控制连接被连接上限拒绝",
					"event", "conn_reject", "reason", reason,
					"remote", conn.RemoteAddr().String())
			}
			_ = conn.Close()
			continue
		}
		l.srv.wg.Add(1)
		go func() {
			defer l.srv.wg.Done()
			defer l.srv.conns.release(host)
			l.handle(conn)
		}()
	}
}

// handle 处理一条控制连接：Hello 握手 → 消息循环 → 清理。
func (l *TunnelListener) handle(conn net.Conn) {
	s := l.srv
	tc := &tunnelConn{
		srv:   s,
		conn:  conn,
		log:   logx.Component(s.log, "tunnel").With("remote", conn.RemoteAddr().String()),
		done:  make(chan struct{}),
		subCh: make(chan registry.Event, 64),
	}
	tc.touch()

	l.mu.Lock()
	l.conns[tc] = struct{}{}
	l.mu.Unlock()

	defer func() {
		l.mu.Lock()
		delete(l.conns, tc)
		if tc.agentID != "" && l.agents[tc.agentID] == tc {
			delete(l.agents, tc.agentID)
		}
		l.mu.Unlock()

		tc.close()
		if tc.role == protocol.RoleAgent && tc.agentID != "" {
			s.agentGone(tc.agentID)
		}
		tc.log.Info("控制连接关闭",
			"event", "data_close",
			"detail", "control",
			"agent_id", tc.agentID,
			"client_id", tc.clientID,
		)
	}()

	// Hello 必须在限定时间内到达，避免半开连接长期占用资源。
	_ = conn.SetReadDeadline(time.Now().Add(controlHelloTimeout))
	// 报到帧只有 ~200 字节；网络入口统一用 64 KiB 上限（SEC-D1 / P0-1）。
	typ, payload, err := protocol.ReadFrameWithLimit(conn, protocol.NetworkPayloadLimit)
	if err != nil {
		tc.log.Warn("读取 Hello 失败", "event", "hello_fail", "err", err)
		return
	}
	if typ != protocol.MsgHello {
		tc.log.Warn("控制面首帧不是 Hello", "event", "bad_request", "type", typ.String())
		return
	}
	raw, err := protocol.Decode(typ, payload)
	if err != nil {
		tc.log.Warn("Hello 解析失败", "event", "bad_request", "err", err)
		return
	}
	hello, ok := raw.(*protocol.Hello)
	if !ok {
		tc.log.Warn("Hello 类型断言失败", "event", "bad_request")
		return
	}

	tc.role = hello.Role
	switch hello.Role {
	case protocol.RoleAgent, protocol.RoleClient:
	default:
		tc.log.Warn("Hello 角色非法", "event", "bad_request", "role", hello.Role)
		_ = tc.writeError(protocol.ErrCodeBadRequest, "role 必须是 agent 或 client")
		return
	}

	if err := protocol.CheckVersion(hello.Version); err != nil {
		tc.log.Warn("协议版本不匹配",
			"event", "version_mismatch",
			"version", hello.Version,
			"err", err,
		)
		_ = tc.writeError(protocol.ErrCodeUnsupportedVersion, "协议版本不匹配")
		return
	}

	if err := s.checkAuth(conn.RemoteAddr(), hello.Token); err != nil {
		tc.log.Warn("隧道接入被拒绝",
			"event", "auth_failed",
			"role", logx.CleanN(hello.Role, logx.MaxAttrLen),
			"client_name", logx.CleanN(hello.ClientName, logx.MaxAttrLen),
			"remote", conn.RemoteAddr().String(),
			"err", err,
		)
		_ = tc.writeError(protocol.ErrCodeAuthFailed, "接入密钥错误")
		return
	}

	prefix := "c-"
	if hello.Role == protocol.RoleAgent {
		prefix = "a-"
	}
	id, err := newSessionID(prefix)
	if err != nil {
		tc.log.Error("生成会话 ID 失败", "event", "internal", "err", err)
		_ = tc.writeError(protocol.ErrCodeInternal, "生成会话 ID 失败")
		return
	}

	if hello.Role == protocol.RoleAgent {
		tc.agentID = id
		if err := s.agents.Add(&registry.Agent{
			ID:          id,
			RemoteAddr:  conn.RemoteAddr().String(),
			ConnectedAt: time.Now(),
			Control:     tc,
		}); err != nil {
			tc.log.Error("登记 agent 失败", "event", "internal", "agent_id", id, "err", err)
			_ = tc.writeError(protocol.ErrCodeInternal, "登记 agent 失败")
			return
		}
		l.mu.Lock()
		l.agents[id] = tc
		l.mu.Unlock()
	} else {
		tc.clientID = id
	}

	// Hello 之后不再使用握手读超时。
	_ = conn.SetReadDeadline(time.Time{})

	helloOK := &protocol.HelloOK{
		Version:          protocol.ProtocolVersion,
		HeartbeatSeconds: s.heartbeatSeconds(),
		UDPMode:          s.udpMode,
		TCPDataPort:      s.tcpPort,
		UDPDataPort:      s.udpPort,
		// 显式下发（含 false）：新 agent/client 据此决定数据面是否 TLS，
		// nil 只可能来自旧 relay，用于触发「明文 + WARN」回退（SEC-T1）。
		DataTLS: boolPtr(s.dataTLS),
	}
	if hello.Role == protocol.RoleAgent {
		helloOK.AgentID = id
	} else {
		helloOK.ClientID = id
	}
	if err := tc.write(protocol.MsgHelloOK, helloOK); err != nil {
		return
	}

	tc.log = tc.log.With(
		"role", hello.Role,
		"agent_id", tc.agentID,
		"client_id", tc.clientID,
	)
	tc.log.Info("控制面接入成功",
		"event", "hello_ok",
		"client_name", logx.CleanN(hello.ClientName, logx.MaxAttrLen),
		"udp_mode", s.udpMode,
	)
	// 身份字段写入完成后再对外可见（心跳判死 ticker 只检查 ready 的连接）。
	tc.ready.Store(true)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		tc.pushLoop()
	}()

	tc.readLoop()
}

// 隧道接入鉴权错误（供日志与单测判别）。
var (
	// errAuthNonLoopbackEmptyToken 表示 auth_token 为空且来源不是回环地址。
	errAuthNonLoopbackEmptyToken = errors.New("relay: auth_token 为空时只允许回环来源接入")
	// errAuthMismatch 表示 auth_token 与配置不匹配。
	errAuthMismatch = errors.New("relay: 接入密钥错误")
)

// checkAuth 校验隧道接入密钥（server 上的入口，转到纯函数 authAllowed）。
func (s *server) checkAuth(remote net.Addr, got string) error {
	return authAllowed(s.cfg.Relay.AuthToken, got, remote)
}

// authAllowed 是鉴权判定的纯函数版本（SEC-A1）。
//
// 规则：
//   - want == ""：**只允许来源为回环地址**（127.0.0.0/8、::1）的 agent/client 接入；
//     非回环来源一律拒绝。空 token 不再是"人人可接入"。
//   - want != ""：token 必须匹配（两侧先做 SHA-256 再常量时间比较，避免
//     长度/内容的时序侧信道）。
//
// 抽成不依赖 server 状态的函数，是为了能在 loopback 测试里覆盖"非回环来源"
// 这一分支（假 relay 只能产生 127.0.0.1 连接）。
func authAllowed(want, got string, remote net.Addr) error {
	if strings.TrimSpace(want) == "" {
		if !isLoopbackRemote(remote) {
			return errAuthNonLoopbackEmptyToken
		}
		return nil
	}
	wantHash := sha256.Sum256([]byte(want))
	gotHash := sha256.Sum256([]byte(got))
	if subtle.ConstantTimeCompare(wantHash[:], gotHash[:]) != 1 {
		return errAuthMismatch
	}
	return nil
}

// isLoopbackRemote 报告连接来源是否在回环网段。
//
// 判定与 internal/admin.IsLoopbackListen、config.isLoopbackListenAddr 一致：
// 127.0.0.0/8（含 IPv4-mapped IPv6 ::ffff:127.x.x.x）、::1。
func isLoopbackRemote(addr net.Addr) bool {
	if addr == nil {
		return false
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		host = addr.String()
	}
	return isLoopbackHostText(host)
}

// isLoopbackHostText 报告 host 文本是否是回环 IP 或 localhost。
func isLoopbackHostText(host string) bool {
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// readLoop 读取并分发控制面消息。
func (tc *tunnelConn) readLoop() {
	for {
		// 控制面网络入口上限 64 KiB（SEC-D1 / P0-1）：服务列表远小于此值。
		typ, payload, err := protocol.ReadFrameWithLimit(tc.conn, protocol.NetworkPayloadLimit)
		if err != nil {
			if tc.srv.ctx.Err() == nil && !tc.isClosed() {
				tc.log.Warn("控制连接读取失败", "event", "data_close", "err", err)
			}
			return
		}
		tc.touch()
		if tc.agentID != "" {
			_ = tc.srv.agents.Touch(tc.agentID, time.Now())
		}

		msg, err := protocol.Decode(typ, payload)
		if err != nil {
			tc.log.Warn("控制面报文解析失败", "event", "bad_request", "type", typ.String(), "err", err)
			_ = tc.writeError(protocol.ErrCodeBadRequest, "报文结构非法")
			return
		}
		if !tc.srv.tunnel.dispatch(tc, typ, msg) {
			return
		}
	}
}

// dispatch 处理一条已解码的控制面消息；返回 false 表示关闭连接。
func (l *TunnelListener) dispatch(tc *tunnelConn, typ protocol.MessageType, msg any) bool {
	switch typ {
	case protocol.MsgHeartbeat:
		return tc.write(protocol.MsgHeartbeatAck, &protocol.HeartbeatAck{}) == nil
	case protocol.MsgHeartbeatAck:
		return true
	case protocol.MsgRegisterService:
		l.handleRegisterService(tc, msg.(*protocol.RegisterService))
		return true
	case protocol.MsgUnregisterService:
		l.handleUnregisterService(tc, msg.(*protocol.UnregisterService))
		return true
	case protocol.MsgListServices:
		l.handleListServices(tc)
		return true
	case protocol.MsgSubscribe:
		req := msg.(*protocol.Subscribe)
		tc.setSubscribed(req.Enable)
		tc.log.Info("服务订阅状态变更", "event", "subscribe", "enable", req.Enable)
		return true
	case protocol.MsgConnectRequest:
		l.handleConnectRequest(tc, msg.(*protocol.ConnectRequest))
		return true
	case protocol.MsgHello:
		_ = tc.writeError(protocol.ErrCodeBadRequest, "重复的 Hello")
		return false
	case protocol.MsgError:
		peerErr := msg.(*protocol.Error)
		tc.log.Warn("对端返回错误", "event", "peer_error",
			"code", logx.CleanN(peerErr.Code, logx.MaxAttrLen),
			"detail", logx.CleanN(peerErr.Message, logx.MaxAttrLen))
		return true
	default:
		if !typ.Valid() {
			tc.log.Debug("忽略未知控制消息", "event", "message_ignored", "type", typ.String())
			return true
		}
		tc.log.Debug("忽略非预期控制消息", "event", "message_ignored", "type", typ.String())
		return true
	}
}

// handleRegisterService 处理 agent 的服务注册（SEC-A2）。
//
// 依次检查：白名单 → 单 agent 服务数上限 → 归属规则（同 owner 覆盖 / 异 owner
// 在线拒绝 / 异 owner 离线接管）→ 注册并回 MsgServiceRegistered。
// 拒绝原因通过 MsgError.Detail 下发稳定的机器可读码（service_denied /
// service_conflict / service_limit），agent 据此给出可读提示。
func (l *TunnelListener) handleRegisterService(tc *tunnelConn, req *protocol.RegisterService) {
	s := tc.srv
	if tc.role != protocol.RoleAgent {
		_ = tc.writeErrorDetail(protocol.ErrCodeBadRequest, "只有 agent 可以注册服务", "")
		return
	}
	if !validServiceID(req.ServiceID) {
		_ = tc.writeErrorDetail(protocol.ErrCodeBadRequest, "service_id 非法", "")
		return
	}
	if !validProto(req.Proto) {
		_ = tc.writeErrorDetail(protocol.ErrCodeBadRequest, "proto 必须是 tcp 或 udp", "")
		return
	}

	// 1) 白名单（空 = 不限制，启动时已 WARN）。
	if !s.serviceAllowed(req.ServiceID) {
		tc.log.Warn("服务注册被拒绝：服务名不在白名单",
			"event", "service_denied",
			"service_id", req.ServiceID,
			"agent_id", tc.agentID,
			"detail", "service_allow 未列出该服务名",
		)
		_ = tc.writeErrorDetail(protocol.ErrCodeBadRequest, "服务名不在白名单（relay 的 [relay] service_allow 未列出 "+req.ServiceID+"）", protocol.ErrDetailServiceDenied)
		return
	}

	// 2) 单 agent 服务数上限（同一服务重复注册不重复计数）。
	existing, exists := s.services.Get(req.ServiceID)
	if limit := s.maxServicesPerAgent(); limit > 0 && !(exists && existing.AgentID == tc.agentID) {
		if len(s.services.ListByAgent(tc.agentID)) >= limit {
			tc.log.Warn("服务注册被拒绝：该 agent 服务数已达上限",
				"event", "service_denied",
				"reason", "max_services_per_agent",
				"service_id", req.ServiceID,
				"agent_id", tc.agentID,
				"limit", limit,
			)
			_ = tc.writeErrorDetail(protocol.ErrCodeBadRequest,
				fmt.Sprintf("该 agent 已注册 %d 个服务，达到上限 max_services_per_agent = %d", limit, limit),
				protocol.ErrDetailServiceLimit)
			return
		}
	}

	// 3) 归属规则。
	if exists && existing.AgentID != tc.agentID {
		if s.agentOnline(existing.AgentID) {
			// 原 owner 仍在线：拒绝，避免在线抢占/流量劫持。
			tc.log.Warn("服务注册被拒绝：服务已由其它在线 agent 注册",
				"event", "service_conflict",
				"service_id", req.ServiceID,
				"owner", existing.AgentID,
				"requester", tc.agentID,
				"detail", "原 owner 在线，拒绝抢占",
			)
			_ = tc.writeErrorDetail(protocol.ErrCodeBadRequest, "服务已由其它 agent 注册", protocol.ErrDetailServiceConflict)
			return
		}
		// 原 owner 已离线：允许接管（真实 agent 重连自愈），并 WARN 留痕。
		if _, _, ok := s.services.RecentOwner(req.ServiceID); !ok {
			// 在线表存在但历史表未记录（极少见：直接构造状态/历史被淘汰），补一条。
			s.services.RememberOwner(req.ServiceID, existing.AgentID, time.Now())
		}
		tc.log.Warn("服务已由离线 agent 接管",
			"event", "service_takeover",
			"service_id", req.ServiceID,
			"old_owner", existing.AgentID,
			"new_owner", tc.agentID,
			"detail", "原 owner 离线或心跳超时，允许新 agent 接管",
		)
	} else if !exists {
		// 在线表已无该服务（正常断开/注销即删除，保持原有语义）：查"最近归属"
		// 表判断是否为离线接管；原 owner 仍在线（主动注销释放）时不报接管。
		if owner, at, ok := s.services.RecentOwner(req.ServiceID); ok && owner != tc.agentID && !s.agentOnline(owner) {
			tc.log.Warn("服务已由离线 agent 接管",
				"event", "service_takeover",
				"service_id", req.ServiceID,
				"old_owner", owner,
				"new_owner", tc.agentID,
				"disconnected_at", at.Format(time.RFC3339),
				"detail", "服务已从在线表移除且原 owner 不在线，允许新 agent 接管",
			)
		}
	}

	entry := registry.ServiceEntry{
		ID:           req.ServiceID,
		Proto:        req.Proto,
		AgentID:      tc.agentID,
		AgentAddr:    tc.remoteAddr(),
		Online:       true,
		RegisteredAt: time.Now(),
	}
	if err := s.services.Register(entry); err != nil {
		_ = tc.writeErrorDetail(protocol.ErrCodeInternal, "服务注册失败", "")
		return
	}

	ids := make([]string, 0, 4)
	for _, svc := range s.services.ListByAgent(tc.agentID) {
		ids = append(ids, svc.ID)
	}
	_ = s.agents.SetServices(tc.agentID, ids)

	if err := tc.write(protocol.MsgServiceRegistered, &protocol.ServiceRegistered{
		ServiceID: req.ServiceID,
		Proto:     req.Proto,
		UDPMode:   s.udpMode,
	}); err != nil {
		return
	}
	tc.log.Info("服务注册",
		"event", "service_register",
		"service_id", req.ServiceID,
		"proto", req.Proto,
		"owner", tc.agentID,
		"local_hint", req.LocalHint,
	)
}

// handleUnregisterService 处理 agent 的服务注销（阶段 7，0x13）。
//
// 只允许注销本 agent 名下的服务：owner 不符或服务不存在都回 bad_request，
// 不泄露服务归属细节。成功后注册表派发 removed 事件（订阅者可见），
// 并回 MsgServiceUnregistered。
func (l *TunnelListener) handleUnregisterService(tc *tunnelConn, req *protocol.UnregisterService) {
	s := tc.srv
	if tc.role != protocol.RoleAgent {
		_ = tc.writeError(protocol.ErrCodeBadRequest, "只有 agent 可以注销服务")
		return
	}
	if !validServiceID(req.ServiceID) {
		_ = tc.writeError(protocol.ErrCodeBadRequest, "service_id 非法")
		return
	}
	if err := s.services.Unregister(req.ServiceID, tc.agentID); err != nil {
		switch {
		case errors.Is(err, registry.ErrServiceNotFound):
			_ = tc.writeError(protocol.ErrCodeBadRequest, "服务未注册")
		case errors.Is(err, registry.ErrServiceOwnerMismatch):
			_ = tc.writeError(protocol.ErrCodeBadRequest, "服务不属于本 agent")
		default:
			tc.log.Error("注销服务失败", "event", "service_unregister", "service_id", req.ServiceID, "err", err)
			_ = tc.writeError(protocol.ErrCodeInternal, "服务注销失败")
		}
		return
	}

	ids := make([]string, 0, 4)
	for _, svc := range s.services.ListByAgent(tc.agentID) {
		ids = append(ids, svc.ID)
	}
	_ = s.agents.SetServices(tc.agentID, ids)

	if err := tc.write(protocol.MsgServiceUnregistered, &protocol.ServiceUnregistered{
		ServiceID: req.ServiceID,
	}); err != nil {
		return
	}
	tc.log.Info("服务注销",
		"event", "service_unregister",
		"service_id", req.ServiceID,
	)
}

// handleListServices 返回当前服务列表。
func (l *TunnelListener) handleListServices(tc *tunnelConn) {
	s := tc.srv
	entries := s.services.List()
	infos := make([]protocol.ServiceInfo, 0, len(entries))
	for _, entry := range entries {
		infos = append(infos, s.serviceInfo(entry))
	}
	if err := tc.write(protocol.MsgServiceList, &protocol.ServiceList{
		Services: infos,
		Revision: s.services.Revision(),
		UDPMode:  s.udpMode,
	}); err != nil {
		return
	}
	tc.log.Debug("服务列表已返回", "event", "service_list", "count", len(infos))
}

// handleConnectRequest 处理 client 的接入请求：签发令牌并双向通知。
func (l *TunnelListener) handleConnectRequest(tc *tunnelConn, req *protocol.ConnectRequest) {
	s := tc.srv
	if tc.role != protocol.RoleClient {
		_ = tc.writeError(protocol.ErrCodeBadRequest, "只有 client 可以发起接入请求")
		return
	}
	// 本函数产生的错误都直接对应这一次接入请求：带上 client_conn_id，
	// 让 client 能精确关闭对应的本地连接（旧 client 会忽略该字段）。
	// 只回显长度合理的 ID（1..64），避免把超长输入原样放大回写。
	connID := req.ClientConnID
	if len(connID) > 64 {
		connID = ""
	}
	if !validServiceID(req.ServiceID) || connID == "" {
		_ = tc.writeErrorFor(protocol.ErrCodeBadRequest, "service_id 或 client_conn_id 非法", connID)
		return
	}
	key := pairing.PairingKey{ServiceID: req.ServiceID, ClientConnID: req.ClientConnID}
	if err := key.Validate(); err != nil {
		_ = tc.writeErrorFor(protocol.ErrCodeBadRequest, "配对键非法", connID)
		return
	}

	entry, ok := s.services.Get(req.ServiceID)
	if !ok {
		tc.log.Warn("接入请求的服务不存在",
			"event", "connect_request",
			"service_id", req.ServiceID,
			"pairing_key", key.String(),
		)
		_ = tc.writeErrorFor(protocol.ErrCodeServiceNotFound, "服务未注册", connID)
		return
	}
	if !entry.Online {
		_ = tc.writeErrorFor(protocol.ErrCodeNoAgentOnline, "服务当前不可接入", connID)
		return
	}

	agentOnline := s.agentOnline(entry.AgentID)
	if !agentOnline {
		tc.log.Warn("接入请求的 agent 不在线",
			"event", "connect_request",
			"service_id", req.ServiceID,
			"agent_id", entry.AgentID,
		)
		_ = tc.writeErrorFor(protocol.ErrCodeNoAgentOnline, "服务所属 agent 不在线", connID)
		return
	}

	// SEC-D2：配额计入"待配对占位 + 已签发令牌"（tokens.Len() 含已消费未过期条目），
	// 上限可配（[relay] max_pending_pairs，默认 4096，0 = 不限制）。
	if limit := s.cfg.Relay.MaxPendingPairs; limit > 0 && s.pairs.Len()+s.tokens.Len() >= limit {
		tc.log.Warn("待配对/令牌数已达上限，拒绝接入请求",
			"event", "connect_request",
			"reason", "max_pending_pairs",
			"limit", limit,
			"pending_pairs", s.pairs.Len(),
			"tokens", s.tokens.Len(),
			"service_id", req.ServiceID,
		)
		_ = tc.writeErrorFor(protocol.ErrCodeTooManyPending, "待配对表已满，请稍后重试", connID)
		return
	}

	agentConn := s.tunnel.agentConn(entry.AgentID)
	if agentConn == nil {
		_ = tc.writeErrorFor(protocol.ErrCodeNoAgentOnline, "服务所属 agent 不在线", connID)
		return
	}

	// SEC-D2：单连接"已签发但未完成配对"的请求数上限（默认 64）。
	if !tc.reservePending(key) {
		tc.log.Warn("单连接未完成请求数已达上限，拒绝接入请求",
			"event", "connect_request",
			"reason", "max_pending_per_conn",
			"limit", maxPendingPerConn,
			"pairing_key", key.String(),
		)
		_ = tc.writeErrorFor(protocol.ErrCodeTooManyPending, "该连接未完成的接入请求过多，请稍后重试", connID)
		return
	}

	pairingToken, err := s.tokens.Issue(key)
	if err != nil {
		tc.releasePending(key)
		tc.log.Error("签发配对令牌失败", "event", "internal", "pairing_key", key.String(), "err", err)
		_ = tc.writeErrorFor(protocol.ErrCodeInternal, "签发配对令牌失败", connID)
		return
	}

	incoming := &protocol.Incoming{
		PairingKey:   key.String(),
		PairingToken: pairingToken,
		Proto:        entry.Proto,
		Role:         protocol.RoleAgent,
		ClientAddr:   tc.remoteAddr(),
	}
	if err := agentConn.write(protocol.MsgIncoming, incoming); err != nil {
		agentConn.close()
		_ = tc.writeErrorFor(protocol.ErrCodeNoAgentOnline, "服务所属 agent 不在线", connID)
		return
	}

	pairResult := &protocol.PairResult{
		PairingKey:   key.String(),
		PairingToken: pairingToken,
		Proto:        entry.Proto,
		Role:         protocol.RoleClient,
	}
	if err := tc.write(protocol.MsgPairResult, pairResult); err != nil {
		return
	}

	tc.log.Info("接入请求已签发配对",
		"event", "pair_issue",
		"service_id", req.ServiceID,
		"agent_id", entry.AgentID,
		"pairing_key", key.String(),
		"proto", entry.Proto,
	)
}

// setSubscribed 订阅/取消订阅服务变更。
//
// 订阅成功后会立即推送一次**全量** MsgServiceList：既给 client 一个新鲜的
// 服务视图基线（它启动时拉到的列表可能已经过期），也把 lastRev 对齐到注册表
// 当前版本，避免把第一个事件误判成"revision 断档"。
func (tc *tunnelConn) setSubscribed(enable bool) {
	tc.subMu.Lock()
	if enable {
		if tc.subCancel != nil {
			tc.subMu.Unlock()
			return
		}
		tc.subCancel = tc.srv.services.Subscribe(func(ev registry.Event) {
			select {
			case tc.subCh <- ev:
			default:
				// 订阅者处理不过来时丢弃事件，绝不阻塞注册表写操作。
				// 丢弃会导致 revision 断档，pushLoop 检测到后会改发全量列表兜底。
				tc.log.Warn("订阅事件队列已满，丢弃一次推送（将触发全量重同步）",
					"event", "service_resync",
					"service_id", ev.Service.ID,
					"type", ev.Type,
					"revision", ev.Revision,
				)
			}
		})
		tc.subMu.Unlock()
		tc.pushMu.Lock()
		err := tc.pushServiceListLocked()
		tc.pushMu.Unlock()
		if err != nil {
			// 写失败说明连接已不可用，readLoop 随后会退出并走既有清理路径。
			tc.log.Warn("订阅后下发全量服务列表失败", "event", "service_list", "err", err)
		}
		return
	}
	cancel := tc.subCancel
	tc.subCancel = nil
	tc.subMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// pushLoop 串行地把服务变更事件写到控制连接。
//
// revision 必须**严格连续**：注册表在锁外派发事件，多个写入者并发 Register /
// Unregister 时事件可能乱序；推送队列满时还会丢事件。遇到非连续 revision 时
// 不再丢弃或补拍单条事件（那会让 client 永久看不到某个变更），而是改发一次
// 全量 MsgServiceList 让 client 整体重同步。
func (tc *tunnelConn) pushLoop() {
	for {
		select {
		case <-tc.done:
			return
		case ev := <-tc.subCh:
			// pushMu 让"判断 revision + 写帧"与订阅基线的下发互斥，
			// 避免事件先于基线到达 client。
			tc.pushMu.Lock()
			last := tc.lastRevision()
			if ev.Revision <= last {
				tc.log.Debug("丢弃旧的服务事件", "event", "service_event",
					"service_id", ev.Service.ID,
					"detail", fmt.Sprintf("event=%s revision=%d seen=%d", ev.Type, ev.Revision, last))
				tc.pushMu.Unlock()
				continue
			}
			if ev.Revision != last+1 {
				err := tc.pushServiceListLocked()
				tc.pushMu.Unlock()
				if err != nil {
					return
				}
				tc.log.Warn("服务事件 revision 不连续，已改为全量重同步",
					"event", "service_resync",
					"detail", fmt.Sprintf("got=%d expected=%d", ev.Revision, last+1),
					"service_id", ev.Service.ID,
				)
				continue
			}
			tc.bumpLastRev(ev.Revision)

			info := tc.srv.serviceInfo(ev.Service)
			if ev.Type == registry.EventRemoved {
				info.Online = false
			}
			err := tc.write(protocol.MsgServiceEvent, &protocol.ServiceEvent{
				Event:    ev.Type,
				Service:  info,
				Revision: ev.Revision,
			})
			tc.pushMu.Unlock()
			if err != nil {
				return
			}
			tc.log.Debug("服务变更已推送",
				"event", "service_event",
				"service_id", ev.Service.ID,
				"type", ev.Type,
				"revision", ev.Revision,
			)
		}
	}
}

// pushServiceListLocked 在持有 pushMu 时下发一次全量服务列表。
//
// revision 先读、列表后取：列表状态一定不旧于 revision，因此把 lastRev 提到
// revision 是安全的（后续 revision+1 的事件仍会被正常应用）。
func (tc *tunnelConn) pushServiceListLocked() error {
	s := tc.srv
	rev := s.services.Revision()
	entries := s.services.List()
	infos := make([]protocol.ServiceInfo, 0, len(entries))
	for _, entry := range entries {
		infos = append(infos, s.serviceInfo(entry))
	}
	if err := tc.write(protocol.MsgServiceList, &protocol.ServiceList{
		Services: infos,
		Revision: rev,
		UDPMode:  s.udpMode,
	}); err != nil {
		return err
	}
	tc.bumpLastRev(rev)
	return nil
}

// lastRevision 返回已经推送给该 client 的注册表版本号。
func (tc *tunnelConn) lastRevision() uint64 {
	tc.subMu.Lock()
	defer tc.subMu.Unlock()
	return tc.lastRev
}

// bumpLastRev 把已推送版本号提高到 rev（只增不减，允许并发调用）。
func (tc *tunnelConn) bumpLastRev(rev uint64) {
	tc.subMu.Lock()
	if rev > tc.lastRev {
		tc.lastRev = rev
	}
	tc.subMu.Unlock()
}

// write 写一帧（默认超时）。
func (tc *tunnelConn) write(typ protocol.MessageType, payload any) error {
	return tc.writeTimeout(typ, payload, controlWriteTimeout)
}

// writeTimeout 加写锁并写一帧；失败时关闭连接，避免半个帧污染后续通信。
func (tc *tunnelConn) writeTimeout(typ protocol.MessageType, payload any, timeout time.Duration) error {
	tc.writeMu.Lock()
	defer tc.writeMu.Unlock()
	if tc.isClosed() {
		return net.ErrClosed
	}
	if timeout > 0 {
		_ = tc.conn.SetWriteDeadline(time.Now().Add(timeout))
	}
	err := protocol.WriteFrame(tc.conn, typ, payload)
	_ = tc.conn.SetWriteDeadline(time.Time{})
	if err != nil {
		tc.close()
	}
	return err
}

// writeError 写一帧 MsgError。
func (tc *tunnelConn) writeError(code, message string) error {
	return tc.write(protocol.MsgError, &protocol.Error{Code: code, Message: message})
}

// writeErrorFor 写一帧带 client_conn_id 关联的 MsgError。
//
// 只用于直接应答某个 MsgConnectRequest 的错误；connID 为空时退化为普通错误帧。
func (tc *tunnelConn) writeErrorFor(code, message, clientConnID string) error {
	return tc.write(protocol.MsgError, &protocol.Error{
		Code:         code,
		Message:      message,
		ClientConnID: clientConnID,
	})
}

// writeErrorDetail 写一帧带稳定 Detail 的 MsgError。
//
// 服务注册拒绝用 Detail 下发机器可读原因（service_denied / service_conflict /
// service_limit，SEC-A2）；旧 agent 忽略该字段，只看 message。
func (tc *tunnelConn) writeErrorDetail(code, message, detail string) error {
	return tc.write(protocol.MsgError, &protocol.Error{
		Code:    code,
		Message: message,
		Detail:  detail,
	})
}

// close 关闭控制连接（幂等）。
func (tc *tunnelConn) close() {
	tc.closeOnce.Do(func() {
		close(tc.done)
		tc.subMu.Lock()
		if tc.subCancel != nil {
			tc.subCancel()
			tc.subCancel = nil
		}
		tc.subMu.Unlock()
		_ = tc.conn.Close()
	})
}

// isClosed 报告连接是否已关闭。
func (tc *tunnelConn) isClosed() bool {
	select {
	case <-tc.done:
		return true
	default:
		return false
	}
}

// touch 刷新最后活跃时间。
func (tc *tunnelConn) touch() { tc.lastSeen.Store(time.Now().UnixNano()) }

// lastSeenTime 返回最后活跃时间。
func (tc *tunnelConn) lastSeenTime() time.Time { return time.Unix(0, tc.lastSeen.Load()) }

// remoteAddr 返回对端地址字符串。
func (tc *tunnelConn) remoteAddr() string {
	if tc.conn == nil || tc.conn.RemoteAddr() == nil {
		return ""
	}
	return tc.conn.RemoteAddr().String()
}

// Close 实现 registry.Conn，供管理页踢出使用。
func (tc *tunnelConn) Close() error {
	tc.close()
	return nil
}

// RemoteAddr 实现 registry.Conn。
func (tc *tunnelConn) RemoteAddr() net.Addr {
	if tc.conn == nil {
		return nil
	}
	return tc.conn.RemoteAddr()
}

// agentConn 返回指定 agent 的控制连接（不存在时 nil）。
func (l *TunnelListener) agentConn(agentID string) *tunnelConn {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.agents[agentID]
}

// snapshot 返回当前全部控制连接。
func (l *TunnelListener) snapshot() []*tunnelConn {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]*tunnelConn, 0, len(l.conns))
	for tc := range l.conns {
		out = append(out, tc)
	}
	return out
}

// shutdown 停监听并向全部控制连接发送停机通知，然后关闭连接。
func (l *TunnelListener) shutdown() {
	_ = l.ln.Close()

	l.mu.Lock()
	conns := make([]*tunnelConn, 0, len(l.conns))
	for tc := range l.conns {
		conns = append(conns, tc)
	}
	l.mu.Unlock()

	for _, tc := range conns {
		_ = tc.writeTimeout(protocol.MsgServerShutdown, &protocol.ServerShutdown{
			Reason: "relay 即将停机",
		}, shutdownWriteTimeout)
		tc.close()
	}
}
