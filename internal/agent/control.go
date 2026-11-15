package agent

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/logx"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// controlConn 是控制面长连接，写操作必须串行（单写锁）。
//
// 读只由控制面读循环 goroutine 负责；握手/注册阶段在启动 goroutine 中同步读，
// 两者不重叠，因此不需要读锁。
//
// 阶段 7：管理页的服务变更需要"发请求 → 等 relay 确认"，确认帧由 controlLoop
// 读到后通过 pending 通道交给等待方（同一时刻只允许一个在途变更，由 agent.regMu
// 串行化）。握手/首次注册阶段使用同步读，不经过 pending。
type controlConn struct {
	conn net.Conn
	mu   sync.Mutex

	pendingMu sync.Mutex
	pendingID string
	pendingCh chan serviceAck
}

// serviceAck 是一次服务变更请求的应答。
type serviceAck struct {
	// typ 是确认帧类型：MsgServiceRegistered / MsgServiceUnregistered；
	// 出错时为零值且 err 非 nil。
	typ       protocol.MessageType
	serviceID string
	udpMode   string
	err       error
}

// beginServiceWait 登记一个在途服务变更，返回等待通道。
func (c *controlConn) beginServiceWait(serviceID string) chan serviceAck {
	ch := make(chan serviceAck, 1)
	c.pendingMu.Lock()
	c.pendingID = serviceID
	c.pendingCh = ch
	c.pendingMu.Unlock()
	return ch
}

// endServiceWait 清除在途登记（幂等；只清自己登记的那个通道）。
func (c *controlConn) endServiceWait(ch chan serviceAck) {
	c.pendingMu.Lock()
	if c.pendingCh == ch {
		c.pendingCh = nil
		c.pendingID = ""
	}
	c.pendingMu.Unlock()
}

// deliverServiceAck 把控制面确认交给等待方；没有等待方或 serviceID 不匹配时丢弃。
//
// MsgError 的 serviceID 为空（协议未带该字段），按"应答当前在途请求"处理。
func (c *controlConn) deliverServiceAck(ack serviceAck) {
	c.pendingMu.Lock()
	ch := c.pendingCh
	id := c.pendingID
	c.pendingMu.Unlock()
	if ch == nil {
		return
	}
	if ack.serviceID != "" && id != "" && !strings.EqualFold(ack.serviceID, id) {
		return
	}
	select {
	case ch <- ack:
	default:
	}
}

// failServiceWait 在控制连接关闭时唤醒等待方，避免白等超时。
func (c *controlConn) failServiceWait(err error) {
	if err == nil {
		err = errors.New("agent: 控制连接已关闭")
	}
	c.deliverServiceAck(serviceAck{err: err})
}

// newControlConn 包装一个已建立的控制连接。
func newControlConn(conn net.Conn) *controlConn {
	return &controlConn{conn: conn}
}

// write 串行写出一帧。
func (c *controlConn) write(typ protocol.MessageType, payload any) error {
	if c == nil || c.conn == nil {
		return errors.New("agent: 控制连接已关闭")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return protocol.WriteFrame(c.conn, typ, payload)
}

// readFrame 读一帧；timeout > 0 时设置一次性读 deadline（握手/注册阶段用）。
func (c *controlConn) readFrame(timeout time.Duration) (protocol.MessageType, []byte, error) {
	if c == nil || c.conn == nil {
		return 0, nil, errors.New("agent: 控制连接已关闭")
	}
	if timeout > 0 {
		if err := c.conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
			return 0, nil, err
		}
		defer func() { _ = c.conn.SetReadDeadline(time.Time{}) }()
	}
	return protocol.ReadFrameWithLimit(c.conn, protocol.NetworkPayloadLimit)
}

// close 关闭控制连接（幂等）。
func (c *controlConn) close() {
	if c == nil || c.conn == nil {
		return
	}
	c.failServiceWait(errors.New("agent: 控制连接已关闭"))
	_ = c.conn.Close()
}

// remoteAddr 返回控制连接对端地址。
func (c *controlConn) remoteAddr() string {
	if c == nil || c.conn == nil {
		return ""
	}
	if a := c.conn.RemoteAddr(); a != nil {
		return a.String()
	}
	return ""
}

