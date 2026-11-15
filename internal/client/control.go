package client

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/logx"
	"github.com/lsong98sh/pairlink/internal/pairing"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// connectOnce 建立控制连接并完成 Hello / ListServices / Subscribe 握手，返回后
// 本连接可用于读写控制面。
//
// 顺序固定（docs/contract.md §2.5）：HelloOK → ServiceList →（可选）Subscribe。
// 每次（重）连接都整体刷新协商状态（clientID / udp_mode / 心跳 / 数据面地址），
// 并用 relay 下发的全量服务列表**强制**重建本地视图基线（relay 重启后 revision
// 会归零，若沿用「忽略更旧 revision」的规则，视图会永久卡死）。
func (c *client) connectOnce() error {
	conn, isTLS, err := c.dialControl()
	if err != nil {
		return err
	}
	c.setControl(conn)
	c.ctrlLog.Info("控制连接已建立", "event", "control_connect",
		"remote", conn.RemoteAddr().String(), "detail", fmt.Sprintf("tls=%t", isTLS))

	// 握手阶段允许 ctx 取消立即打断阻塞读（不必等 dial_timeout）。
	stopWatch := c.watchCancel(conn)
	defer stopWatch()

	// 握手阶段用读 deadline 兜底，结束后必须清除（读循环依赖阻塞读 + ctx 取消）。
	defer func() {
		_ = conn.SetReadDeadline(time.Time{})
		_ = conn.SetWriteDeadline(time.Time{})
	}()
	timeout := c.dialTimeout()

	// 1) Hello
	hostname, _ := os.Hostname()
	hello := &protocol.Hello{
		Role:       protocol.RoleClient,
		Token:      c.cfg.Client.Token,
		Version:    protocol.ProtocolVersion,
		ClientName: hostname,
	}
	if err := c.writeControlTo(conn, protocol.MsgHello, hello); err != nil {
		c.closeControl()
		return fmt.Errorf("client: 发送 hello: %w", err)
	}
	msg, err := c.readControlFrame(conn, timeout)
	if err != nil {
		c.closeControl()
		return fmt.Errorf("client: 读取 hello 应答: %w", err)
	}
	helloOK, ok := msg.(*protocol.HelloOK)
	if !ok {
		c.closeControl()
		if e, isErr := msg.(*protocol.Error); isErr {
			// relay 明确拒绝（auth_failed 等）：重连没有意义。
			return permanent(fmt.Errorf("client: relay 拒绝接入: %w", e))
		}
		return fmt.Errorf("client: 期望 hello_ok，收到 %T", msg)
	}
	if err := protocol.CheckVersion(helloOK.Version); err != nil {
		c.closeControl()
		return permanent(fmt.Errorf("client: relay 协议版本不匹配: %w", err))
	}

	// 数据面 TLS（SEC-T1）：本地 [client] data_tls × relay 的 HelloOK.data_tls。
	// 不兼容属永久错误（不重连、不静默降级）；旧 relay 未下发时按明文 + WARN。
	dataNeg := config.NegotiateDataTLS(c.cfg.Client.DataTLS, helloOK.DataTLS)
	if dataNeg.Err != nil {
		c.closeControl()
		return permanent(fmt.Errorf("client: %w", dataNeg.Err))
	}
	if dataNeg.Warning != "" {
		c.ctrlLog.Warn(dataNeg.Warning, "event", "data_tls", "detail", "old_relay_or_missing_field")
	}
	var dataTLSCfg *tls.Config
	if dataNeg.UseTLS {
		cfg, cfgErr := c.dataTLSConfig()
		if cfgErr != nil {
			c.closeControl()
			return cfgErr
		}
		dataTLSCfg = cfg
	}

	neg := negotiated{
		clientID:   helloOK.ClientID,
		udpMode:    c.resolveUDPMode(helloOK.UDPMode, "hello_ok"),
		heartbeat:  negotiateHeartbeat(c.cfg.Client.Heartbeat, helloOK.HeartbeatSeconds),
		tcpPort:    helloOK.TCPDataPort,
		udpPort:    helloOK.UDPDataPort,
		dataTLS:    dataNeg.UseTLS,
		dataTLSCfg: dataTLSCfg,
	}
	c.ctrlLog.Info("接入成功", "event", "hello_ok", "client_id", neg.clientID,
		"data_tls", neg.dataTLS,
		"detail", fmt.Sprintf("udp_mode=%s tcp_data_port=%d udp_data_port=%d heartbeat=%s data_tls=%v",
			neg.udpMode, neg.tcpPort, neg.udpPort, neg.heartbeat, neg.dataTLS))
	c.logDataPlaneTLS(dataNeg.UseTLS, helloOK.DataTLS)

	// 原生 UDP 数据面不引入 DTLS：relay 要求数据面 TLS 时明确提示 UDP 仍明文。
	if dataNeg.UseTLS && neg.udpMode == protocol.UDPModeNative {
		c.ctrlLog.Warn("udp_mode = native：原生 UDP 数据面仍为明文（不引入 DTLS），只有 TCP 数据面走 TLS；如需加密请让 relay 移除 udp_listen 使用 udp_mode = tcp_tunnel",
			"event", "config_warning", "detail", "data_tls 不覆盖原生 UDP")
	}

	// 2) 服务列表（既是管理页数据，也是本地监听协议的唯一依据）
	if err := c.writeControlTo(conn, protocol.MsgListServices, &protocol.ListServices{}); err != nil {
		c.closeControl()
		return fmt.Errorf("client: 发送 list_services: %w", err)
	}
	msg, err = c.readControlFrame(conn, timeout)
	if err != nil {
		c.closeControl()
		return fmt.Errorf("client: 读取服务列表: %w", err)
	}
	list, ok := msg.(*protocol.ServiceList)
	if !ok {
		c.closeControl()
		if e, isErr := msg.(*protocol.Error); isErr {
			return permanent(fmt.Errorf("client: 拉取服务列表被拒绝: %w", e))
		}
		return fmt.Errorf("client: 期望 service_list，收到 %T", msg)
	}
	if list.UDPMode != "" {
		neg.udpMode = c.resolveUDPMode(list.UDPMode, "service_list")
	}
	c.applyServiceListForce(list, true)
	c.ctrlLog.Info("服务列表已获取", "event", "service_list",
		"detail", fmt.Sprintf("services=%d revision=%d udp_mode=%s",
			len(list.Services), list.Revision, neg.udpMode))

	// 3) 数据面拨号地址（§4.5）
	//
	// 推导失败不再让 Run 退出：只在握手阶段 WARN，等到真正需要数据面时放弃该次
	// 配对并关闭本地连接/会话。
	neg.tcpDataAddr, neg.tcpDataErr = dataAddr(c.host, neg.tcpPort, c.cfg.Relay.TCPListen)
	if neg.tcpDataErr != nil {
		c.ctrlLog.Warn("无法推导 relay TCP 数据面地址，TCP 接入将在配对阶段失败（Run 继续运行）",
			"event", "hello_ok", "detail", "tcp_data_addr_unavailable", "err", neg.tcpDataErr)
	}
	if neg.udpMode == protocol.UDPModeNative {
		addr, addrErr := dataAddr(c.host, neg.udpPort, c.cfg.Relay.UDPListen)
		if addrErr != nil {
			neg.udpDataErr = addrErr
		} else {
			neg.udpDataAddr = addr
			udpAddr, resolveErr := net.ResolveUDPAddr("udp", addr)
			if resolveErr != nil {
				neg.udpDataErr = fmt.Errorf("client: 解析 UDP 数据面地址 %s: %w", addr, resolveErr)
			} else {
				neg.udpDataUDPAddr = udpAddr
			}
		}
		if neg.udpDataErr != nil {
			c.ctrlLog.Warn("无法推导 relay 原生 UDP 数据面地址，UDP 接入将在配对阶段失败（Run 继续运行）",
				"event", "hello_ok", "detail", "udp_data_addr_unavailable", "err", neg.udpDataErr)
		}
	}
	c.setNegotiated(neg)

	// 4) 订阅（可选）：重连后必须重新订阅，否则不再收到服务变更推送。
	if c.cfg.Client.Subscribe {
		if err := c.writeControlTo(conn, protocol.MsgSubscribe, &protocol.Subscribe{Enable: true}); err != nil {
			c.closeControl()
			return fmt.Errorf("client: 发送 subscribe: %w", err)
		}
		c.ctrlLog.Info("已订阅服务列表变更", "event", "subscribe")
	}
	return nil
}

