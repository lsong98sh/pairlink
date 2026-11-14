package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// controlConn 是控制面长连接，写操作必须串行（单写锁）。
//
// 读只由控制面读循环 goroutine 负责；握手/注册阶段在启动 goroutine 中同步读，
// 两者不重叠，因此不需要读锁。
type controlConn struct {
	conn net.Conn
	mu   sync.Mutex
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
	return protocol.ReadFrame(c.conn)
}

// close 关闭控制连接（幂等）。
func (c *controlConn) close() {
	if c == nil || c.conn == nil {
		return
	}
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
		"detail", fmt.Sprintf("version=%d udp_mode=%s tcp_data_port=%d udp_data_port=%d heartbeat_seconds=%d",
			ok.Version, mode, ok.TCPDataPort, ok.UDPDataPort, ok.HeartbeatSeconds),
	)
	return nil
}

// registerServices 依次注册 cfg.Server.All() 的全部服务。
//
// 每次（重）连接都要重新注册：relay 在旧控制连接断开时会注销该 agent 的全部服务，
// 重连后必须重建注册表，否则服务列表里看不到本机服务。
func (a *agent) registerServices(ctrl *controlConn) error {
	a.resetRegisteredServices()
	timeout := a.dialTimeout()
	for _, svc := range a.cfg.Server.All() {
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
			return "", permanent(fmt.Errorf("agent: relay 拒绝注册服务 %s: %s", svc.ID, protocolErrText(payload)))
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
		typ, payload, err := protocol.ReadFrame(ctrl.conn)
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
			go func() {
				defer a.wg.Done()
				a.handleIncoming(msg)
			}()
		case protocol.MsgHeartbeat:
			if err := ctrl.write(protocol.MsgHeartbeatAck, &protocol.HeartbeatAck{}); err != nil {
				a.log.Warn("回复 relay 心跳失败", "event", "heartbeat", "err", err)
			}
		case protocol.MsgHeartbeatAck:
			a.ctrlMu.Lock()
			a.lastAck = time.Now()
			a.ctrlMu.Unlock()
		case protocol.MsgError:
			a.log.Warn("relay 下发错误", "event", "relay_error", "detail", protocolErrText(payload))
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
func protocolErrText(payload []byte) string {
	decoded, err := protocol.Decode(protocol.MsgError, payload)
	if err != nil {
		return "无法解析的错误响应"
	}
	e, ok := decoded.(*protocol.Error)
	if !ok || e == nil {
		return "错误响应类型异常"
	}
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

// decodeReason 解析 MsgKicked / MsgServerShutdown 的 reason 字段。
func decodeReason(typ protocol.MessageType, payload []byte) string {
	decoded, err := protocol.Decode(typ, payload)
	if err != nil {
		return ""
	}
	switch v := decoded.(type) {
	case *protocol.Kicked:
		return v.Reason
	case *protocol.ServerShutdown:
		return v.Reason
	}
	return ""
}
