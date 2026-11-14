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
	// maxPendingPairs 是待配对表的软上限，超过后对新的 ConnectRequest 回
	// too_many_pending。
	maxPendingPairs = 4096
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

	s.pairs.SetOnExpire(s.onPairExpire)
	return s
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
