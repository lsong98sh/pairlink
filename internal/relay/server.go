package relay

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lsong98sh/pairlink/internal/admin"
	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/logx"
	"github.com/lsong98sh/pairlink/internal/pairing"
	"github.com/lsong98sh/pairlink/internal/protocol"
	"github.com/lsong98sh/pairlink/internal/registry"
	"github.com/lsong98sh/pairlink/internal/token"
	"github.com/lsong98sh/pairlink/internal/udpdtls"
	"github.com/lsong98sh/pairlink/internal/version"
)

// server 是 relay 进程内的全部共享状态。
//
// 并发约定：
//   - services / agents / pairs / tokens / udpTable 各自内部有锁；
//   - waits / active / agentConns / dataConns 由 server 自己的锁保护；
//   - 监听器与连接表只在启动/停机时修改，正常运行期间只读。
type server struct {
	cfg *config.Config
	log *slog.Logger

	// udpMode 在启动时确定，运行期间不变：native 或 tcp_tunnel。
	udpMode string

	// tunnelTLS / dataTLS 在启动时由三态 tls / data_tls 与证书材料确定，
	// 运行期间不变；tlsCfg 为两个监听器共用（证书与 MinVersion 完全一致）。
	tunnelTLS bool
	dataTLS   bool
	tlsCfg    *tls.Config
	// serviceAllow 是 [relay] service_allow 白名单集合（空表示不限制，SEC-A2）。
	serviceAllow map[string]struct{}

	// 阶段 7：管理页可在运行期修改的三个超时（纳秒，atomic）。
	//
	// pair_timeout 由 PairingTable 自己持有（SetTimeout），token_ttl 由 token.Store
	// 持有（SetTTL），不在这里重复存储。
	idleTimeout       atomic.Int64
	heartbeatTimeout  atomic.Int64
	udpSessionTimeout atomic.Int64
	// carrierTimeout 是承载连接单帧读超时（SEC-M4），随 udp_session_timeout 联动。
	carrierTimeout atomic.Int64

	services *registry.ServiceRegistry
	agents   *registry.AgentRegistry
	pairs    *pairing.PairingTable
	tokens   *token.Store
	udpTable *registry.UDPTable // 仅 native 模式
	carriers *carrierTable      // UDP over TCP 承载会话表（两种模式都存在，native 下通常为空）

	tunnel  *TunnelListener
	tcpData *TCPDataListener
	udpData *UDPDataListener
	admin   *admin.Server

	tunnelAddr string
	tcpAddr    string
	tcpPort    int
	udpAddr    string
	udpPort    int

	// ready 在监听端口全部就绪后关闭；started 在管理页与 ticker 启动后关闭。
	ready   chan struct{}
	started chan struct{}

	ctx    context.Context
	cancel context.CancelFunc

	wg sync.WaitGroup

	dataConns *connSet

	// conns 限制隧道控制面与 TCP 数据面的并发连接数（SEC-D2）。
	conns *connLimiter

	waitMu sync.Mutex
	waits  map[pairing.PairingKey]*pairWait

	activeMu sync.Mutex
	active   map[pairing.PairingKey]activePair

	agentConnMu sync.Mutex
	agentConns  map[string]map[net.Conn]struct{}

	closing      atomic.Bool
	shutdownOnce sync.Once

	errMu  sync.Mutex
	runErr error
}

