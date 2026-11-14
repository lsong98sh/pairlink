// Package agent 实现 PairLink 的 agent 模式（服务提供者侧）。
//
// 职责（docs/contract.md §2.4）：
//   - 用 [server] relay_addr 连接 relay 隧道端口（TLS 规则见 docs/architecture.md §7.6），
//     完成 MsgHello/MsgHelloOK 握手并注册 cfg.Server.All() 的全部服务；
//   - 维持控制长连接：心跳、处理 MsgIncoming / MsgHeartbeat / MsgError / MsgKicked /
//     MsgServerShutdown；
//   - 每个 MsgIncoming 起一个独立 goroutine 完成数据面报到，并在数据连接与本地目标之间
//     双向转发：proto=tcp 走 tcp_listen 的纯字节透传；proto=udp 按 relay 下发的 udp_mode
//     走原生 UDP（udp_listen）或复用 tcp_listen 的 UDP over TCP 隧道帧；
//   - manage_listen 非空时提供只读管理页；
//   - ctx 取消时关闭控制连接、全部数据/承载连接与本地 UDP 会话，等全部 goroutine 退出后返回 nil。
//
// 数据面是明文哑管道，不解析任何上层协议。控制连接意外断开时**不自动重连**（由
// systemd / Windows 服务恢复策略重启进程），这是第一版明确的设计选择。
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/lsong98sh/pairlink/internal/admin"
	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/logx"
	"github.com/lsong98sh/pairlink/internal/pairing"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// modeName 是日志 mode 字段与管理页展示使用的模式名。
const modeName = "agent"

const (
	// defaultHeartbeat 是未配置且 relay 未建议时的心跳间隔。
	defaultHeartbeat = 30 * time.Second
	// defaultDialTimeout 是连接 relay / 本地目标的默认超时。
	defaultDialTimeout = 10 * time.Second
	// defaultPairTimeout 是等待数据面报到应答（MsgBindOK）的默认超时。
	defaultPairTimeout = 10 * time.Second
	// defaultUDPSessionTimeout 是本地 UDP 会话空闲清理的默认值。
	defaultUDPSessionTimeout = 60 * time.Second
	// minSessionSweepInterval / maxSessionSweepInterval 限定空闲清理 ticker 的周期范围。
	minSessionSweepInterval = 50 * time.Millisecond
	maxSessionSweepInterval = 5 * time.Second
	// udpDatagramSize 是 UDP 读写缓冲大小（单个数据报上限，65535）。
	udpDatagramSize = protocol.MaxUDPDatagram
	// adminShutdownTimeout 是关停管理页的超时。
	adminShutdownTimeout = 3 * time.Second
)

// 本地记录的 UDP 数据面承载方式（日志与管理页展示用）。
const (
	// carrierNative 表示原生 UDP（[relay] udp_listen）。
	carrierNative = "native"
	// carrierTCP 表示 TCP 纯透传（protocol.CarrierTCP 的值）。
	carrierTCP = protocol.CarrierTCP
	// carrierUDPOverTCP 表示 UDP over TCP 承载连接（protocol.CarrierUDPOverTCP 的值）。
	carrierUDPOverTCP = protocol.CarrierUDPOverTCP
)

// errSessionExists 表示同一配对键已有活跃会话（重复 MsgIncoming）。
var errSessionExists = errors.New("agent: 该配对键已有活跃会话")

// registeredService 是一条本机已注册服务的快照（管理页展示用）。
type registeredService struct {
	// Service 是 ini 中声明的服务映射。
	Service config.ServerService
	// UDPMode 是注册成功时 relay 下发的承载模式。
	UDPMode string
}