// setNegotiated 发布本次连接的协商状态快照。
func (c *client) setNegotiated(n negotiated) {
	c.negMu.Lock()
	c.negState = n
	c.negMu.Unlock()
}

// negotiated 返回当前协商状态快照（未连接时为零值）。
func (c *client) negotiated() negotiated {
	c.negMu.RLock()
	defer c.negMu.RUnlock()
	return c.negState
}

// setControl 发布当前控制连接。
func (c *client) setControl(conn net.Conn) {
	c.ctrlMu.Lock()
	c.ctrl = conn
	c.ctrlMu.Unlock()
}

// controlConn 返回当前控制连接（nil 表示未连接/重连等待中）。
func (c *client) controlConn() net.Conn {
	c.ctrlMu.Lock()
	defer c.ctrlMu.Unlock()
	return c.ctrl
}

// closeControl 关闭并清空当前控制连接（幂等）。
func (c *client) closeControl() {
	c.ctrlMu.Lock()
	conn := c.ctrl
	c.ctrl = nil
	c.ctrlMu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

// dialControl 建立到 relay 隧道端口的连接，返回连接与是否使用了 TLS。
//
// TLS 三层决策（阶段 6G，见 docs/architecture.md §7.6）：
//   - tls = true → 必须 TLS；tls_ca 非空时 RootCAs 只含该 CA，
//     tls_ca 为空时使用系统根证书做标准校验（无需 tls_insecure_skip_verify）；
//   - tls = false → 必定明文，tls_ca / tls_insecure_skip_verify 被忽略（配了材料则 WARN）；
//   - tls = auto（默认）→ tls_ca 非空或 tls_insecure_skip_verify=true 时 TLS，否则明文，
//     与阶段 1~6 的既有行为一致。
//
// 该规则与 relay 侧「tls=true 或 auto 且有证书 → tls.Listen」是对称的；两侧配置不匹配时
// 握手会失败，此时返回带排查提示的错误，属预期行为，不做自动降级/协议探测。
func (c *client) dialControl() (net.Conn, bool, error) {
	mode := c.cfg.Client.TLS
	if !mode.DialTLS(c.cfg.Client.TLSCA, c.cfg.Client.TLSInsecureSkipVerify) {
		// auto + 无材料仍是明文（向后兼容），但明确提示风险与出口（SEC-T2）。
		if mode == config.TLSAuto && strings.TrimSpace(c.cfg.Client.TLSCA) == "" && !c.cfg.Client.TLSInsecureSkipVerify {
			c.ctrlLog.Warn("当前以明文连接 relay（隧道内 token 与信令可被链路上的人读取）；公网部署请把 tls 设为 true 并配置 tls_ca（公网 CA 场景可不配 CA）",
				"event", "control_connect", "detail", "plaintext_auto")
		}
		if mode == config.TLSFalse && (c.cfg.Client.TLSCA != "" || c.cfg.Client.TLSInsecureSkipVerify) {
			c.ctrlLog.Warn("tls = false：已配置的 tls_ca / tls_insecure_skip_verify 被忽略，控制面按明文拨号",
				"event", "control_connect", "detail", "plaintext_forced")
		}
		dialer := &net.Dialer{Timeout: c.dialTimeout()}
		conn, err := dialer.DialContext(c.ctx, "tcp", c.cfg.Client.RelayAddr)
		if err != nil {
			return nil, false, fmt.Errorf("client: 连接 relay %s: %w", c.cfg.Client.RelayAddr, err)
		}
		return conn, false, nil
	}

	if c.cfg.Client.TLSInsecureSkipVerify {
		detail := "tls_insecure_skip_verify=true"
		if mode == config.TLSTrue {
			detail = "tls=true 时该开关是多余的，但它会关闭证书校验（仅建议测试环境）"
		}
		c.ctrlLog.Warn("已跳过 relay 证书校验（仅建议测试环境）", "event", "control_connect",
			"detail", detail)
	}
	tlsCfg, err := c.tlsConfig()
	if err != nil {
		return nil, false, err
	}
	dialer := &net.Dialer{Timeout: c.dialTimeout()}
	conn, err := c.dialTLS(dialer, tlsCfg)
	if err != nil {
		return nil, false, err
	}
	return conn, true, nil
}

// tlsConfig 按 tls_ca / tls_insecure_skip_verify 构造控制面 TLS 配置。
//
// tls_ca 为空时 RootCAs 保持 nil，表示使用系统根证书（tls=true 场景的标准校验路径）。
// CA 解析规则由 config.ClientTLSConfig 统一实现（与 agent / 数据面共用）。
func (c *client) tlsConfig() (*tls.Config, error) {
	cfg, err := config.ClientTLSConfig(c.cfg.Client.TLSCA, c.cfg.Client.TLSInsecureSkipVerify, c.host)
	if err != nil {
		return nil, permanent(fmt.Errorf("client: %w", err))
	}
	return cfg, nil
}

// dataTLSConfig 构造数据面 TLS 配置（与隧道共用 CA / insecure / SNI 规则）。
func (c *client) dataTLSConfig() (*tls.Config, error) {
	cfg, err := config.ClientTLSConfig(c.cfg.Client.TLSCA, c.cfg.Client.TLSInsecureSkipVerify, c.host)
	if err != nil {
		return nil, permanent(fmt.Errorf("client: 数据面 TLS: %w", err))
	}
	return cfg, nil
}

// logDataPlaneTLS 打印本次连接的数据面加密状态（SEC-T1 可见性要求）。
func (c *client) logDataPlaneTLS(useTLS bool, advertised *bool) {
	if useTLS {
		verify := "系统根证书"
		if strings.TrimSpace(c.cfg.Client.TLSCA) != "" {
			verify = "tls_ca=" + c.cfg.Client.TLSCA
		}
		if c.cfg.Client.TLSInsecureSkipVerify {
			verify = "跳过证书校验（tls_insecure_skip_verify = true，仅建议测试环境）"
		}
		c.ctrlLog.Info("数据面: TLS（TCP）", "event", "data_tls", "detail", "数据面: TLS（"+verify+"）")
		return
	}
	reason := "relay 未启用数据面 TLS"
	switch {
	case advertised == nil:
		reason = "旧 relay 未下发 data_tls（< v0.4.0）"
	case c.cfg.Client.DataTLS == config.TLSFalse:
		reason = "data_tls = false（本端强制明文）"
	}
	c.ctrlLog.Warn("数据面: 明文（业务数据与 pairing_token 不加密）",
		"event", "data_tls", "detail", "数据面: 明文（"+reason+"）")
}

// dialTLS 先建立 TCP 再在同一连接上完成 TLS 握手（带超时）。
func (c *client) dialTLS(dialer *net.Dialer, tlsCfg *tls.Config) (net.Conn, error) {
	raw, err := dialer.DialContext(c.ctx, "tcp", c.cfg.Client.RelayAddr)
	if err != nil {
		return nil, fmt.Errorf("client: 连接 relay %s: %w", c.cfg.Client.RelayAddr, err)
	}
	tlsConn := tls.Client(raw, tlsCfg)
	hctx, cancel := context.WithTimeout(c.ctx, c.dialTimeout())
	defer cancel()
	if err := tlsConn.HandshakeContext(hctx); err != nil {
		_ = raw.Close()
		// TLS 握手失败几乎总是配置不匹配（relay 没开 TLS / CA 不对），重连无意义。
		// 提示里给出可执行排查方向：relay 的 tls、本端 tls / tls_ca、是否误配 tls=false。
		return nil, permanent(fmt.Errorf(
			"client: TLS 握手失败（请检查 relay 是否启用 TLS，以及本端 tls / tls_ca 是否与 relay 证书匹配）: %w", err))
	}
	return tlsConn, nil
}

// writeControl 向当前控制连接串行写一帧（带写超时，避免对端不读导致永久阻塞）。
func (c *client) writeControl(typ protocol.MessageType, payload any) error {
	conn := c.controlConn()
	if conn == nil {
		return errControlClosed
	}
	return c.writeControlTo(conn, typ, payload)
}

// writeControlTo 向指定控制连接串行写一帧。
//
// 串行化使用 ctrlMu：同一条连接上的帧必须串行写；不同连接（重连前后）不会并发写，
// 因为旧连接的心跳在重连前已停止。
func (c *client) writeControlTo(conn net.Conn, typ protocol.MessageType, payload any) error {
	if conn == nil {
		return errControlClosed
	}
	c.ctrlMu.Lock()
	defer c.ctrlMu.Unlock()
	if c.ctrl != conn {
		// 连接已被替换/关闭：拒绝写到旧连接上。
		return errControlClosed
	}
	if d := c.dialTimeout(); d > 0 {
		_ = conn.SetWriteDeadline(time.Now().Add(d))
	}
	err := protocol.WriteFrame(conn, typ, payload)
	if err == nil {
		_ = conn.SetWriteDeadline(time.Time{})
	}
	return err
}

// readControlFrame 在握手阶段按 deadline 从指定连接读取一帧并解码。
func (c *client) readControlFrame(conn net.Conn, d time.Duration) (any, error) {
	if d > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(d))
	} else {
		_ = conn.SetReadDeadline(time.Time{})
	}
	typ, payload, err := protocol.ReadFrameWithLimit(conn, protocol.NetworkPayloadLimit)
	if err != nil {
		return nil, err
	}
	return protocol.Decode(typ, payload)
}