// serve 是 relay 的主生命周期：监听 → 启动循环 → 等待 ctx → 清理 → 返回。
func (s *server) serve(parent context.Context) error {
	s.ctx, s.cancel = context.WithCancel(parent)
	defer s.cancel()

	s.log.Info("relay 启动",
		"event", "start",
		"udp_mode", s.udpMode,
		"tunnel_listen", s.cfg.Relay.TunnelListen,
		"tcp_listen", s.cfg.Relay.TCPListen,
		"udp_listen", s.cfg.Relay.UDPListen,
	)
	if s.udpMode == protocol.UDPModeNative {
		if s.cfg.Relay.UDPDTLS {
			s.log.Info("原生 UDP 数据面: DTLS-PSK（DTLS 1.2 / AES-128-GCM）",
				"event", "start", "udp_dtls", true,
				"detail", "PSK = HKDF-SHA256(auth_token, info="+udpdtls.InfoPrefix+"pairing_key)")
		} else {
			s.log.Warn("udp_dtls = false：原生 UDP 数据面按 v0.4.0 明文运行，业务数据与 pairing_token 可被链路上的人读取（仅用于混合版本过渡）",
				"event", "udp_dtls_disabled", "udp_dtls", false)
		}
	}
	if strings.TrimSpace(s.cfg.Relay.AuthToken) == "" {
		s.log.Warn("auth_token 为空：仅允许回环来源接入隧道（非回环来源将回 auth_failed），仅供本机调试",
			"event", "config_warning", "detail", "auth_token 未配置")
	}
	if len(s.serviceAllow) == 0 {
		s.log.Warn("未配置 service_allow：任何持有 auth_token 的接入者都可以注册任意服务名（SEC-A2），多租户/公网部署建议显式列出允许的服务名",
			"event", "config_warning", "detail", "service_allow 未配置")
	}
	if s.cfg.Relay.MaxConns > 0 || s.cfg.Relay.MaxConnsPerIP > 0 {
		s.log.Info("连接上限已启用",
			"event", "start",
			"max_conns", s.cfg.Relay.MaxConns,
			"max_conns_per_ip", s.cfg.Relay.MaxConnsPerIP,
		)
	}

	// 0) 先按 tls / data_tls 决定两个监听器是否 TLS，并加载（共用）证书。
	if err := s.prepareTLS(); err != nil {
		return err
	}

	// 1) 先监听 TCP 数据面（data_tls 生效时为 TLS）。
	tcpLn, err := s.listenTCPData()
	if err != nil {
		return err
	}
	s.tcpData = &TCPDataListener{srv: s, ln: tcpLn}
	s.tcpAddr = tcpLn.Addr().String()
	s.tcpPort = portOf(tcpLn.Addr())

	// 2) native 模式再监听 UDP 数据面。
	if s.udpMode == protocol.UDPModeNative {
		udpAddr, resolveErr := net.ResolveUDPAddr("udp", s.cfg.Relay.UDPListen)
		if resolveErr != nil {
			_ = tcpLn.Close()
			return fmt.Errorf("relay: 解析 UDP 数据面地址 %s: %w", s.cfg.Relay.UDPListen, resolveErr)
		}
		udpConn, listenErr := net.ListenUDP("udp", udpAddr)
		if listenErr != nil {
			_ = tcpLn.Close()
			return fmt.Errorf("relay: 监听 UDP 数据面 %s: %w", s.cfg.Relay.UDPListen, listenErr)
		}
		s.udpData = &UDPDataListener{srv: s, conn: udpConn}
		s.udpAddr = udpConn.LocalAddr().String()
		s.udpPort = portOf(udpConn.LocalAddr())
	}

	// 3) 最后监听控制面（隧道），此时数据面已经就绪。
	tunnelLn, err := s.listenTunnel()
	if err != nil {
		s.closeDataListeners()
		return err
	}
	s.tunnel = &TunnelListener{
		srv:    s,
		ln:     tunnelLn,
		conns:  make(map[*tunnelConn]struct{}),
		agents: make(map[string]*tunnelConn),
	}
	s.tunnelAddr = tunnelLn.Addr().String()

	// 监听全部就绪，允许测试/调用方读取实际地址。
	close(s.ready)

	// 4) 启动数据面循环，再启动隧道 accept 循环。
	s.wg.Add(1)
	go s.tcpData.acceptLoop()
	if s.udpData != nil {
		s.wg.Add(1)
		go s.udpData.loop()
	}
	s.wg.Add(1)
	go s.tunnel.acceptLoop()

	// 5) 管理页（可选）。
	if err := s.startAdmin(); err != nil {
		s.shutdown()
		s.wg.Wait()
		return err
	}

	// 6) 后台 ticker。
	s.wg.Add(1)
	go s.runTickers()
	close(s.started)

	// 7) 等待 ctx 取消或运行期不可恢复错误。
	<-s.ctx.Done()
	s.shutdown()
	s.wg.Wait()

	if err := s.firstError(); err != nil && parent.Err() == nil {
		return err
	}
	return nil
}