// agent 是一次 agent 运行的实例。
//
// 并发约定：
//   - ctx/cancel、ctx 派生的连接、sessions、carrier 由各自内部锁保护或只在启动阶段写；
//   - ctrlMu 保护「控制连接状态」这一组会被管理页与心跳 goroutine 并发读写的字段；
//   - services / byService / dataAddr 在启动阶段（前台 goroutine）写完后只读，
//     后台 goroutine 都是在那之后创建的，因此不存在数据竞争。
type agent struct {
	cfg *config.Config
	log *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc

	relayHost string

	ctrl *controlConn

	ctrlMu       sync.Mutex
	agentID      string
	relayVersion int
	udpMode      string
	tcpDataPort  int
	udpDataPort  int
	heartbeat    time.Duration
	connectedAt  time.Time
	lastAck      time.Time

	// services 是已注册服务的快照，与下面的控制连接状态一起受 ctrlMu 保护
	// （注册发生在启动阶段，管理页与测试会并发读取）。
	services []registeredService

	tcpDataAddr string
	udpDataAddr string

	byService map[string]config.ServerService

	sessions *sessionTable
	carrier  *carrierManager

	adminSrv *admin.Server

	wg           sync.WaitGroup
	shutdownOnce sync.Once
}

// Run 启动 agent 模式，直到 ctx 取消或控制连接终止。
//
// 返回值语义（docs/contract.md §2.1）：
//   - ctx 取消且清理完成 → nil；
//   - 启动失败（拨号失败、握手失败、注册失败、管理页监听失败）→ 非 nil 错误；
//   - 运行中不可恢复（控制连接断开、被踢出）→ 非 nil 错误；收到 MsgServerShutdown → nil。
func Run(ctx context.Context, cfg *config.Config) error {
	if cfg == nil {
		return errors.New("agent: 配置为 nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	log := logx.New(logx.Config{
		Level:  cfg.Common.LogLevel,
		Format: cfg.Common.LogFormat,
		Mode:   modeName,
	})
	return newAgent(ctx, cfg, log).run()
}

// newAgent 构造 agent 实例（测试可直接调用后自行驱动 run）。
func newAgent(ctx context.Context, cfg *config.Config, log *slog.Logger) *agent {
	if log == nil {
		log = slog.Default()
	}
	actx, cancel := context.WithCancel(ctx)
	a := &agent{
		cfg:       cfg,
		log:       log,
		ctx:       actx,
		cancel:    cancel,
		sessions:  newSessionTable(),
		byService: make(map[string]config.ServerService),
	}
	for _, svc := range cfg.Server.All() {
		a.byService[svc.ID] = svc
	}
	a.carrier = newCarrierManager(a)
	return a
}

// run 完成启动、等待退出信号、执行关停。
func (a *agent) run() error {
	if err := a.connect(); err != nil {
		a.shutdown()
		return err
	}
	if err := a.startAdmin(); err != nil {
		a.shutdown()
		return err
	}
	a.startBackground()

	// 控制面读循环是唯一读取控制连接的 goroutine；它退出即代表控制面终止。
	a.wg.Add(1)
	resultCh := make(chan error, 1)
	go func() {
		defer a.wg.Done()
		resultCh <- a.controlLoop()
	}()

	a.log.Info("agent 已启动", "event", "start",
		"agent_id", a.agentIDSnapshot(),
		"detail", fmt.Sprintf("relay=%s services=%d udp_mode=%s", a.cfg.Server.RelayAddr, len(a.registeredServices()), a.udpModeSnapshot()),
	)

	var err error
	select {
	case <-a.ctx.Done():
	case err = <-resultCh:
	}
	a.shutdown()
	return err
}

// connect 建立控制连接、完成握手、注册全部服务并推导数据面地址。
func (a *agent) connect() error {
	host, _, err := splitRelayAddr(a.cfg.Server.RelayAddr)
	if err != nil {
		return fmt.Errorf("agent: [server] relay_addr 非法: %w", err)
	}
	a.relayHost = host

	tcfg, err := serverTLSConfig(&a.cfg.Server, host, a.log)
	if err != nil {
		return err
	}
	conn, err := a.dialRelay(tcfg)
	if err != nil {
		return fmt.Errorf("agent: 连接 relay %s: %w", a.cfg.Server.RelayAddr, err)
	}
	a.ctrl = newControlConn(conn)
	a.log.Info("已连接 relay 控制面", "event", "control_connect",
		"remote", conn.RemoteAddr().String(), "detail", fmt.Sprintf("tls=%t", tcfg != nil))

	if err := a.handshake(); err != nil {
		return err
	}
	if err := a.registerServices(); err != nil {
		return err
	}
	a.resolveDataAddrs()
	return nil
}