// controlLoop 是控制面读循环：唯一读控制连接的 goroutine，按消息类型分发。
//
// 返回 nil 表示 relay 停机（MsgServerShutdown）或本端主动关闭；返回 errKicked
// （包装后）表示被踢出；其它错误代表连接断开，由 supervise 决定是否重连。
func (c *client) controlLoop(conn net.Conn) error {
	for {
		// 网络入口上限 64 KiB（SEC-D1 / P0-1）：服务列表远小于此值。
		typ, payload, err := protocol.ReadFrameWithLimit(conn, protocol.NetworkPayloadLimit)
		if err != nil {
			if c.ctx.Err() != nil || errors.Is(err, net.ErrClosed) || c.controlConn() != conn {
				return nil
			}
			c.ctrlLog.Warn("控制连接已断开", "event", "control_close", "err", err)
			return fmt.Errorf("client: 控制连接断开: %w", err)
		}
		msg, err := protocol.Decode(typ, payload)
		if err != nil {
			// 未知消息类型按前向兼容忽略（docs/protocol.md §2）。
			c.ctrlLog.Warn("忽略无法解析的控制面消息", "event", "control_message_invalid", "err", err)
			continue
		}
		switch m := msg.(type) {
		case *protocol.PairResult:
			// 数据面绑定可能阻塞（拨号/等 BindOK），放到独立 goroutine，
			// 保证读循环能持续处理其它连接的结果与心跳。
			c.tasks.Go(func() { c.handlePairResult(m) })
		case *protocol.Error:
			c.handleControlError(m)
		case *protocol.ServiceEvent:
			c.applyServiceEvent(m)
		case *protocol.ServiceList:
			// relay 主动发起的**全量重同步**：订阅事件出现 revision 断档
			// （推送队列满导致丢弃、或多个写入者并发 dispatch 导致乱序）时，
			// relay 不再发单条 ServiceEvent，而是直接下发完整列表。
			before, _ := c.servicesView()
			c.applyServiceList(m)
			c.ctrlLog.Info("服务列表已全量重同步", "event", "service_list",
				"detail", fmt.Sprintf("before=%d after=%d revision=%d udp_mode=%s",
					len(before), len(m.Services), m.Revision, m.UDPMode))
		case *protocol.Kicked:
			c.ctrlLog.Warn("被 relay 踢出，client 退出（不重连）", "event", "kicked", "detail", logx.CleanN(m.Reason, logx.MaxAttrLen))
			return fmt.Errorf("%w: %s", errKicked, logx.CleanN(m.Reason, logx.MaxAttrLen))
		case *protocol.ServerShutdown:
			c.ctrlLog.Info("relay 即将停机，client 退出", "event", "server_shutdown", "detail", logx.CleanN(m.Reason, logx.MaxAttrLen))
			return nil
		case *protocol.Heartbeat:
			if err := c.writeControlTo(conn, protocol.MsgHeartbeatAck, &protocol.HeartbeatAck{}); err != nil && c.ctx.Err() == nil {
				c.ctrlLog.Debug("回复心跳失败", "event", "heartbeat", "err", err)
			}
		case *protocol.HeartbeatAck:
			// 控制面任何消息都代表对端存活，无需额外处理。
		case *protocol.HelloOK:
			c.ctrlLog.Debug("忽略重复的 hello_ok", "event", "hello_ok")
		default:
			c.ctrlLog.Debug("忽略非预期控制面消息", "event", "control_message", "detail", typ.String())
		}
	}
}