// handshake 在给定控制连接上发送 MsgHello 并处理 MsgHelloOK。
//
// relay 明确拒绝（MsgError）与版本不兼容属于不可重试错误，返回 permanentErr。
func (a *agent) handshake(ctrl *controlConn) error {
	hello := &protocol.Hello{
		Role:       protocol.RoleAgent,
		Token:      a.cfg.Server.Token,
		Version:    protocol.ProtocolVersion,
		ClientName: hostname(),
	}
	if err := ctrl.write(protocol.MsgHello, hello); err != nil {
		return fmt.Errorf("agent: 发送 MsgHello: %w", err)
	}
	timeout := a.dialTimeout()
	typ, payload, err := ctrl.readFrame(timeout)
	if err != nil {
		return fmt.Errorf("agent: 等待 MsgHelloOK: %w", err)
	}
	switch typ {
	case protocol.MsgHelloOK:
	case protocol.MsgError:
		return permanent(fmt.Errorf("agent: relay 拒绝接入: %s", protocolErrText(payload)))
	default:
		return fmt.Errorf("agent: 期望 MsgHelloOK，收到 %s", typ)
	}

	decoded, err := protocol.Decode(typ, payload)
	if err != nil {
		return fmt.Errorf("agent: 解析 MsgHelloOK: %w", err)
	}
	ok, isOK := decoded.(*protocol.HelloOK)
	if !isOK {
		return fmt.Errorf("agent: MsgHelloOK 负载类型异常 %T", decoded)
	}
	if err := protocol.CheckVersion(ok.Version); err != nil {
		return permanent(fmt.Errorf("agent: relay 协议版本不兼容: %w", err))
	}

	// 数据面 TLS（SEC-T1）：本地 [server] data_tls × relay 的 HelloOK.data_tls。
	// 不兼容属永久错误（不重连、不静默降级）；旧 relay 未下发时按明文 + WARN。
	negotiation := config.NegotiateDataTLS(a.cfg.Server.DataTLS, ok.DataTLS)
	if negotiation.Err != nil {
		return permanent(fmt.Errorf("agent: %w", negotiation.Err))
	}
	if negotiation.Warning != "" {
		a.log.Warn(negotiation.Warning, "event", "data_tls", "detail", "old_relay_or_missing_field")
	}
	var dataTLSCfg *tls.Config
	if negotiation.UseTLS {
		cfg, cfgErr := dataTLSConfig(&a.cfg.Server, a.relayHost)
		if cfgErr != nil {
			return permanent(cfgErr)
		}
		dataTLSCfg = cfg
	}
	a.setDataTLS(negotiation.UseTLS, dataTLSCfg)

	mode := normalizeUDPMode(ok.UDPMode, a.log, "hello_ok")
	a.ctrlMu.Lock()
	a.agentID = ok.AgentID
	a.relayVersion = ok.Version
	a.connectedAt = time.Now()
	a.lastAck = a.connectedAt
	a.udpMode = mode
	a.tcpDataPort = ok.TCPDataPort
	a.udpDataPort = ok.UDPDataPort
	a.heartbeat = heartbeatInterval(a.cfg.Server.Heartbeat, time.Duration(ok.HeartbeatSeconds)*time.Second)
	a.ctrlMu.Unlock()

	a.log.Info("接入 relay 成功", "event", "hello_ok",
		"agent_id", ok.AgentID,
		"data_tls", negotiation.UseTLS,
		"detail", fmt.Sprintf("version=%d udp_mode=%s tcp_data_port=%d udp_data_port=%d heartbeat_seconds=%d data_tls=%v",
			ok.Version, mode, ok.TCPDataPort, ok.UDPDataPort, ok.HeartbeatSeconds, negotiation.UseTLS),
	)
	a.logDataPlaneTLS(negotiation.UseTLS, ok.DataTLS)

	// 原生 UDP 数据面不引入 DTLS：relay 要求数据面 TLS 时明确提示 UDP 仍明文。
	if negotiation.UseTLS && mode == protocol.UDPModeNative {
		a.log.Warn("udp_mode = native：原生 UDP 数据面仍为明文（不引入 DTLS），只有 TCP 数据面走 TLS；如需加密请让 relay 移除 udp_listen 使用 udp_mode = tcp_tunnel",
			"event", "config_warning", "detail", "data_tls 不覆盖原生 UDP")
	}
	return nil
}