// shutdown 执行一次性关停：关连接 → 等 goroutine 返回 → 关管理页。
//
// 顺序遵循 docs/architecture.md §5：先关闭控制连接与全部数据/承载/本地 UDP socket，
// 让各 goroutine 的阻塞读写立即返回，再 wg.Wait 等它们退出，最后关闭管理页。
func (a *agent) shutdown() {
	a.shutdownOnce.Do(func() {
		// 先取消 ctx：数据面 forward.Bidirectional 的看护 goroutine 会关闭两端，
		// 心跳/清理 ticker 也会随 ctx.Done 退出。
		a.cancel()
		if a.ctrl != nil {
			a.ctrl.close()
		}
		a.sessions.closeAll()
		a.carrier.closeAll()
		a.wg.Wait()
		if a.adminSrv != nil {
			sctx, cancel := context.WithTimeout(context.Background(), adminShutdownTimeout)
			if err := a.adminSrv.Shutdown(sctx); err != nil {
				a.log.Warn("关闭管理页不干净", "event", "admin_shutdown", "err", err)
			}
			cancel()
		}
		a.log.Info("agent 已停止", "event", "shutdown",
			"agent_id", a.agentIDSnapshot(),
			"detail", fmt.Sprintf("relay=%s", a.cfg.Server.RelayAddr))
	})
}

// ---------------------------------------------------------------------------
// 配置派生与访问器
// ---------------------------------------------------------------------------

// dialTimeout 返回连接超时。
func (a *agent) dialTimeout() time.Duration {
	return positiveDuration(a.cfg.Server.DialTimeout, defaultDialTimeout)
}

// pairTimeout 返回等待数据面报到应答的超时。
//
// agent 的 ini 没有 pair_timeout 字段，使用 [relay] pair_timeout（config 默认 10s；
// agent 的 ini 通常没有 [relay] 段，因此等价于默认值），见 docs/protocol.md §8。
func (a *agent) pairTimeout() time.Duration {
	return positiveDuration(a.cfg.Relay.PairTimeout, defaultPairTimeout)
}

// udpSessionTimeout 返回本地 UDP 会话空闲清理时间。
func (a *agent) udpSessionTimeout() time.Duration {
	return positiveDuration(a.cfg.Server.UDPSessionTimeout, defaultUDPSessionTimeout)
}

// agentIDSnapshot 返回 relay 分配的 agent_id。
func (a *agent) agentIDSnapshot() string {
	a.ctrlMu.Lock()
	defer a.ctrlMu.Unlock()
	return a.agentID
}

// udpModeSnapshot 返回当前使用的 UDP 承载模式。
func (a *agent) udpModeSnapshot() string {
	a.ctrlMu.Lock()
	defer a.ctrlMu.Unlock()
	if a.udpMode == "" {
		return protocol.UDPModeNative
	}
	return a.udpMode
}

// setUDPMode 记录 relay 下发的 UDP 承载模式（以最后一次响应为准）。
func (a *agent) setUDPMode(mode string) {
	a.ctrlMu.Lock()
	a.udpMode = mode
	a.ctrlMu.Unlock()
}

// addRegisteredService 记录一条注册成功的服务（管理页展示用）。
func (a *agent) addRegisteredService(reg registeredService) {
	a.ctrlMu.Lock()
	a.services = append(a.services, reg)
	a.ctrlMu.Unlock()
}

// registeredServices 返回已注册服务快照的副本。
func (a *agent) registeredServices() []registeredService {
	a.ctrlMu.Lock()
	defer a.ctrlMu.Unlock()
	return append([]registeredService(nil), a.services...)
}

// tcpDataPortSnapshot 返回 relay TCP 数据面端口。
func (a *agent) tcpDataPortSnapshot() int {
	a.ctrlMu.Lock()
	defer a.ctrlMu.Unlock()
	return a.tcpDataPort
}

// udpDataPortSnapshot 返回 relay 原生 UDP 数据面端口。
func (a *agent) udpDataPortSnapshot() int {
	a.ctrlMu.Lock()
	defer a.ctrlMu.Unlock()
	return a.udpDataPort
}

// controlSnapshot 是控制连接状态的只读快照（管理页展示用）。
type controlSnapshot struct {
	addr        string
	connected   bool
	since       time.Time
	agentID     string
	udpMode     string
	tcpDataPort int
	udpDataPort int
	heartbeat   time.Duration
	lastAck     time.Time
	version     int
}