// prepareTLS 按三态 tls / data_tls 决定隧道与数据面是否 TLS，并加载共用证书。
//
// 决策顺序（docs/architecture.md §7.6 / §7.11）：
//   - 隧道 tls = true：必须 TLS；tls_cert/tls_key 缺失时返回错误（Validate 已提前拦截）；
//   - 隧道 tls = false：一律明文；若配了证书则 WARN（材料被忽略）；
//   - 隧道 tls = auto（默认）：证书+私钥均非空 → TLS；否则明文 + WARN（既有行为）；
//   - 数据面 data_tls = true：强制 TLS，证书与隧道共用；
//   - 数据面 data_tls = false：强制明文；
//   - 数据面 data_tls = auto：跟随隧道判定（隧道 TLS 时数据面也 TLS）。
//
// 证书只加载一次，隧道与数据面共享同一个 *tls.Config（MinVersion 均为 1.2）。
func (s *server) prepareTLS() error {
	certPath := strings.TrimSpace(s.cfg.Relay.TLSCert)
	keyPath := strings.TrimSpace(s.cfg.Relay.TLSKey)
	complete := certPath != "" && keyPath != ""
	mode := s.cfg.Relay.TLS

	switch mode {
	case config.TLSFalse:
		if certPath != "" || keyPath != "" {
			s.log.Warn("已配置 tls_cert / tls_key 但 tls = false，隧道按明文监听（证书被忽略）",
				"event", "config_warning", "detail", "tls=false")
		} else {
			s.log.Warn("tls = false，隧道以明文运行（不建议在公网使用）",
				"event", "config_warning", "detail", "tls=false")
		}
	case config.TLSTrue:
		if !complete {
			return fmt.Errorf("relay: tls = true 但缺少 tls_cert / tls_key（强制 TLS 必须同时配置证书与私钥）")
		}
	default: // auto
		if !complete {
			if certPath != "" || keyPath != "" {
				s.log.Warn("tls_cert 与 tls_key 必须成对配置，隧道按明文运行",
					"event", "config_warning", "detail", "TLS 证书配置不完整")
			} else {
				s.log.Warn("未配置 TLS，隧道以明文运行（不建议在公网使用）",
					"event", "config_warning", "detail", "隧道明文")
			}
		}
	}
	s.tunnelTLS = mode.ListenTLS(certPath, keyPath)

	dataMode := s.cfg.Relay.DataTLS
	if dataMode == config.TLSTrue && !complete {
		return fmt.Errorf("relay: data_tls = true 但缺少 tls_cert / tls_key（数据面 TLS 与隧道共用证书，必须同时配置证书与私钥）")
	}
	s.dataTLS = dataMode.RelayDataTLS(s.tunnelTLS)
	if s.tunnelTLS || s.dataTLS {
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return fmt.Errorf("relay: 加载 TLS 证书 %s / %s: %w", certPath, keyPath, err)
		}
		s.tlsCfg = &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
		}
	}

	// 数据面最终状态的可见性：启动日志必须能一眼看出是否加密（SEC-T1）。
	if s.dataTLS {
		s.log.Info("数据面: TLS", "event", "start",
			"detail", "数据面: TLS(证书="+certPath+", 类型=TCP)",
			"data_tls", true, "tls_min_version", "1.2")
	} else {
		detail := "数据面: 明文（业务数据与 pairing_token 在链路上可被读取）"
		if dataMode == config.TLSFalse {
			detail = "数据面: 明文（data_tls = false 显式关闭，业务数据与 pairing_token 不加密）"
		} else if s.tunnelTLS {
			detail = "数据面: 明文（data_tls = false 覆盖了隧道 TLS；只有控制面加密）"
		}
		s.log.Warn(detail, "event", "config_warning", "detail", detail, "data_tls", false)
	}
	if s.dataTLS && s.udpMode == protocol.UDPModeNative && !s.cfg.Relay.UDPDTLS {
		s.log.Warn("原生 UDP 数据面仍为明文（udp_dtls = false，data_tls 不覆盖原生 UDP）：如需加密请设置 udp_dtls = true 改用 DTLS-PSK，或移除 udp_listen 改用 udp_mode = tcp_tunnel（承载走已加密的 TCP 数据面）",
			"event", "config_warning", "detail", "data_tls 不覆盖原生 UDP")
	}
	return nil
}

