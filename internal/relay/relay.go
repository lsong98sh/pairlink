// Package relay 实现 relay 模式（公网中继服务器）。
//
// relay 严格分离控制面与数据面：
//   - TunnelListener（tunnel_listen）：信令，含 TLS；
//   - TCPDataListener（tcp_listen）：纯 TCP 透传 + UDP over TCP 承载连接；
//   - UDPDataListener（udp_listen，可选）：原生 UDP 数据面。
//
// 实现遵循 docs/contract.md 与 docs/protocol.md：数据面只读建立瞬间的一帧
// MsgBind，之后（除 UDP over TCP 的隧道帧外）不解析任何上层协议字节。
package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/logx"
	"github.com/lsong98sh/pairlink/internal/pairing"
	"github.com/lsong98sh/pairlink/internal/protocol"
	"github.com/lsong98sh/pairlink/internal/registry"
	"github.com/lsong98sh/pairlink/internal/token"
)

const (
	// maxPendingPerConn 是单条 client 控制连接允许的"已签发但未完成配对"请求上限，
	// 超过后该连接的 ConnectRequest 回 too_many_pending（SEC-D2）。
	maxPendingPerConn = 64
	// minCarrierReadTimeout 是承载连接单帧读超时的下限（SEC-M4）。
	//
	// 有效值 = max(minCarrierReadTimeout, 2×udp_session_timeout)：半帧/静默连接
	// 最多占用资源这么久就会被关闭并清理会话。
	minCarrierReadTimeout = 30 * time.Second
	// controlHelloTimeout 是控制面等待首个 MsgHello 的读超时。
	controlHelloTimeout = 10 * time.Second
	// controlWriteTimeout 是控制面单次写帧的超时。
	controlWriteTimeout = 10 * time.Second
	// shutdownWriteTimeout 是停机通知的写超时（尽力而为）。
	shutdownWriteTimeout = time.Second
	// minSweepInterval 是 PairingTable.Sweep 的最小周期。
	minSweepInterval = 100 * time.Millisecond
	// heartbeatCheckInterval 是心跳判死检查周期。
	heartbeatCheckInterval = 5 * time.Second
)

// serviceIDPattern 与服务ID 的约定一致（[a-z0-9-_]{1,64}）。
var serviceIDPattern = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// validServiceID 报告服务ID 是否符合协议约定。
func validServiceID(id string) bool { return serviceIDPattern.MatchString(id) }

// validProto 报告协议类型是否合法。
func validProto(p string) bool { return p == "tcp" || p == "udp" }

// Run 启动 relay 模式，直到 ctx 取消后返回。
//
// 启动顺序遵循 docs/contract.md §2.2：先监听数据面，再监听隧道，最后启动
// 管理页与后台 ticker；返回前关闭全部监听与连接并等待自己启动的 goroutine 退出。
func Run(ctx context.Context, cfg *config.Config) error {
	return newServer(cfg).serve(ctx)
}

// newServer 构造 relay 的共享状态与监听器占位（尚不监听端口）。
func newServer(cfg *config.Config) *server {
	log := logx.New(logx.Config{
		Level:  cfg.Common.LogLevel,
		Format: cfg.Common.LogFormat,
		Mode:   "relay",
	})

	s := &server{
		cfg:        cfg,
		log:        log,
		services:   registry.NewServiceRegistry(),
		agents:     registry.NewAgentRegistry(),
		pairs:      pairing.NewTable(cfg.Relay.PairTimeout),
		tokens:     token.NewStore(cfg.Relay.TokenTTL),
		carriers:   newCarrierTable(),
		dataConns:  newConnSet(),
		conns:      newConnLimiter(cfg.Relay.MaxConns, cfg.Relay.MaxConnsPerIP),
		waits:      make(map[pairing.PairingKey]*pairWait),
		active:     make(map[pairing.PairingKey]activePair),
		agentConns: make(map[string]map[net.Conn]struct{}),
		ready:      make(chan struct{}),
		started:    make(chan struct{}),
	}

	// udp_mode 在启动时确定：配置了 udp_listen 走原生 UDP，否则复用 tcp_listen。
	if strings.TrimSpace(cfg.Relay.UDPListen) != "" {
		s.udpMode = protocol.UDPModeNative
		s.udpTable = registry.NewUDPTable()
	} else {
		s.udpMode = protocol.UDPModeTCPTunnel
	}

	// service_allow 白名单：启动时固化，运行期不变（SEC-A2）。
	if len(cfg.Relay.ServiceAllow) > 0 {
		s.serviceAllow = make(map[string]struct{}, len(cfg.Relay.ServiceAllow))
		for _, id := range cfg.Relay.ServiceAllow {
			s.serviceAllow[id] = struct{}{}
		}
	}

	s.pairs.SetOnExpire(s.onPairExpire)
	s.idleTimeout.Store(int64(cfg.Relay.IdleTimeout))
	s.heartbeatTimeout.Store(int64(cfg.Relay.HeartbeatTimeout))
	s.udpSessionTimeout.Store(int64(cfg.Relay.UDPSessionTimeout))
	s.carrierTimeout.Store(int64(carrierReadTimeout(cfg.Relay.UDPSessionTimeout)))
	return s
}

