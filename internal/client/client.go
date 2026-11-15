// Package client 实现 client 模式（访问侧）。
//
// client 连接 relay 的隧道端口，拉取（并可选订阅）服务列表，然后按
// [client] service.<ID> = <本地监听地址> 在本机开 TCP/UDP 监听；第三方客户端
// 连本地端口时，client 生成 client_conn_id 并发 MsgConnectRequest，走完整配对
// 流程，把本地连接与 relay 数据面连接双向转发（TCP 纯透传 / UDP 原生或
// UDP over TCP 隧道帧）。
//
// 本包由阶段 2 client 子代理独占（docs/contract.md §1），不 import relay/agent，
// 也不修改任何冻结包。
//
// 阶段 6C 起（docs/contract.md §2.5）：
//   - 控制连接意外断开（非踢出/非 relay 停机）后**默认自动重连**（[client] reconnect，
//     默认 true）：指数退避 base 1s / ×2 / cap reconnect_max_backoff（默认 60s）/ ±20%
//     抖动；重连成功后重新 Hello、重新 ListServices、重新 Subscribe，刷新本地服务视图
//     与 udp_mode，并**强制接受 relay 的新 revision 基线**（relay 重启后 revision 会归零）；
//     本地监听不重建，第三方无感；
//   - 重连期间关闭全部在途本地会话（reason=reconnect），不做迁移；
//   - 订阅到 proto 变化时热切换本地监听（TCP↔UDP），失败回滚到旧协议（见 listener.go）；
//   - reconnect = false 时保持旧行为（断开即返回错误，由外部守护重启）；
//   - UDP 本地会话空闲超时由 [client] udp_session_timeout 控制（默认 60s）；
//   - TLS 由三态 [client] tls 控制（阶段 6G）：auto 时按 tls_ca / tls_insecure_skip_verify
//     推断（两者皆空走明文，既有行为）；true 强制 TLS，tls_ca 为空时用系统根证书；
//     false 强制明文。两侧配置必须匹配，不匹配时握手失败并报清晰错误；
//   - relay 未下发数据面端口且本机 ini 无 [relay] 兜底端口时只 WARN，
//     Run 继续运行，失败的只是需要数据面的那一次配对。
package client

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/lsong98sh/pairlink/internal/admin"
	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/logx"
	"github.com/lsong98sh/pairlink/internal/protocol"
	"github.com/lsong98sh/pairlink/internal/reconnect"
)

const (
	// defaultUDPSessionTimeout 是 [client] udp_session_timeout 缺省值。
	//
	// 与 [relay] udp_session_timeout 的默认值保持一致；ini 可覆盖
	// （阶段 4 起 [client] 段新增该字段，取代阶段 2/3 的包内常量）。
	defaultUDPSessionTimeout = 60 * time.Second

	defaultPairTimeout = 10 * time.Second
	defaultDialTimeout = 10 * time.Second

	// minSweepInterval / maxSweepInterval 限定配对超时与 UDP 空闲清理的扫描周期。
	minSweepInterval = 20 * time.Millisecond
	maxSweepInterval = time.Second

	// adminShutdownTimeout 是 Run 退出前等待管理页关闭的上限。
	adminShutdownTimeout = 3 * time.Second
)

// 包内哨兵错误。
var (
	// errKicked 表示被 relay 管理页踢出（返回错误，不自动重连）。
	errKicked = errors.New("client: 已被 relay 踢出")
	// errCarrierClosed 表示 UDP over TCP 承载连接已关闭。
	errCarrierClosed = errors.New("client: UDP over TCP 承载连接已关闭")
	// errControlClosed 表示控制连接尚未建立或已关闭。
	errControlClosed = errors.New("client: 控制连接已关闭")
)

// Run 启动 client 模式，直到 ctx 取消、relay 停机或遇到不可恢复错误后返回。
//
// 返回语义（docs/contract.md §2.1 / §2.5）：
//   - ctx 取消且清理完成 → nil；
//   - relay 发 MsgServerShutdown → nil；
//   - 本地监听失败（端口占用）、配置非法、relay 明确拒绝（MsgError）或协议版本不匹配
//     → 非 nil（不重试）；
//   - 启用 [client] reconnect（默认）时：拨号/握手阶段的可重试失败与控制连接意外断开
//     不会结束进程，而是按指数退避重连；重连期间进程存活、本地监听保持不变；
//   - 被踢出（MsgKicked）→ 清理后返回非 nil；
//   - reconnect = false 时控制连接断开 → 返回非 nil（旧行为）。
func Run(ctx context.Context, cfg *config.Config) error {
	if cfg == nil {
		return errors.New("client: 配置为 nil")
	}
	return newClient(ctx, cfg).run()
}