// listenTCPData 按 data_tls 决定以 TLS 还是明文监听 TCP 数据面。
//
// tls.Listener 在 Accept 时不握手，握手发生在首帧读取；明文客户端连 TLS 端口
// 会在 handleDataConn 的报到读帧处失败，并记录 data_plane_tls_handshake_fail。
func (s *server) listenTCPData() (net.Listener, error) {
	if s.dataTLS {
		ln, err := tls.Listen("tcp", s.cfg.Relay.TCPListen, s.tlsCfg)
		if err != nil {
			return nil, fmt.Errorf("relay: 监听 TCP 数据面（TLS）%s: %w", s.cfg.Relay.TCPListen, err)
		}
		s.log.Info("TCP 数据面使用 TLS 监听", "event", "start",
			"tcp_listen", ln.Addr().String(), "tls_min_version", "1.2")
		return ln, nil
	}
	ln, err := net.Listen("tcp", s.cfg.Relay.TCPListen)
	if err != nil {
		return nil, fmt.Errorf("relay: 监听 TCP 数据面 %s: %w", s.cfg.Relay.TCPListen, err)
	}
	s.log.Info("TCP 数据面以明文监听", "event", "start", "tcp_listen", ln.Addr().String())
	return ln, nil
}

// listenTunnel 按 prepareTLS 的结果监听隧道端口。
func (s *server) listenTunnel() (net.Listener, error) {
	if !s.tunnelTLS {
		return s.listenPlainTunnel()
	}
	ln, err := tls.Listen("tcp", s.cfg.Relay.TunnelListen, s.tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("relay: 监听隧道（TLS）%s: %w", s.cfg.Relay.TunnelListen, err)
	}
	s.log.Info("隧道使用 TLS 监听", "event", "start", "tunnel_listen", ln.Addr().String(), "tls_min_version", "1.2")
	return ln, nil
}

// listenPlainTunnel 以明文监听隧道端口（tls=false 或 auto 且证书不完整）。
func (s *server) listenPlainTunnel() (net.Listener, error) {
	ln, err := net.Listen("tcp", s.cfg.Relay.TunnelListen)
	if err != nil {
		return nil, fmt.Errorf("relay: 监听隧道 %s: %w", s.cfg.Relay.TunnelListen, err)
	}
	s.log.Info("隧道以明文监听", "event", "start", "tunnel_listen", ln.Addr().String())
	return ln, nil
}

// closeDataListeners 在启动失败时关闭已经打开的数据面监听。
func (s *server) closeDataListeners() {
	if s.tcpData != nil {
		_ = s.tcpData.ln.Close()
	}
	if s.udpData != nil {
		s.udpData.close()
	}
}

// startAdmin 在 manage_listen 非空时启动管理页。
func (s *server) startAdmin() error {
	if strings.TrimSpace(s.cfg.Common.ManageListen) == "" {
		return nil
	}
	opts := admin.Options{
		Title:   "PairLink relay 管理页",
		Mode:    "relay",
		Version: version.Version,
		Logger:  logx.Component(s.log, "admin"),
		Pprof:   s.cfg.Common.Pprof,
		Actions: []admin.Action{
			{
				Name:    "kick",
				Title:   "踢出",
				Method:  "POST",
				Handler: s.handleKickAction,
			},
			{
				Name:    "relay-config",
				Title:   "修改 relay 参数",
				Method:  "POST",
				Handler: s.handleConfigAction,
			},
		},
	}
	srv := admin.New(s.cfg.Common.ManageListen, opts)
	srv.AddPage(admin.Page{
		Path:   "/relay",
		Title:  "relay 状态",
		Render: s.renderAdmin,
	})
	// 必须在 Start 之前发布 s.admin：管理页开始服务后，页面渲染/kickForm 会
	// 读取 s.admin 来生成 CSRF 输入，晚于此写入会与 HTTP handler 并发读写。
	s.admin = srv
	if err := srv.Start(s.ctx); err != nil {
		s.admin = nil
		return fmt.Errorf("relay: 启动管理页 %s: %w", s.cfg.Common.ManageListen, err)
	}
	return nil
}