// logDataPlaneTLS 打印本次连接的数据面加密状态（SEC-T1 可见性要求）。
func (a *agent) logDataPlaneTLS(useTLS bool, advertised *bool) {
	if useTLS {
		verify := "系统根证书"
		if strings.TrimSpace(a.cfg.Server.TLSCA) != "" {
			verify = "tls_ca=" + a.cfg.Server.TLSCA
		}
		if a.cfg.Server.TLSInsecureSkipVerify {
			verify = "跳过证书校验（tls_insecure_skip_verify = true，仅建议测试环境）"
		}
		a.log.Info("数据面: TLS（TCP）", "event", "data_tls", "detail", "数据面: TLS（"+verify+"）")
		return
	}
	reason := "relay 未启用数据面 TLS"
	switch {
	case advertised == nil:
		reason = "旧 relay 未下发 data_tls（< v0.4.0）"
	case a.cfg.Server.DataTLS == config.TLSFalse:
		reason = "data_tls = false（本端强制明文）"
	}
	a.log.Warn("数据面: 明文（业务数据与 pairing_token 不加密）",
		"event", "data_tls", "detail", "数据面: 明文（"+reason+"）")
}

// registerServices 依次注册 cfg.Server.All() 的全部服务。
//
// 每次（重）连接都要重新注册：relay 在旧控制连接断开时会注销该 agent 的全部服务，
// 重连后必须重建注册表，否则服务列表里看不到本机服务。
func (a *agent) registerServices(ctrl *controlConn) error {
	a.resetRegisteredServices()
	timeout := a.dialTimeout()
	for _, svc := range a.svcStore.snapshot() {
		req := &protocol.RegisterService{
			ServiceID: svc.ID,
			Proto:     svc.Proto,
			LocalHint: svc.Addr,
		}
		if err := ctrl.write(protocol.MsgRegisterService, req); err != nil {
			return fmt.Errorf("agent: 注册服务 %s: %w", svc.ID, err)
		}
		mode, err := a.awaitRegistered(ctrl, svc, timeout)
		if err != nil {
			return err
		}
		if mode != "" {
			a.setUDPMode(mode)
		}
		a.addRegisteredService(registeredService{Service: svc, UDPMode: a.udpModeSnapshot()})
		a.log.Info("服务已注册", "event", "service_registered",
			"service_id", svc.ID,
			"proto", svc.Proto,
			"detail", "local="+svc.Addr,
		)
	}
	return nil
}

// awaitRegistered 等待指定服务的 MsgServiceRegistered，返回响应里的 udp_mode。
//
// 期间收到的其它控制面消息（如心跳）按协议处理而不是报错。
// relay 明确拒绝（MsgError）不可重试，返回 permanentErr。
func (a *agent) awaitRegistered(ctrl *controlConn, svc config.ServerService, timeout time.Duration) (string, error) {
	for {
		typ, payload, err := ctrl.readFrame(timeout)
		if err != nil {
			return "", fmt.Errorf("agent: 等待服务 %s 的注册确认: %w", svc.ID, err)
		}
		switch typ {
		case protocol.MsgServiceRegistered:
			decoded, derr := protocol.Decode(typ, payload)
			if derr != nil {
				return "", fmt.Errorf("agent: 解析服务 %s 的注册确认: %w", svc.ID, derr)
			}
			reg, ok := decoded.(*protocol.ServiceRegistered)
			if !ok {
				return "", fmt.Errorf("agent: MsgServiceRegistered 负载类型异常 %T", decoded)
			}
			if reg.ServiceID != "" && reg.ServiceID != svc.ID {
				a.log.Debug("忽略其它服务的注册确认", "event", "service_registered", "service_id", reg.ServiceID)
				continue
			}
			if reg.UDPMode == "" {
				return "", nil
			}
			return normalizeUDPMode(reg.UDPMode, a.log, "service_registered"), nil
		case protocol.MsgError:
			return "", permanent(serviceRegisterError(svc.ID, payload))
		case protocol.MsgHeartbeat:
			if err := ctrl.write(protocol.MsgHeartbeatAck, &protocol.HeartbeatAck{}); err != nil {
				return "", fmt.Errorf("agent: 回复心跳: %w", err)
			}
		default:
			a.log.Debug("注册期间收到忽略的消息", "event", "control_ignore", "detail", typ.String())
		}
	}
}