// heartbeatLoop 在给定控制连接上按协商间隔发送 MsgHeartbeat，ctx 取消即退出。
//
// 心跳与连接同生命周期：重连后由 serveControl 为新连接重建心跳。
func (c *client) heartbeatLoop(ctx context.Context, conn net.Conn) {
	interval := c.negotiated().heartbeat
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.writeControlTo(conn, protocol.MsgHeartbeat, &protocol.Heartbeat{}); err != nil {
				if c.ctx.Err() != nil || ctx.Err() != nil {
					return
				}
				c.ctrlLog.Warn("发送心跳失败", "event", "heartbeat", "err", err)
			}
		}
	}
}

// handlePairResult 处理 MsgPairResult：按配对键里的 client_conn_id 找到本地连接/会话，
// 然后选择数据面（tcp / udp）。
func (c *client) handlePairResult(pr *protocol.PairResult) {
	key, err := pairing.ParsePairingKey(pr.PairingKey)
	if err != nil {
		c.log.Warn("配对结果中的配对键非法，忽略", "event", "pair_result", "err", err)
		return
	}
	if pr.Role != "" && pr.Role != protocol.RoleClient {
		c.log.Warn("配对结果的 role 非预期，按 client 处理", "event", "pair_result",
			"pairing_key", pr.PairingKey, "detail", "role="+pr.Role)
	}

	if s := c.tcpSession(key.ClientConnID); s != nil {
		if s.svcID != key.ServiceID {
			c.log.Warn("配对结果的服务ID 与本地连接不一致，关闭连接", "event", "pair_result",
				"service_id", s.svcID, "pairing_key", pr.PairingKey)
			c.closeTCPSession(s, "pair_service_mismatch", nil)
			return
		}
		if !s.resolve(key, pr.Proto) {
			// 已超时/已关闭：直接丢弃，不再转发。
			c.log.Debug("配对结果到达时本地连接已结束", "event", "pair_result", "pairing_key", pr.PairingKey)
			return
		}
		c.log.Info("收到配对结果", "event", "pair_result", "service_id", key.ServiceID,
			"pairing_key", key.String(), "detail", "proto="+pr.Proto)
		if pr.Proto != "tcp" {
			c.log.Warn("本地是 TCP 监听但 relay 返回非 tcp 数据面，关闭连接", "event", "pair_result",
				"service_id", key.ServiceID, "pairing_key", key.String(), "detail", "proto="+pr.Proto)
			c.closeTCPSession(s, "pair_proto_mismatch", nil)
			return
		}
		c.tasks.Go(func() { c.forwardTCP(s, key, pr.PairingToken) })
		return
	}

	if s := c.udpSessionByConnID(key.ClientConnID); s != nil {
		if s.svcID != key.ServiceID {
			c.log.Warn("配对结果的服务ID 与本地 UDP 会话不一致，关闭会话", "event", "pair_result",
				"service_id", s.svcID, "pairing_key", pr.PairingKey)
			c.closeUDPSession(s, "pair_service_mismatch", nil)
			return
		}
		if !s.resolve(key, pr.Proto) {
			c.log.Debug("配对结果到达时本地 UDP 会话已结束", "event", "pair_result", "pairing_key", pr.PairingKey)
			return
		}
		c.log.Info("收到配对结果", "event", "pair_result", "service_id", key.ServiceID,
			"pairing_key", key.String(), "detail", "proto="+pr.Proto)
		if pr.Proto != "udp" {
			c.log.Warn("本地是 UDP 监听但 relay 返回非 udp 数据面，关闭会话", "event", "pair_result",
				"service_id", key.ServiceID, "pairing_key", key.String(), "detail", "proto="+pr.Proto)
			c.closeUDPSession(s, "pair_proto_mismatch", nil)
			return
		}
		c.tasks.Go(func() { c.forwardUDP(s, key, pr.PairingToken) })
		return
	}

	c.log.Warn("收到未知连接的配对结果（本地连接可能已超时关闭）", "event", "pair_result",
		"pairing_key", pr.PairingKey)
}