// newClient 构造 client（Run 使用的入口）。
func newClient(ctx context.Context, cfg *config.Config) *client {
	log := logx.New(logx.Config{
		Level:  cfg.Common.LogLevel,
		Format: cfg.Common.LogFormat,
		Mode:   string(config.ModeClient),
	})
	return newClientWithLogger(ctx, cfg, log)
}

// newClientWithLogger 用指定 logger 构造 client（测试注入可观测 logger 用）。
func newClientWithLogger(ctx context.Context, cfg *config.Config, log *slog.Logger) *client {
	if ctx == nil {
		ctx = context.Background()
	}
	if log == nil {
		log = slog.Default()
	}
	cctx, cancel := context.WithCancel(ctx)

	host := ""
	if h, _, err := net.SplitHostPort(cfg.Client.RelayAddr); err == nil {
		host = h
	}

	return &client{
		cfg:         cfg,
		log:         log,
		parent:      ctx,
		ctx:         cctx,
		cancel:      cancel,
		stopSig:     newStopSignal(),
		tasks:       newTaskGroup(),
		startedAt:   time.Now(),
		ctrlLog:     logx.Component(log, "control"),
		host:        host,
		services:    make(map[string]protocol.ServiceInfo),
		mappings:    append([]config.ClientService(nil), cfg.Client.Services...),
		tcpSessions: make(map[string]*tcpSession),
		udpSessions: make(map[udpSessionKey]*udpSession),
		udpByConn:   make(map[string]*udpSession),
	}
}

// negotiated 是每次（重）握手协商出的只读状态快照。
//
// 重连会整体替换它，因此所有读取都必须走 negotiated() 拿快照，不能直接读字段
// （数据面 goroutine 和控制面并发访问）。
type negotiated struct {
	clientID       string
	udpMode        string
	heartbeat      time.Duration
	tcpPort        int
	udpPort        int
	tcpDataAddr    string
	tcpDataErr     error
	udpDataAddr    string
	udpDataUDPAddr *net.UDPAddr
	udpDataErr     error
	// dataTLS 是本次连接协商出的数据面 TLS 状态（relay 的 HelloOK.data_tls ×
	// 本地 [client] data_tls）；dataTLSCfg 仅在 dataTLS=true 时非 nil，只读可复用。
	dataTLS    bool
	dataTLSCfg *tls.Config
}

// client 是 client 模式的全部运行状态。
//
// 并发约定：
//   - ctrl / ctrlMu 保护控制连接指针与写（帧必须串行；重连整体替换 ctrl）；
//   - negMu 保护每次握手协商出的状态快照（重连时整体替换）；
//   - svcMu 保护服务列表视图（控制面写，管理页读）；
//   - tcpMu / udpMu 分别保护本地 TCP 连接表与本地 UDP 会话表；
//   - listenerMu 保护本地监听器列表，listenSwitchMu 串行化热切换；
//   - carrierMu 保护 UDP over TCP 承载连接指针，carrierDialMu 串行化承载连接的创建；
//   - 所有 IO（Close/Write/Dial）都在锁外进行，避免锁内阻塞。
type client struct {
	cfg    *config.Config
	log    *slog.Logger
	parent context.Context

	ctx    context.Context
	cancel context.CancelFunc

	stopSig      *stopSignal
	tasks        *taskGroup
	shutdownOnce sync.Once
	startedAt    time.Time

	ctrl    net.Conn
	ctrlMu  sync.Mutex
	ctrlLog *slog.Logger

	// 握手与数据面协商结果（每次（重）连接整体替换）。
	host     string
	negMu    sync.RWMutex
	negState negotiated

	svcMu    sync.RWMutex
	services map[string]protocol.ServiceInfo
	revision uint64

	listenerMu     sync.Mutex
	listenSwitchMu sync.Mutex
	listeners      []*localListener

	// mappingMu 保护运行期本地映射表（阶段 7：管理页可增删改）。
	// cfg.Client.Services 只作为启动时的初始值，重连校对与管理页都以本表为准。
	mappingMu sync.Mutex
	mappings  []config.ClientService
	// mappingOpMu 串行化管理页的映射变更（新增/修改/删除）。
	mappingOpMu sync.Mutex

	// listenFn 打开本地监听（测试可注入以模拟端口占用/失败）。
	listenFn func(proto, addr string) (*localListener, error)

	tcpMu       sync.Mutex
	tcpSessions map[string]*tcpSession

	udpMu       sync.Mutex
	udpSessions map[udpSessionKey]*udpSession
	udpByConn   map[string]*udpSession

	carrierMu     sync.Mutex
	carrierDialMu sync.Mutex
	carrier       *udpTunnelCarrier

	// reconnectBase 是重连退避的基准等待（默认 1s，测试可缩短）。
	reconnectBase time.Duration
	backoffOnce   sync.Once
	backoffSeq    *reconnect.Backoff

	admin     *admin.Server
	adminAddr string
}