// resolveDataAddrs 按 docs/contract.md §4.5 推导数据面拨号地址。
//
// TCP 数据面始终推导；UDP 数据面只在 udp_mode=native 时推导（tcp_tunnel 复用
// TCP 数据面）。端口优先取 HelloOK 下发的值，旧 relay 未下发时回退本 ini 的
// [relay] 段；仍取不到只告警（该协议的配对会被放弃），不影响 Run 继续服务其它服务。
//
// 每次（重）连接都重新推导：relay 重启后端口可能变化，旧地址不能沿用。
func (a *agent) resolveDataAddrs() {
	var tcpAddr, udpAddr string

	tcpPort := a.tcpDataPortSnapshot()
	if tcpPort <= 0 {
		tcpPort = listenPort(a.cfg.Relay.TCPListen)
	}
	if addr, err := dataAddr(a.relayHost, tcpPort); err != nil {
		a.log.Warn("无法推导 TCP 数据面地址，TCP 服务配对将被放弃", "event", "data_addr",
			"detail", "tcp_data_port/relay.tcp_listen 均不可用", "err", err)
	} else {
		tcpAddr = addr
	}

	// 只有 native 模式才需要推导原生 UDP 数据面地址；tcp_tunnel 复用
	// tcpAddr，不应因为 relay 未下发 udp_data_port 而告警。
	mode := a.udpModeSnapshot()
	if mode == protocol.UDPModeNative {
		udpPort := a.udpDataPortSnapshot()
		if udpPort <= 0 {
			udpPort = listenPort(a.cfg.Relay.UDPListen)
		}
		if addr, err := dataAddr(a.relayHost, udpPort); err != nil {
			a.log.Warn("无法推导 UDP 数据面地址，原生 UDP 配对将被放弃", "event", "data_addr",
				"detail", "udp_data_port/relay.udp_listen 均不可用", "err", err)
		} else {
			udpAddr = addr
		}
	}

	a.setDataAddrs(tcpAddr, udpAddr)

	if mode == protocol.UDPModeTCPTunnel && tcpAddr == "" {
		a.log.Warn("udp_mode=tcp_tunnel 但 TCP 数据面地址不可用，UDP 服务无法建立数据面", "event", "data_addr")
	}
}