// handleControlError 处理控制面的 MsgError。
//
// 匹配规则（docs/protocol.md §4.12）：
//  1. `e.ClientConnID` 非空 → 按 client_conn_id **精确**关闭对应本地连接/会话
//     （新 relay 的行为；避免并发接入时错误归属到别的连接）；
//  2. 该字段为空（旧 relay）或匹配不到仍在等待的连接 → 回退到"最早进入等待状态的
//     本地连接"近似匹配，保持与旧 relay 的向后兼容。
func (c *client) handleControlError(e *protocol.Error) {
	sess := c.waitingSessionByConnID(e.ClientConnID)
	match := "client_conn_id"
	if sess == nil {
		sess = c.oldestWaitingSession()
		match = "fifo_fallback"
	}
	if sess == nil {
		c.log.Warn("收到没有对应本地连接的 relay 错误，忽略", "event", "pair_fail", "err", e)
		return
	}
	event := "pair_fail"
	if e.Code == protocol.ErrCodePairTimeout {
		event = "pair_timeout"
	}
	c.log.Warn("relay 拒绝了本次接入，关闭对应本地连接（不影响其它连接）", "event", event,
		"service_id", sess.serviceID(), "pairing_key", sess.displayKey(),
		"detail", "match="+match, "err", e)
	switch s := sess.(type) {
	case *tcpSession:
		c.closeTCPSession(s, "relay_error", e)
	case *udpSession:
		c.closeUDPSession(s, "relay_error", e)
	}
}