// run 是 Run 的主体：管理页 → 超时清理 ticker → 连接监督（含本地监听）→ 关停。
//
// 本地监听只在**首次**接入成功后启动，重连不重建（避免端口占用与第三方连接中断）。
func (c *client) run() error {
	defer c.cancel()

	c.log.Info("client 启动", "event", "start",
		"detail", fmt.Sprintf("relay=%s services=%d reconnect=%t",
			c.cfg.Client.RelayAddr, len(c.mappingSnapshot()), c.reconnectEnabled()))
	// v0.5.0：原生 UDP 数据面默认 DTLS-PSK；关闭时启动即提示风险（须与 relay 一致）。
	if !c.cfg.Client.UDPDTLS {
		c.log.Warn("udp_dtls = false：原生 UDP 数据面按 v0.4.0 明文运行（业务数据可被链路上的人读取），且必须与 relay 的 udp_dtls 一致",
			"event", "udp_dtls_disabled", "udp_dtls", false)
	}

	if err := c.startAdmin(); err != nil {
		c.shutdown()
		return err
	}

	// 超时清理 ticker 与连接无关，整个进程只启动一次。
	c.tasks.Go(c.sweepLoop)

	err := c.supervise()
	c.shutdown()
	return err
}

// shutdown 执行统一关停：先取消内部 ctx 并停止接收新任务，再按
// 「关监听 → 关控制连接 → 关数据面/会话 → 等 goroutine 全退 → 关管理页」顺序清理。
//
// shutdown 幂等，可被 run 的任意错误分支重复调用。
func (c *client) shutdown() {
	c.shutdownOnce.Do(func() {
		c.log.Info("开始关停", "event", "shutdown")
		c.cancel()
		c.tasks.Close()

		c.closeListeners()

		c.closeControl()

		c.closeSessions("shutdown")
		c.tasks.Wait()

		if c.admin != nil {
			ctx, cancel := context.WithTimeout(context.Background(), adminShutdownTimeout)
			if err := c.admin.Shutdown(ctx); err != nil {
				c.log.Warn("管理页关闭不干净", "event", "shutdown", "err", err)
			}
			cancel()
		}
		c.log.Info("client 已退出", "event", "exit")
	})
}

// closeSessions 关闭承载连接与全部本地 TCP/UDP 会话（幂等，可在锁外调用）。
//
// reason 用于日志：关停为 shutdown，重连前为 reconnect。本地监听不在此关闭。
func (c *client) closeSessions(reason string) {
	c.carrierMu.Lock()
	cc := c.carrier
	c.carrier = nil
	c.carrierMu.Unlock()
	if cc != nil {
		cc.close(reason, nil)
	}

	var tcpList []*tcpSession
	c.tcpMu.Lock()
	for _, s := range c.tcpSessions {
		tcpList = append(tcpList, s)
	}
	c.tcpMu.Unlock()
	for _, s := range tcpList {
		c.closeTCPSession(s, reason, nil)
	}

	var udpList []*udpSession
	c.udpMu.Lock()
	for _, s := range c.udpSessions {
		udpList = append(udpList, s)
	}
	c.udpMu.Unlock()
	for _, s := range udpList {
		c.closeUDPSession(s, reason, nil)
	}
}

// sweepLoop 是唯一的超时清理驱动：配对超时（TCP 与 UDP 等待 PairResult）
// 与 UDP 会话空闲清理共用同一个 ticker，不产生 per-session goroutine。
func (c *client) sweepLoop() {
	t := time.NewTicker(c.sweepInterval())
	defer t.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case now := <-t.C:
			c.sweep(now)
		}
	}
}

// sweepInterval 返回清理周期：pair_timeout/4，限制在 [20ms, 1s]。
func (c *client) sweepInterval() time.Duration {
	d := c.pairTimeout() / 4
	if d > maxSweepInterval {
		d = maxSweepInterval
	}
	if d < minSweepInterval {
		d = minSweepInterval
	}
	return d
}