// controlLoop 是控制面唯一的读循环；返回即代表控制面终止。
//
// MsgIncoming 必须转交独立 goroutine，绝不阻塞读循环。返回 nil 表示收到
// MsgServerShutdown，返回 errKicked（包装后）表示被踢出。
func (a *agent) controlLoop(ctrl *controlConn) error {
	for {
		// 网络入口上限 64 KiB（SEC-D1 / P0-1），与服务列表的实际规模一致。
		typ, payload, err := protocol.ReadFrameWithLimit(ctrl.conn, protocol.NetworkPayloadLimit)
		if err != nil {
			// 只有「我们自己关停」才算正常结束；对端关闭连接属于运行中可恢复错误。
			if a.ctx.Err() != nil || a.controlConn() != ctrl {
				return nil
			}
			return fmt.Errorf("agent: 控制连接已断开: %w", err)
		}
		switch typ {
		case protocol.MsgIncoming:
			decoded, derr := protocol.Decode(typ, payload)
			if derr != nil {
				a.log.Warn("接入通知解析失败，已忽略", "event", "incoming", "err", derr)
				continue
			}
			msg, ok := decoded.(*protocol.Incoming)
			if !ok {
				a.log.Warn("接入通知负载类型异常，已忽略", "event", "incoming", "detail", fmt.Sprintf("%T", decoded))
				continue
			}
			a.wg.Add(1)
			select {
			case a.incomingSem <- struct{}{}:
				go func() {
					defer a.wg.Done()
					defer func() { <-a.incomingSem }()
					a.handleIncoming(msg)
				}()
			default:
				// SEC-D2：并发 MsgIncoming 超过 256 时丢弃本次通知并记 WARN。
				// 丢弃是安全的：client 侧数据面报到会因令牌校验/超时而失败，
				// 不会产生半开会话；绝不因此 panic 或无限起 goroutine。
				a.wg.Done()
				a.log.Warn("并发接入通知已达上限，丢弃本次通知",
					"event", "incoming_drop",
					"pairing_key", logx.CleanN(msg.PairingKey, logx.MaxAttrLen),
					"limit", maxConcurrentIncoming,
				)
			}
		case protocol.MsgHeartbeat:
			if err := ctrl.write(protocol.MsgHeartbeatAck, &protocol.HeartbeatAck{}); err != nil {
				a.log.Warn("回复 relay 心跳失败", "event", "heartbeat", "err", err)
			}
		case protocol.MsgHeartbeatAck:
			a.ctrlMu.Lock()
			a.lastAck = time.Now()
			a.ctrlMu.Unlock()
		case protocol.MsgServiceRegistered:
			decoded, derr := protocol.Decode(typ, payload)
			if derr != nil {
				a.log.Warn("服务注册确认解析失败", "event", "service_registered", "err", derr)
				continue
			}
			reg, ok := decoded.(*protocol.ServiceRegistered)
			if !ok {
				continue
			}
			ctrl.deliverServiceAck(serviceAck{
				typ:       protocol.MsgServiceRegistered,
				serviceID: reg.ServiceID,
				udpMode:   reg.UDPMode,
			})
		case protocol.MsgServiceUnregistered:
			decoded, derr := protocol.Decode(typ, payload)
			if derr != nil {
				a.log.Warn("服务注销确认解析失败", "event", "service_unregistered", "err", derr)
				continue
			}
			unreg, ok := decoded.(*protocol.ServiceUnregistered)
			if !ok {
				continue
			}
			ctrl.deliverServiceAck(serviceAck{
				typ:       protocol.MsgServiceUnregistered,
				serviceID: unreg.ServiceID,
			})
		case protocol.MsgError:
			a.log.Warn("relay 下发错误", "event", "relay_error", "detail", protocolErrText(payload))
			ctrl.deliverServiceAck(serviceAck{err: fmt.Errorf("relay 拒绝: %s", protocolErrText(payload))})
		case protocol.MsgKicked:
			reason := decodeReason(protocol.MsgKicked, payload)
			a.log.Warn("已被 relay 踢出，不自动重连", "event", "kicked", "detail", reason)
			return fmt.Errorf("%w（%s）", errKicked, reason)
		case protocol.MsgServerShutdown:
			reason := decodeReason(protocol.MsgServerShutdown, payload)
			a.log.Info("relay 即将停机，agent 主动退出", "event", "server_shutdown", "detail", reason)
			return nil
		default:
			a.log.Debug("忽略未处理的控制面消息", "event", "control_ignore", "detail", typ.String())
		}
	}
}

// heartbeatLoop 在给定控制连接上周期性发送 MsgHeartbeat，ctx 取消即退出。
//
// 心跳与连接同生命周期：重连后由 serveControl 为新连接重建心跳。
func (a *agent) heartbeatLoop(ctx context.Context, ctrl *controlConn) {
	interval := positiveDuration(a.heartbeatSnapshot(), defaultHeartbeat)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := ctrl.write(protocol.MsgHeartbeat, &protocol.Heartbeat{}); err != nil {
				a.log.Warn("发送心跳失败", "event", "heartbeat", "err", err)
				continue
			}
			a.log.Debug("已发送心跳", "event", "heartbeat", "detail", "interval="+interval.String())
		}
	}
}

// heartbeatSnapshot 返回协商后的心跳间隔。
func (a *agent) heartbeatSnapshot() time.Duration {
	a.ctrlMu.Lock()
	defer a.ctrlMu.Unlock()
	return a.heartbeat
}

// startBackground 启动本地 UDP 会话清理 ticker（心跳按连接创建，见 serveControl）。
func (a *agent) startBackground() {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.sweepLoop()
	}()
}