// pairSweepInterval 返回待配对表的扫描周期（pair_timeout/2，下限 minSweepInterval）。
//
// P3-3：每次 tick 都重新计算，运行期把 pair_timeout 改小后扫描周期会跟着变，
// 而不是一直沿用启动时的值（旧实现只在启动时算一次）。
func (s *server) pairSweepInterval() time.Duration {
	d := s.pairTimeout() / 2
	if d < minSweepInterval {
		return minSweepInterval
	}
	return d
}

// runTickers 是唯一的后台清理循环，所有超时清理都由它驱动。
func (s *server) runTickers() {
	defer s.wg.Done()

	pairInterval := s.pairSweepInterval()
	pairTicker := time.NewTicker(pairInterval)
	tokenTicker := time.NewTicker(time.Second)
	udpTicker := time.NewTicker(time.Second)
	heartbeatTicker := time.NewTicker(heartbeatCheckInterval)
	defer func() {
		pairTicker.Stop()
		tokenTicker.Stop()
		udpTicker.Stop()
		heartbeatTicker.Stop()
	}()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-pairTicker.C:
			if want := s.pairSweepInterval(); want != pairInterval {
				pairTicker.Reset(want)
				pairInterval = want
			}
			s.pairs.Sweep(time.Now())
		case <-tokenTicker.C:
			s.tokens.Sweep()
		case <-udpTicker.C:
			s.sweepUDP()
		case <-heartbeatTicker.C:
			s.checkHeartbeats()
		}
	}
}

// sweepUDP 清理原生 UDP 与 UDP over TCP 的超时会话。
func (s *server) sweepUDP() {
	now := time.Now()

	if s.udpMode == protocol.UDPModeNative && s.udpTable != nil {
		// 半会话用 pair_timeout（从创建时间算起）。
		for _, entry := range s.udpTable.Snapshot() {
			if entry.Complete {
				continue
			}
			if now.Sub(entry.CreatedAt) < s.pairTimeout() {
				continue
			}
			s.udpTable.Remove(entry.Key)
			s.log.Warn("UDP 半会话配对超时",
				"event", "pair_timeout",
				"pairing_key", entry.Key.String(),
				"service_id", entry.Key.ServiceID,
			)
		}
		// 完整会话用 udp_session_timeout（从最后活动时间算起）。
		for _, key := range s.udpTable.Sweep(now, s.udpSessionTimeoutValue()) {
			s.log.Info("UDP 会话空闲清理",
				"event", "udp_session_close",
				"pairing_key", key.String(),
				"service_id", key.ServiceID,
			)
		}
	}

	half, full := s.carriers.sweep(now, s.pairTimeout(), s.udpSessionTimeoutValue())
	for _, key := range half {
		s.log.Warn("UDP over TCP 半会话配对超时",
			"event", "pair_timeout",
			"pairing_key", key.String(),
			"service_id", key.ServiceID,
		)
	}
	for _, key := range full {
		s.log.Info("UDP over TCP 会话空闲清理",
			"event", "udp_session_close",
			"pairing_key", key.String(),
			"service_id", key.ServiceID,
		)
	}
}

// checkHeartbeats 依据 LastSeen 判死控制面连接（周期 5s）。
func (s *server) checkHeartbeats() {
	timeout := s.heartbeatTimeoutValue()
	if timeout <= 0 {
		return
	}
	now := time.Now()

	for _, agent := range s.agents.List() {
		if now.Sub(agent.LastSeen) <= timeout {
			continue
		}
		s.log.Warn("agent 心跳超时，关闭控制连接",
			"event", "heartbeat_timeout",
			"agent_id", agent.ID,
			"remote", agent.RemoteAddr,
			"last_seen", agent.LastSeen.Format(time.RFC3339),
		)
		if tc := s.tunnel.agentConn(agent.ID); tc != nil {
			tc.close()
			continue
		}
		s.agentGone(agent.ID)
	}

	for _, tc := range s.tunnel.snapshot() {
		if !tc.ready.Load() {
			continue
		}
		if tc.role != protocol.RoleClient {
			continue
		}
		if now.Sub(tc.lastSeenTime()) <= timeout {
			continue
		}
		s.log.Warn("client 心跳超时，关闭控制连接",
			"event", "heartbeat_timeout",
			"client_id", tc.clientID,
			"remote", tc.remoteAddr(),
		)
		tc.close()
	}
}