// sweep 清理超时的 TCP 本地连接与 UDP 本地会话。
func (c *client) sweep(now time.Time) {
	pairTimeout := c.pairTimeout()

	var expiredTCP []*tcpSession
	c.tcpMu.Lock()
	for _, s := range c.tcpSessions {
		if s.pairExpired(now, pairTimeout) {
			expiredTCP = append(expiredTCP, s)
		}
	}
	c.tcpMu.Unlock()
	for _, s := range expiredTCP {
		c.log.Warn("等待配对结果超时，关闭本地连接", "event", "pair_timeout",
			"service_id", s.svcID, "pairing_key", s.displayKey(), "remote", s.remote)
		c.closeTCPSession(s, "pair_timeout", nil)
	}

	var expiredUDP, idleUDP []*udpSession
	idleTimeout := c.udpSessionTimeout()
	c.udpMu.Lock()
	for _, s := range c.udpSessions {
		switch {
		case s.pairExpired(now, pairTimeout):
			expiredUDP = append(expiredUDP, s)
		case s.idleExpired(now, idleTimeout):
			idleUDP = append(idleUDP, s)
		}
	}
	c.udpMu.Unlock()
	for _, s := range expiredUDP {
		c.log.Warn("UDP 会话等待配对结果超时，关闭会话", "event", "pair_timeout",
			"service_id", s.svcID, "pairing_key", s.displayKey(), "remote", s.remoteAddr())
		c.closeUDPSession(s, "pair_timeout", nil)
	}
	for _, s := range idleUDP {
		c.log.Info("UDP 会话空闲超时，关闭会话", "event", "data_close",
			"service_id", s.svcID, "pairing_key", s.displayKey(), "remote", s.remoteAddr(),
			"detail", fmt.Sprintf("reason=idle_timeout idle=%s", idleTimeout))
		c.closeUDPSession(s, "idle_timeout", nil)
	}
}

// udpSessionTimeout 返回 client 侧 UDP 本地会话的空闲清理时间。
//
// ini 未配置（或绕过 ini 直接构造 Config）时回退 defaultUDPSessionTimeout。
func (c *client) udpSessionTimeout() time.Duration {
	if c.cfg.Client.UDPSessionTimeout > 0 {
		return c.cfg.Client.UDPSessionTimeout
	}
	return defaultUDPSessionTimeout
}

// pairTimeout 返回等待 PairResult 的超时。
func (c *client) pairTimeout() time.Duration {
	if c.cfg.Client.PairTimeout > 0 {
		return c.cfg.Client.PairTimeout
	}
	return defaultPairTimeout
}

// dialTimeout 返回拨号（控制面/数据面）与握手读写的超时。
func (c *client) dialTimeout() time.Duration {
	if c.cfg.Client.DialTimeout > 0 {
		return c.cfg.Client.DialTimeout
	}
	return defaultDialTimeout
}

// newConnID 生成 8 字节 crypto/rand 的 hex（16 字符），满足 [a-z0-9-_]{1,64}。
func newConnID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("client: 生成 client_conn_id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// stopSignal 是一次性的「Run 该退出了」信号，携带退出错误（nil 表示正常退出）。
//
// 触发者与等待者通过 close(done) 建立 happens-before，读 Err 无需额外加锁。
type stopSignal struct {
	once sync.Once
	done chan struct{}
	err  error
}

// newStopSignal 构造 stopSignal。
func newStopSignal() *stopSignal { return &stopSignal{done: make(chan struct{})} }

// trigger 触发一次（后续调用被忽略）。
func (s *stopSignal) trigger(err error) {
	s.once.Do(func() {
		s.err = err
		close(s.done)
	})
}

// Err 返回退出错误；必须在 <-done 之后调用。
func (s *stopSignal) Err() error { return s.err }

// taskGroup 是可关闭的 goroutine 组：Close 之后 Go 不再启动新 goroutine，
// 从而避免 sync.WaitGroup 的 Add 与 Wait 并发导致计数错乱。
type taskGroup struct {
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

// newTaskGroup 构造 taskGroup。
func newTaskGroup() *taskGroup { return &taskGroup{} }

// Go 启动一个受管 goroutine；组已关闭时直接丢弃（不再启动）。
func (g *taskGroup) Go(fn func()) {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return
	}
	g.wg.Add(1)
	g.mu.Unlock()
	go func() {
		defer g.wg.Done()
		fn()
	}()
}

// Close 关闭组：此后再调用 Go 不会启动新 goroutine。
func (g *taskGroup) Close() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
}

// Wait 等待所有已启动的 goroutine 退出。
func (g *taskGroup) Wait() { g.wg.Wait() }