// normalizeUDPMode 校验并规范化 relay 下发的 udp_mode；缺失或未知值回退 native 并 WARN。
func normalizeUDPMode(mode string, log *slog.Logger, where string) string {
	switch mode {
	case protocol.UDPModeNative, protocol.UDPModeTCPTunnel:
		return mode
	case "":
		if log != nil {
			log.Warn("relay 未下发 udp_mode，回退 native", "event", "config_warning", "detail", where)
		}
		return protocol.UDPModeNative
	default:
		if log != nil {
			log.Warn("relay 下发了未识别的 udp_mode，回退 native", "event", "config_warning",
				"detail", fmt.Sprintf("%s: udp_mode=%q", where, mode))
		}
		return protocol.UDPModeNative
	}
}

// protocolErrText 把 MsgError 的负载转成可读错误文本（不含任何令牌）。
//
// code / message 都由对端（relay）控制，返回前清洗控制字符并截断到
// logx.MaxAttrLen，避免日志注入/放大（SEC-M2）。
func protocolErrText(payload []byte) string {
	decoded, err := protocol.Decode(protocol.MsgError, payload)
	if err != nil {
		return "无法解析的错误响应"
	}
	e, ok := decoded.(*protocol.Error)
	if !ok || e == nil {
		return "错误响应类型异常"
	}
	return errTextOf(e)
}

// errTextOf 把 protocol.Error 转成清洗后的 "code: message" 文本。
func errTextOf(e *protocol.Error) string {
	code := logx.CleanN(e.Code, logx.MaxAttrLen)
	msg := logx.CleanN(e.Message, logx.MaxAttrLen)
	if msg == "" {
		return code
	}
	return code + ": " + msg
}

// serviceRegisterError 把 relay 的服务注册拒绝映射成可读、可执行的错误（SEC-A2）。
//
// relay 在 MsgError.Detail 中下发稳定原因码（v0.4.0+）；旧 relay 没有该字段时
// 退化为通用的 code + message，不解析中文文本。
func serviceRegisterError(serviceID string, payload []byte) error {
	decoded, err := protocol.Decode(protocol.MsgError, payload)
	if err != nil {
		return fmt.Errorf("agent: relay 拒绝注册服务 %s: %s", serviceID, protocolErrText(payload))
	}
	e, ok := decoded.(*protocol.Error)
	if !ok || e == nil {
		return fmt.Errorf("agent: relay 拒绝注册服务 %s: %s", serviceID, protocolErrText(payload))
	}
	detail := logx.CleanN(e.Message, logx.MaxAttrLen)
	switch e.Detail {
	case protocol.ErrDetailServiceDenied:
		return fmt.Errorf("agent: relay 拒绝注册服务 %s：服务名不在 relay 的 [relay] service_allow 白名单（%s）；请在 relay 端加入该服务名，或改用允许的名字", serviceID, detail)
	case protocol.ErrDetailServiceConflict:
		return fmt.Errorf("agent: relay 拒绝注册服务 %s：服务名已被另一个在线 agent 占用（%s）；同一服务名同时只允许一个 agent 提供，请确认是否重复部署或原 agent 尚未下线", serviceID, detail)
	case protocol.ErrDetailServiceLimit:
		return fmt.Errorf("agent: relay 拒绝注册服务 %s：本 agent 注册的服务数已达 relay 上限（%s）；请减少服务数量或调大 relay 的 max_services_per_agent", serviceID, detail)
	default:
		return fmt.Errorf("agent: relay 拒绝注册服务 %s: %s", serviceID, protocolErrText(payload))
	}
}

// decodeReason 解析 MsgKicked / MsgServerShutdown 的 reason 字段（清洗+截断）。
func decodeReason(typ protocol.MessageType, payload []byte) string {
	decoded, err := protocol.Decode(typ, payload)
	if err != nil {
		return ""
	}
	switch v := decoded.(type) {
	case *protocol.Kicked:
		return logx.CleanN(v.Reason, logx.MaxAttrLen)
	case *protocol.ServerShutdown:
		return logx.CleanN(v.Reason, logx.MaxAttrLen)
	}
	return ""
}