// agentGone 清理一个 agent 的全部注册状态与未完成配对资源。
//
// 调用方已经（或即将）关闭 agent 的控制连接；本函数只做注册表与资源清理，
// 不直接关闭控制连接，避免与既有清理路径重复。
func (s *server) agentGone(agentID string) {
	if agentID == "" {
		return
	}

	services := s.services.ListByAgent(agentID)
	serviceSet := make(map[string]struct{}, len(services))
	for _, svc := range services {
		serviceSet[svc.ID] = struct{}{}
	}

	removed := s.services.UnregisterAgent(agentID)
	_ = s.agents.Remove(agentID)
	for _, id := range removed {
		s.log.Info("服务已注销",
			"event", "service_unregister",
			"agent_id", agentID,
			"service_id", id,
		)
	}

	// 关闭该 agent 名下未完成配对的占位与数据连接。
	if len(serviceSet) > 0 {
		for _, snap := range s.pairs.Snapshot() {
			if snap.Completed {
				continue
			}
			if _, ok := serviceSet[snap.Key.ServiceID]; !ok {
				continue
			}
			// 先唤醒等待方（它负责关闭自己的连接），再移除表项。
			if w := s.lookupWait(snap.Key); w != nil {
				w.cancel()
			}
			s.pairs.Remove(snap.Key)
			s.log.Warn("agent 下线，关闭未完成配对",
				"event", "pair_timeout",
				"agent_id", agentID,
				"pairing_key", snap.Key.String(),
			)
		}
	}
	s.closeAgentDataConns(agentID)

	// 承载会话：删除归属于该 agent 的会话登记（不关闭承载 TCP 连接本身，
	// 承载连接的读循环会在对端断开后自行清理）。
	for _, key := range s.carriers.removeAgent(agentID) {
		s.log.Warn("agent 下线，移除承载会话",
			"event", "udp_session_close",
			"agent_id", agentID,
			"pairing_key", key.String(),
			"service_id", key.ServiceID,
		)
	}

	s.log.Info("agent 已下线",
		"event", "agent_gone",
		"agent_id", agentID,
		"services", len(removed),
	)
}

// closeAgentDataConns 关闭归属于某 agent 的数据面连接。
func (s *server) closeAgentDataConns(agentID string) {
	s.agentConnMu.Lock()
	conns := s.agentConns[agentID]
	delete(s.agentConns, agentID)
	s.agentConnMu.Unlock()
	for conn := range conns {
		_ = conn.Close()
	}
}

// trackAgentDataConn 记录一个 agent 侧数据连接，供 agent 下线时清理。
func (s *server) trackAgentDataConn(agentID string, conn net.Conn) {
	if agentID == "" || conn == nil {
		return
	}
	s.agentConnMu.Lock()
	defer s.agentConnMu.Unlock()
	m := s.agentConns[agentID]
	if m == nil {
		m = make(map[net.Conn]struct{})
		s.agentConns[agentID] = m
	}
	m[conn] = struct{}{}
}

// untrackAgentDataConn 移除 agent 侧数据连接记录。
func (s *server) untrackAgentDataConn(agentID string, conn net.Conn) {
	if agentID == "" || conn == nil {
		return
	}
	s.agentConnMu.Lock()
	defer s.agentConnMu.Unlock()
	m := s.agentConns[agentID]
	if m == nil {
		return
	}
	delete(m, conn)
	if len(m) == 0 {
		delete(s.agentConns, agentID)
	}
}