// carrierReadTimeout 计算承载连接单帧读超时：max(30s, 2×udp_session_timeout)。
//
// 承载连接可能承载多个 UDP 会话，静默期允许较长；但半帧（只发长度前缀不发
// payload）不应无限占用 goroutine 与会话表项（SEC-M4）。
func carrierReadTimeout(udpSessionTimeout time.Duration) time.Duration {
	timeout := 2 * udpSessionTimeout
	if timeout < minCarrierReadTimeout {
		timeout = minCarrierReadTimeout
	}
	return timeout
}

// pairTimeout 返回当前占位/数据面报到超时（运行期可被管理页修改）。
func (s *server) pairTimeout() time.Duration {
	return s.pairs.Timeout()
}

// tokenTTL 返回当前一次性令牌有效期（运行期可被管理页修改）。
func (s *server) tokenTTL() time.Duration {
	return s.tokens.TTL()
}

// idleTimeoutValue 返回当前数据面空闲超时。
func (s *server) idleTimeoutValue() time.Duration {
	return time.Duration(s.idleTimeout.Load())
}

// heartbeatTimeoutValue 返回当前控制面心跳判死超时。
func (s *server) heartbeatTimeoutValue() time.Duration {
	return time.Duration(s.heartbeatTimeout.Load())
}

// udpSessionTimeoutValue 返回当前 UDP 会话空闲超时。
func (s *server) udpSessionTimeoutValue() time.Duration {
	return time.Duration(s.udpSessionTimeout.Load())
}

// carrierFrameTimeout 返回承载连接单帧读超时（构造时/运行期按 udp_session_timeout 计算）。
func (s *server) carrierFrameTimeout() time.Duration {
	return time.Duration(s.carrierTimeout.Load())
}

// setRuntimeTimeouts 更新三个 atomic 超时（调用方已完成校验）。
//
// pair_timeout / token_ttl 分别通过 PairingTable / token.Store 的 Set* 更新，
// 不经过本函数。
func (s *server) setRuntimeTimeouts(idle, heartbeat, udpSession time.Duration) {
	s.idleTimeout.Store(int64(idle))
	s.heartbeatTimeout.Store(int64(heartbeat))
	s.udpSessionTimeout.Store(int64(udpSession))
	s.carrierTimeout.Store(int64(carrierReadTimeout(udpSession)))
}

// onPairExpire 是 PairingTable 的超时回调：关闭超时占位的连接。
//
// 该回调在 PairingTable 锁外执行，可以安全地 Close；具体的超时日志由等待方
// 记录（避免与 Sweep 重复刷屏）。
func (s *server) onPairExpire(key pairing.PairingKey, side pairing.Side, peer any) {
	if c, ok := peer.(interface{ Close() error }); ok {
		_ = c.Close()
	}
	s.log.Debug("配对占位超时", "event", "pair_timeout", "pairing_key", key.String(), "side", side.String())
}

// newSessionID 生成 agent/client 会话 ID（前缀 + 8 字节 crypto/rand 的 hex）。
func newSessionID(prefix string) (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("relay: 生成会话 ID: %w", err)
	}
	return prefix + hex.EncodeToString(raw[:]), nil
}

// boolPtr 返回 v 的指针（用于 protocol.HelloOK.DataTLS 这类可选布尔字段）。
func boolPtr(v bool) *bool { return &v }

// serviceAllowed 报告服务名是否在 [relay] service_allow 白名单内。
//
// 未配置白名单（空）时一律允许——启动时会 WARN 该风险（SEC-A2）。
func (s *server) serviceAllowed(serviceID string) bool {
	if len(s.serviceAllow) == 0 {
		return true
	}
	_, ok := s.serviceAllow[serviceID]
	return ok
}

// maxServicesPerAgent 返回单 agent 服务数上限（0 = 不限）。
func (s *server) maxServicesPerAgent() int { return s.cfg.Relay.MaxServicesPerAgent }

// agentOnline 报告 agent 是否存在且最近心跳未超时。
//
// 用于两处判定：client 的 ConnectRequest 是否可接入，以及服务换主时原 owner
// 是否仍在线（在线则拒绝抢占，离线则允许接管）。
func (s *server) agentOnline(agentID string) bool {
	agent, ok := s.agents.Get(agentID)
	if !ok {
		return false
	}
	if timeout := s.heartbeatTimeoutValue(); timeout > 0 {
		return time.Since(agent.LastSeen) <= timeout
	}
	return true
}