// waitingSessionByConnID 按 client_conn_id 精确查找仍在等待/建立配对的本地连接。
//
// 已经进入转发或已关闭的连接不匹配（错误帧不应杀掉已建立的转发）。
func (c *client) waitingSessionByConnID(connID string) localSession {
	if connID == "" {
		return nil
	}
	if s := c.tcpSession(connID); s != nil && s.isWaiting() {
		return s
	}
	if s := c.udpSessionByConnID(connID); s != nil && s.isWaiting() {
		return s
	}
	return nil
}

// resolveUDPMode 归一化 relay 下发的 udp_mode；未知/缺失回退 native 并 WARN。
func (c *client) resolveUDPMode(raw, source string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case protocol.UDPModeNative:
		return protocol.UDPModeNative
	case protocol.UDPModeTCPTunnel:
		return protocol.UDPModeTCPTunnel
	case "":
		c.ctrlLog.Warn("relay 未下发 udp_mode，回退 native", "event", "hello_ok",
			"detail", "source="+source)
		return protocol.UDPModeNative
	default:
		c.ctrlLog.Warn("relay 下发了未知 udp_mode，回退 native", "event", "hello_ok",
			"detail", fmt.Sprintf("source=%s udp_mode=%s", source, raw))
		return protocol.UDPModeNative
	}
}

// negotiateHeartbeat 取 min(本地 heartbeat, relay 建议值)，并避免 0/负值造成忙循环。
func negotiateHeartbeat(local time.Duration, suggestedSeconds int) time.Duration {
	hb := local
	if suggestedSeconds > 0 {
		suggested := time.Duration(suggestedSeconds) * time.Second
		if hb <= 0 || suggested < hb {
			hb = suggested
		}
	}
	if hb < time.Second {
		hb = time.Second
	}
	return hb
}