// agentIDForService 返回服务所属的 agent ID（服务不存在时为空）。
func (s *server) agentIDForService(serviceID string) string {
	entry, ok := s.services.Get(serviceID)
	if !ok {
		return ""
	}
	return entry.AgentID
}

// serviceInfo 把注册表条目转换为协议里的 ServiceInfo，并刷新在线状态。
func (s *server) serviceInfo(entry registry.ServiceEntry) protocol.ServiceInfo {
	online := entry.Online
	if online {
		agent, ok := s.agents.Get(entry.AgentID)
		if !ok {
			online = false
		} else if s.heartbeatTimeoutValue() > 0 && time.Since(agent.LastSeen) > s.heartbeatTimeoutValue() {
			online = false
		}
	}
	return protocol.ServiceInfo{
		ServiceID: entry.ID,
		Proto:     entry.Proto,
		AgentID:   entry.AgentID,
		Online:    online,
	}
}

// heartbeatSeconds 返回下发给 agent/client 的建议心跳间隔（秒）。
func (s *server) heartbeatSeconds() int {
	if s.heartbeatTimeoutValue() <= 0 {
		return 30
	}
	sec := int(s.heartbeatTimeoutValue() / (3 * time.Second))
	if sec < 1 {
		sec = 1
	}
	if sec > 60 {
		sec = 60
	}
	return sec
}

// shutdown 关闭全部监听、连接与后台资源；幂等。
func (s *server) shutdown() {
	s.shutdownOnce.Do(func() {
		s.closing.Store(true)
		s.log.Info("relay 开始停机", "event", "server_shutdown")

		// 先停监听，避免停机过程中还有新连接涌入。
		if s.tunnel != nil {
			s.tunnel.shutdown()
		}
		if s.tcpData != nil {
			_ = s.tcpData.ln.Close()
		}
		if s.udpData != nil {
			s.udpData.close()
		}

		// 关闭数据面/承载连接（关闭即触发各自读循环退出）。
		s.dataConns.closeAll()

		// 关闭管理页。
		if s.admin != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := s.admin.Shutdown(ctx); err != nil {
				s.log.Warn("管理页关闭不干净", "event", "admin_shutdown", "err", err)
			}
			cancel()
		}

		// 取消内部 ctx：转发看护、等待中的配对等全部退出。
		if s.cancel != nil {
			s.cancel()
		}
	})
}

// fail 记录运行期不可恢复错误并触发停机。
func (s *server) fail(err error) {
	if err == nil {
		return
	}
	s.errMu.Lock()
	if s.runErr == nil {
		s.runErr = err
	}
	s.errMu.Unlock()
	s.log.Error("relay 运行期错误", "event", "error", "err", err)
	if s.cancel != nil {
		s.cancel()
	}
}

// firstError 返回记录到的首个运行期错误。
func (s *server) firstError() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.runErr
}

// portOf 从 net.Addr 中提取端口。
func portOf(addr net.Addr) int {
	if addr == nil {
		return 0
	}
	switch a := addr.(type) {
	case *net.TCPAddr:
		return a.Port
	case *net.UDPAddr:
		return a.Port
	}
	_, portStr, err := net.SplitHostPort(addr.String())
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 0
	}
	return port
}

// connSet 是一个可整体关闭的 net.Conn 集合。
type connSet struct {
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
}

// newConnSet 创建空集合。
func newConnSet() *connSet {
	return &connSet{conns: make(map[net.Conn]struct{})}
}

// add 加入连接；集合已关闭时返回 false（调用方应立即关闭连接）。
func (c *connSet) add(conn net.Conn) bool {
	if conn == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	c.conns[conn] = struct{}{}
	return true
}

// remove 移除连接。
func (c *connSet) remove(conn net.Conn) {
	if conn == nil {
		return
	}
	c.mu.Lock()
	delete(c.conns, conn)
	c.mu.Unlock()
}

// closeAll 关闭并清空集合（幂等）。
func (c *connSet) closeAll() {
	c.mu.Lock()
	c.closed = true
	conns := c.conns
	c.conns = make(map[net.Conn]struct{})
	c.mu.Unlock()
	for conn := range conns {
		_ = conn.Close()
	}
}