// controlSnapshot 返回控制连接状态快照。
func (a *agent) controlSnapshot() controlSnapshot {
	a.ctrlMu.Lock()
	defer a.ctrlMu.Unlock()
	s := controlSnapshot{
		connected:   a.ctrl != nil,
		since:       a.connectedAt,
		agentID:     a.agentID,
		udpMode:     a.udpMode,
		tcpDataPort: a.tcpDataPort,
		udpDataPort: a.udpDataPort,
		heartbeat:   a.heartbeat,
		lastAck:     a.lastAck,
		version:     a.relayVersion,
	}
	if a.ctrl != nil {
		s.addr = a.ctrl.remoteAddr()
	}
	return s
}

// endSession 关闭并移除一个本地会话（关连接 → 删表项）。
func (a *agent) endSession(sess *localSession) {
	if sess == nil {
		return
	}
	bytesIn, bytesOut := sess.bytesIn.Load(), sess.bytesOut.Load()
	ready := sess.isReady()
	sess.closeAll()
	a.sessions.remove(sess)
	if ready {
		a.log.Info("配对数据连接已关闭", "event", "data_close",
			"pairing_key", sess.key.String(),
			"proto", sess.proto,
			"carrier", sess.carrierName(),
			"bytes_in", bytesIn,
			"bytes_out", bytesOut,
		)
		return
	}
	a.log.Warn("配对会话未建立即结束", "event", "data_close",
		"pairing_key", sess.key.String(), "proto", sess.proto, "carrier", sess.carrierName())
}

// handleIncoming 处理一条 MsgIncoming：解析配对键、登记本地会话、按协议分派数据面。
//
// 该函数在独立 goroutine 中运行（由控制面读循环启动），因此不会阻塞读循环。
func (a *agent) handleIncoming(msg *protocol.Incoming) {
	if msg == nil {
		return
	}
	key, err := pairing.ParsePairingKey(msg.PairingKey)
	if err != nil {
		a.log.Warn("接入通知的配对键非法，已忽略", "event", "incoming", "pairing_key", msg.PairingKey, "err", err)
		return
	}
	svc, ok := a.byService[key.ServiceID]
	if !ok {
		a.log.Warn("接入通知指向未注册的服务，已忽略", "event", "incoming",
			"pairing_key", key.String(), "service_id", key.ServiceID)
		return
	}
	if msg.Role != "" && msg.Role != protocol.RoleAgent {
		a.log.Warn("接入通知的角色不是 agent，已忽略", "event", "incoming",
			"pairing_key", key.String(), "detail", "role="+msg.Role)
		return
	}
	if msg.PairingToken == "" {
		// 只记事实，绝不打印令牌内容。
		a.log.Warn("接入通知缺少配对令牌，已忽略", "event", "incoming", "pairing_key", key.String())
		return
	}

	proto := svc.Proto
	if msg.Proto != "" && msg.Proto != proto {
		// 本地目标协议由 ini 决定（决定怎么拨号），因此按本机配置处理并告警。
		a.log.Warn("接入通知的协议与本机服务配置不一致，按本机配置处理", "event", "incoming",
			"pairing_key", key.String(), "detail", msg.Proto+" != "+proto)
	}

	sess := newLocalSession(key, proto, svc.Addr)
	if err := a.sessions.add(sess); err != nil {
		a.log.Warn("该配对键已有活跃会话，忽略重复通知", "event", "incoming", "pairing_key", key.String())
		return
	}
	a.log.Info("收到接入通知", "event", "incoming",
		"pairing_key", key.String(),
		"service_id", svc.ID,
		"proto", proto,
		"remote", msg.ClientAddr,
		"detail", "local="+svc.Addr,
	)

	switch proto {
	case "udp":
		if a.udpModeSnapshot() == protocol.UDPModeTCPTunnel {
			a.handleUDPTunnel(sess, key, svc, msg.PairingToken)
			return
		}
		a.handleUDPNative(sess, key, svc, msg.PairingToken)
	default:
		a.handleTCP(sess, key, svc, msg.PairingToken)
	}
}

// hostname 返回本机名（MsgHello.client_name，失败时为空）。
func hostname() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return name
}
