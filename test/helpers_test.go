// Package e2e 是 PairLink 的端到端验收测试。
//
// 测试策略（docs/contract.md 阶段 3 要求）：
//   - 主力：进程内真实栈——直接调用 relay.Run / agent.Run / client.Run，
//     读取 t.TempDir() 中写出的真实 ini，使用真实的 loopback 端口与真实第三方
//     TCP/UDP 服务；这样能控制端口、断言 Run 返回值并验证优雅关停。
//   - 辅助：真实二进制子进程冒烟（见 binary_test.go）。
//
// 所有等待都是“轮询 + 截止时间”，没有固定 sleep 作为同步手段。
package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/agent"
	"github.com/lsong98sh/pairlink/internal/client"
	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/protocol"
	"github.com/lsong98sh/pairlink/internal/relay"
)

const testToken = "e2e-auth-token"

// ---------------------------------------------------------------------------
// 端口分配
// ---------------------------------------------------------------------------

var (
	portMu    sync.Mutex
	usedPorts = make(map[int]string)
)

// freeTCPPort 分配一个当前空闲、且在本测试进程内未被使用过的 TCP 端口。
func freeTCPPort(t *testing.T) int {
	t.Helper()
	return allocatePort(t, "tcp")
}

// freeUDPPort 分配一个当前空闲、且在本测试进程内未被使用过的 UDP 端口。
func freeUDPPort(t *testing.T) int {
	t.Helper()
	return allocatePort(t, "udp")
}

func allocatePort(t *testing.T, network string) int {
	t.Helper()
	for attempt := 0; attempt < 100; attempt++ {
		var port int
		switch network {
		case "tcp":
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("分配 TCP 端口: %v", err)
			}
			port = ln.Addr().(*net.TCPAddr).Port
			_ = ln.Close()
		case "udp":
			conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatalf("分配 UDP 端口: %v", err)
			}
			port = conn.LocalAddr().(*net.UDPAddr).Port
			_ = conn.Close()
		default:
			t.Fatalf("未知端口类型 %q", network)
		}
		portMu.Lock()
		_, used := usedPorts[port]
		if !used {
			usedPorts[port] = network
		}
		portMu.Unlock()
		if !used && port > 0 {
			return port
		}
	}
	t.Fatalf("连续 100 次未能分配未被占用的 %s 端口", network)
	return 0
}

// hostPort 返回 127.0.0.1:<port>。
func hostPort(port int) string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}

// hostPortOf 从 URL（如 httptest 的 http://127.0.0.1:1234）取出 host:port。
func hostPortOf(t *testing.T, rawURL string) string {
	t.Helper()
	s := strings.TrimPrefix(rawURL, "http://")
	s = strings.TrimPrefix(s, "https://")
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		t.Fatalf("解析 URL %q: %v", rawURL, err)
	}
	return net.JoinHostPort(host, port)
}

// ---------------------------------------------------------------------------
// ini 生成与加载
// ---------------------------------------------------------------------------

type relaySpec struct {
	Tunnel, TCP, UDP, Manage string
	AuthToken                string
	TLSCert, TLSKey          string
	Pprof                    bool
	LogLevel                 string
	PairTimeout              time.Duration
	IdleTimeout              time.Duration
	HeartbeatTimeout         time.Duration
	UDPSessionTimeout        time.Duration
	TokenTTL                 time.Duration
}

type serverService struct {
	ID   string
	Spec string // 例如 "tcp:127.0.0.1:1234"
}

type agentSpec struct {
	RelayAddr, Token, Manage string
	Protocol, Local          string
	Services                 []serverService
	TLSCA                    string
	TLSInsecure              bool
	Pprof                    bool
	LogLevel                 string
	Heartbeat                time.Duration
	UDPSessionTimeout        time.Duration
}

type clientService struct {
	ID     string
	Listen string
}

type clientSpec struct {
	RelayAddr, Token, Manage string
	Services                 []clientService
	TLSCA                    string
	TLSInsecure              bool
	Pprof                    bool
	LogLevel                 string
	Heartbeat                time.Duration
	PairTimeout              time.Duration
	UDPSessionTimeout        time.Duration
	NoSubscribe              bool
}

func writeRelayINI(t *testing.T, s relaySpec) (*config.Config, string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("[common]\n")
	if s.Manage != "" {
		fmt.Fprintf(&b, "manage_listen = %s\n", s.Manage)
	}
	fmt.Fprintf(&b, "pprof = %v\n", s.Pprof)
	b.WriteString("log_level = " + defaultLogLevel(s.LogLevel) + "\n")
	b.WriteString("\n[relay]\n")
	fmt.Fprintf(&b, "tunnel_listen = %s\n", s.Tunnel)
	fmt.Fprintf(&b, "tcp_listen = %s\n", s.TCP)
	if s.UDP != "" {
		fmt.Fprintf(&b, "udp_listen = %s\n", s.UDP)
	}
	token := s.AuthToken
	if token == "" {
		token = testToken
	}
	fmt.Fprintf(&b, "auth_token = %s\n", token)
	if s.TLSCert != "" {
		fmt.Fprintf(&b, "tls_cert = %s\n", s.TLSCert)
	}
	if s.TLSKey != "" {
		fmt.Fprintf(&b, "tls_key = %s\n", s.TLSKey)
	}
	writeDuration(&b, "pair_timeout", s.PairTimeout)
	writeDuration(&b, "idle_timeout", s.IdleTimeout)
	writeDuration(&b, "heartbeat_timeout", s.HeartbeatTimeout)
	writeDuration(&b, "udp_session_timeout", s.UDPSessionTimeout)
	writeDuration(&b, "token_ttl", s.TokenTTL)
	return loadConfig(t, writeINI(t, "relay.ini", b.String()), config.ModeRelay)
}

func writeAgentINI(t *testing.T, s agentSpec) (*config.Config, string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("[common]\n")
	if s.Manage != "" {
		fmt.Fprintf(&b, "manage_listen = %s\n", s.Manage)
	}
	fmt.Fprintf(&b, "pprof = %v\n", s.Pprof)
	b.WriteString("log_level = " + defaultLogLevel(s.LogLevel) + "\n")
	b.WriteString("\n[server]\n")
	fmt.Fprintf(&b, "relay_addr = %s\n", s.RelayAddr)
	token := s.Token
	if token == "" {
		token = testToken
	}
	fmt.Fprintf(&b, "token = %s\n", token)
	if s.Local != "" {
		fmt.Fprintf(&b, "protocol = %s\n", s.Protocol)
		fmt.Fprintf(&b, "local = %s\n", s.Local)
	}
	for _, svc := range s.Services {
		fmt.Fprintf(&b, "service.%s = %s\n", svc.ID, svc.Spec)
	}
	if s.TLSCA != "" {
		fmt.Fprintf(&b, "tls_ca = %s\n", s.TLSCA)
	}
	if s.TLSInsecure {
		b.WriteString("tls_insecure_skip_verify = true\n")
	}
	writeDuration(&b, "heartbeat", s.Heartbeat)
	writeDuration(&b, "udp_session_timeout", s.UDPSessionTimeout)
	return loadConfig(t, writeINI(t, "server.ini", b.String()), config.ModeAgent)
}

func writeClientINI(t *testing.T, s clientSpec) (*config.Config, string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("[common]\n")
	if s.Manage != "" {
		fmt.Fprintf(&b, "manage_listen = %s\n", s.Manage)
	}
	fmt.Fprintf(&b, "pprof = %v\n", s.Pprof)
	b.WriteString("log_level = " + defaultLogLevel(s.LogLevel) + "\n")
	b.WriteString("\n[client]\n")
	fmt.Fprintf(&b, "relay_addr = %s\n", s.RelayAddr)
	token := s.Token
	if token == "" {
		token = testToken
	}
	fmt.Fprintf(&b, "token = %s\n", token)
	if s.NoSubscribe {
		b.WriteString("subscribe = false\n")
	}
	for _, svc := range s.Services {
		fmt.Fprintf(&b, "service.%s = %s\n", svc.ID, svc.Listen)
	}
	if s.TLSCA != "" {
		fmt.Fprintf(&b, "tls_ca = %s\n", s.TLSCA)
	}
	if s.TLSInsecure {
		b.WriteString("tls_insecure_skip_verify = true\n")
	}
	writeDuration(&b, "heartbeat", s.Heartbeat)
	writeDuration(&b, "pair_timeout", s.PairTimeout)
	writeDuration(&b, "udp_session_timeout", s.UDPSessionTimeout)
	return loadConfig(t, writeINI(t, "client.ini", b.String()), config.ModeClient)
}

func defaultLogLevel(s string) string {
	if s == "" {
		return "warn"
	}
	return s
}

func writeDuration(b *strings.Builder, key string, d time.Duration) {
	if d > 0 {
		fmt.Fprintf(b, "%s = %s\n", key, d.String())
	}
}

func writeINI(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写 ini %s: %v", path, err)
	}
	return path
}

func loadConfig(t *testing.T, path string, mode config.Mode) (*config.Config, string) {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load(%s): %v", path, err)
	}
	if err := cfg.Validate(mode); err != nil {
		t.Fatalf("config.Validate(%s, %s): %v", path, mode, err)
	}
	for _, w := range cfg.Warnings {
		t.Logf("config warning: %s", w)
	}
	return cfg, path
}

// ---------------------------------------------------------------------------
// 进程内 Run 生命周期
// ---------------------------------------------------------------------------

type runHandle struct {
	name string

	cancel   context.CancelFunc
	stopOnce sync.Once
	done     chan struct{}
	err      error
}

func startRun(t *testing.T, name string, run func(context.Context) error) *runHandle {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h := &runHandle{name: name, cancel: cancel, done: make(chan struct{})}
	go func() {
		h.err = run(ctx)
		close(h.done)
	}()
	t.Cleanup(func() {
		h.stop(t, 15*time.Second)
		if h.err != nil && !errors.Is(h.err, context.Canceled) {
			t.Logf("cleanup: %s Run 返回: %v", name, h.err)
		}
	})
	return h
}

func (h *runHandle) stop(t *testing.T, timeout time.Duration) {
	t.Helper()
	h.stopOnce.Do(func() { h.cancel() })
	select {
	case <-h.done:
	case <-time.After(timeout):
		t.Errorf("%s: Run 未在 %s 内退出", h.name, timeout)
	}
}

func (h *runHandle) exited() bool {
	select {
	case <-h.done:
		return true
	default:
		return false
	}
}

// result 只能在 done 关闭后调用（exited()==true）。
func (h *runHandle) result() error { return h.err }

func startRelay(t *testing.T, cfg *config.Config) *runHandle {
	t.Helper()
	h := startRun(t, "relay", func(ctx context.Context) error { return relay.Run(ctx, cfg) })
	waitUntil(t, 8*time.Second, "relay 隧道端口就绪", []*runHandle{h}, func() bool {
		conn, err := net.DialTimeout("tcp", cfg.Relay.TunnelListen, 300*time.Millisecond)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	})
	if cfg.Common.ManageListen != "" {
		waitUntil(t, 5*time.Second, "relay 管理页就绪", []*runHandle{h}, func() bool {
			return adminReachable(cfg.Common.ManageListen, "/")
		})
	}
	return h
}

func startAgent(t *testing.T, cfg *config.Config) *runHandle {
	t.Helper()
	return startRun(t, "agent", func(ctx context.Context) error { return agent.Run(ctx, cfg) })
}

func startClient(t *testing.T, cfg *config.Config) *runHandle {
	t.Helper()
	return startRun(t, "client", func(ctx context.Context) error { return client.Run(ctx, cfg) })
}

// waitUntil 轮询直到 fn 返回 true；期间若有 Run 提前退出则立即失败。
func waitUntil(t *testing.T, timeout time.Duration, desc string, handles []*runHandle, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if fn() {
			return
		}
		for _, h := range handles {
			if h.exited() {
				t.Fatalf("%s: %s 提前退出，err=%v", desc, h.name, h.result())
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: 等待超时（%s）", desc, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// 管理页 HTTP 辅助
// ---------------------------------------------------------------------------

var adminHTTP = &http.Client{
	Timeout: 2 * time.Second,
	Transport: &http.Transport{
		DisableKeepAlives: true,
	},
}

func adminGet(addr, path string) (int, string, error) {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	resp, err := adminHTTP.Get("http://" + addr + path)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, "", err
	}
	return resp.StatusCode, string(body), nil
}

func adminReachable(addr, path string) bool {
	code, _, err := adminGet(addr, path)
	return err == nil && code == http.StatusOK
}

func mustAdminGet(t *testing.T, addr, path string) string {
	t.Helper()
	code, body, err := adminGet(addr, path)
	if err != nil {
		t.Fatalf("GET http://%s%s: %v", addr, path, err)
	}
	if code != http.StatusOK {
		t.Fatalf("GET http://%s%s 状态码 = %d，期望 200", addr, path, code)
	}
	return body
}

func pageInt(body, pattern string) (int, bool) {
	re := regexp.MustCompile(pattern)
	m := re.FindStringSubmatch(body)
	if len(m) != 2 {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

func waitPageCount(t *testing.T, h *runHandle, manage, pattern string, want int, desc string) {
	t.Helper()
	waitUntil(t, 8*time.Second, desc, []*runHandle{h}, func() bool {
		_, body, err := adminGet(manage, "/relay?refresh=0")
		if err != nil {
			return false
		}
		n, ok := pageInt(body, pattern)
		return ok && n == want
	})
}

func waitRelayServiceCount(t *testing.T, h *runHandle, manage string, want int) {
	t.Helper()
	waitPageCount(t, h, manage, `服务注册表（(\d+)）`, want, "relay 服务注册表数量")
}

func waitRelayAgentCount(t *testing.T, h *runHandle, manage string, want int) {
	t.Helper()
	waitPageCount(t, h, manage, `在线 agent（(\d+)）`, want, "relay 在线 agent 数量")
}

func waitRelayPairCount(t *testing.T, h *runHandle, manage string, want int) {
	t.Helper()
	waitPageCount(t, h, manage, `当前配对表（(\d+)）`, want, "relay 当前配对表数量")
}

func waitUDPSessionCount(t *testing.T, h *runHandle, manage string, native bool, want int) {
	t.Helper()
	pattern := `UDP 会话表（UDP over TCP，(\d+)）`
	desc := "relay UDP over TCP 会话数"
	if native {
		pattern = `UDP 会话表（native，(\d+)）`
		desc = "relay 原生 UDP 会话数"
	}
	waitPageCount(t, h, manage, pattern, want, desc)
}

// waitServicesRegistered 等待 relay 管理页出现指定数量的服务注册项。
func waitServicesRegistered(t *testing.T, h *runHandle, manage string, ids ...string) {
	t.Helper()
	waitUntil(t, 8*time.Second, "agent 服务注册完成", []*runHandle{h}, func() bool {
		code, body, err := adminGet(manage, "/relay?refresh=0")
		if err != nil || code != http.StatusOK {
			return false
		}
		n, ok := pageInt(body, `服务注册表（(\d+)）`)
		if !ok || n != len(ids) {
			return false
		}
		for _, id := range ids {
			if !strings.Contains(body, ">"+id+"<") {
				return false
			}
		}
		return true
	})
}

// waitClientMappings 等待 client 管理页展示全部本地监听映射。
func waitClientMappings(t *testing.T, h *runHandle, manage string, mappings map[string]string) {
	t.Helper()
	waitUntil(t, 8*time.Second, "client 本地监听就绪", []*runHandle{h}, func() bool {
		code, body, err := adminGet(manage, "/client?refresh=0")
		if err != nil || code != http.StatusOK {
			return false
		}
		if !strings.Contains(body, "本地端口映射表") {
			return false
		}
		for id, addr := range mappings {
			if !strings.Contains(body, id) || !strings.Contains(body, addr) {
				return false
			}
		}
		return true
	})
}

// relayPage 返回 relay 管理页正文（断言失败即终止）。
func relayPage(t *testing.T, manage string) string {
	t.Helper()
	return mustAdminGet(t, manage, "/relay?refresh=0")
}

// ---------------------------------------------------------------------------
// 真实第三方 TCP / UDP 服务
// ---------------------------------------------------------------------------

// tcpEchoServer 是只做回显的真实 TCP 服务（每个连接回写读到的字节）。
type tcpEchoServer struct {
	ln net.Listener

	mu    sync.Mutex
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup

	closeOnce sync.Once
}

func startTCPEchoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动 TCP echo 服务: %v", err)
	}
	s := &tcpEchoServer{ln: ln, conns: make(map[net.Conn]struct{})}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns[conn] = struct{}{}
			s.mu.Unlock()
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				defer func() {
					s.mu.Lock()
					delete(s.conns, conn)
					s.mu.Unlock()
					_ = conn.Close()
				}()
				buf := make([]byte, 32*1024)
				for {
					n, err := conn.Read(buf)
					if n > 0 {
						if _, werr := conn.Write(buf[:n]); werr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		s.closeOnce.Do(func() {
			_ = ln.Close()
			s.mu.Lock()
			for c := range s.conns {
				_ = c.Close()
			}
			s.mu.Unlock()
			s.wg.Wait()
		})
	})
	return ln.Addr().String()
}

// udpEchoServer 是只做回显的真实 UDP 服务；回包统一加前缀以便验证路由。
type udpEchoServer struct {
	conn *net.UDPConn
	done chan struct{}
}

func startUDPEchoServer(t *testing.T, prefix string) string {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("启动 UDP echo 服务: %v", err)
	}
	s := &udpEchoServer{conn: conn, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		buf := make([]byte, 65535)
		for {
			n, addr, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			out := make([]byte, 0, len(prefix)+n)
			out = append(out, prefix...)
			out = append(out, buf[:n]...)
			_, _ = conn.WriteToUDP(out, addr)
		}
	}()
	t.Cleanup(func() {
		_ = conn.Close()
		<-s.done
	})
	return conn.LocalAddr().String()
}

// startHTTP 启动真实 net/http 服务，返回 host:port 与关闭函数注册到 t.Cleanup。
func startHTTP(t *testing.T, handler http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return hostPortOf(t, srv.URL)
}

// echoText 通过一个第三方 TCP 连接发送文本并断言原样回显。
func echoText(t *testing.T, conn net.Conn, payload string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	if err := conn.SetDeadline(deadline); err != nil {
		t.Fatalf("设置连接 deadline: %v", err)
	}
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatalf("写入 echo 请求 %q: %v", payload, err)
	}
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("读取 echo 响应 %q: %v", payload, err)
	}
	if got := string(buf); got != payload {
		t.Fatalf("echo 响应 = %q，期望 %q", got, payload)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		t.Fatalf("清除连接 deadline: %v", err)
	}
}

// udpRoundTrip 通过第三方 UDP socket 发一个数据报并断言带前缀的回包。
func udpRoundTrip(t *testing.T, conn *net.UDPConn, target *net.UDPAddr, payload, want string) {
	t.Helper()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("设置 UDP deadline: %v", err)
	}
	if _, err := conn.WriteToUDP([]byte(payload), target); err != nil {
		t.Fatalf("发送 UDP 数据报 %q: %v", payload, err)
	}
	buf := make([]byte, 65535)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("读取 UDP 回包（期望 %q）: %v", want, err)
	}
	if got := string(buf[:n]); got != want {
		t.Fatalf("UDP 回包 = %q，期望 %q", got, want)
	}
	_ = conn.SetDeadline(time.Time{})
}

func newUDPClient(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("创建第三方 UDP socket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// ---------------------------------------------------------------------------
// 原始控制面 / 数据面客户端（用于精确操纵报到顺序与失败路径）
// ---------------------------------------------------------------------------

type rawControl struct {
	t    *testing.T
	conn net.Conn
	// hello 保存握手收到的 MsgHelloOK（udp_mode / 数据面端口协商结果）。
	hello *protocol.HelloOK
}

func dialRawControl(t *testing.T, tunnelAddr, role, token string) *rawControl {
	t.Helper()
	conn, err := net.DialTimeout("tcp", tunnelAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("连接 relay 隧道 %s: %v", tunnelAddr, err)
	}
	rc := &rawControl{t: t, conn: conn}
	t.Cleanup(func() { _ = conn.Close() })
	rc.send(protocol.MsgHello, &protocol.Hello{
		Role:       role,
		Token:      token,
		Version:    protocol.ProtocolVersion,
		ClientName: "e2e-raw-" + role,
	})
	typ, msg := rc.recv(3 * time.Second)
	if typ != protocol.MsgHelloOK {
		t.Fatalf("raw %s Hello 响应 = %s (%+v)，期望 hello_ok", role, typ, msg)
	}
	ok, isOK := msg.(*protocol.HelloOK)
	if !isOK {
		t.Fatalf("raw %s HelloOK 类型 = %T", role, msg)
	}
	rc.hello = ok
	return rc
}

// close 关闭原始控制连接（幂等）。
func (r *rawControl) close() { _ = r.conn.Close() }

func (r *rawControl) send(typ protocol.MessageType, payload any) {
	r.t.Helper()
	if err := r.conn.SetWriteDeadline(time.Now().Add(3 * time.Second)); err != nil {
		r.t.Fatalf("设置控制面写 deadline: %v", err)
	}
	if err := protocol.WriteFrame(r.conn, typ, payload); err != nil {
		r.t.Fatalf("写控制面 %s: %v", typ, err)
	}
	_ = r.conn.SetWriteDeadline(time.Time{})
}

func (r *rawControl) recv(timeout time.Duration) (protocol.MessageType, any) {
	r.t.Helper()
	typ, msg, err := r.tryRecv(timeout)
	if err != nil {
		r.t.Fatalf("读控制面消息: %v", err)
	}
	return typ, msg
}

func (r *rawControl) tryRecv(timeout time.Duration) (protocol.MessageType, any, error) {
	r.t.Helper()
	if err := r.conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return 0, nil, err
	}
	defer func() { _ = r.conn.SetReadDeadline(time.Time{}) }()
	typ, payload, err := protocol.ReadFrame(r.conn)
	if err != nil {
		return 0, nil, err
	}
	msg, err := protocol.Decode(typ, payload)
	if err != nil {
		return typ, nil, err
	}
	return typ, msg, nil
}

// startFakeAgent 建立一条 agent 控制连接并注册给定服务（不建立数据面）。
func startFakeAgent(t *testing.T, tunnelAddr string, services ...protocol.RegisterService) *rawControl {
	t.Helper()
	rc := dialRawControl(t, tunnelAddr, protocol.RoleAgent, testToken)
	for _, svc := range services {
		svc := svc
		rc.send(protocol.MsgRegisterService, &svc)
		typ, msg := rc.recv(3 * time.Second)
		if typ != protocol.MsgServiceRegistered {
			t.Fatalf("注册服务 %s 响应 = %s (%+v)", svc.ServiceID, typ, msg)
		}
	}
	return rc
}

// requestPair 让 raw client 发起 ConnectRequest 并返回 MsgPairResult。
func (r *rawControl) requestPair(serviceID, connID string) *protocol.PairResult {
	r.t.Helper()
	r.send(protocol.MsgConnectRequest, &protocol.ConnectRequest{
		ServiceID:    serviceID,
		ClientConnID: connID,
	})
	for {
		typ, msg := r.recv(3 * time.Second)
		switch m := msg.(type) {
		case *protocol.PairResult:
			return m
		case *protocol.ServiceEvent:
			continue
		case *protocol.Error:
			r.t.Fatalf("ConnectRequest 被拒绝: %s", m.Error())
		default:
			r.t.Fatalf("期望 pair_result，收到 %s (%T)", typ, msg)
		}
	}
}

// expectIncoming 在 fake agent 控制连接上等待 MsgIncoming。
func (r *rawControl) expectIncoming() *protocol.Incoming {
	r.t.Helper()
	for {
		typ, msg := r.recv(3 * time.Second)
		switch m := msg.(type) {
		case *protocol.Incoming:
			return m
		case *protocol.ServiceEvent:
			continue
		default:
			r.t.Fatalf("期望 incoming，收到 %s (%T)", typ, msg)
		}
	}
}

// listServices 发送 MsgListServices 并返回服务列表。
func (r *rawControl) listServices() *protocol.ServiceList {
	r.t.Helper()
	r.send(protocol.MsgListServices, &protocol.ListServices{})
	for {
		typ, msg := r.recv(3 * time.Second)
		switch m := msg.(type) {
		case *protocol.ServiceList:
			return m
		case *protocol.ServiceEvent:
			continue
		default:
			r.t.Fatalf("期望 service_list，收到 %s (%T)", typ, msg)
		}
	}
}

// expectError 等待一条 MsgError 并返回。
func (r *rawControl) expectError() *protocol.Error {
	r.t.Helper()
	for {
		typ, msg := r.recv(3 * time.Second)
		switch m := msg.(type) {
		case *protocol.Error:
			return m
		case *protocol.ServiceEvent:
			continue
		default:
			r.t.Fatalf("期望 error，收到 %s (%T)", typ, msg)
		}
	}
}

// bindRawData 建立 TCP 数据面连接到 tcp_listen 并写出 MsgBind（TCP 不等应答）。
func bindRawData(t *testing.T, tcpAddr, key, token, role string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", tcpAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("连接 relay TCP 数据面 %s: %v", tcpAddr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	bind := &protocol.Bind{
		PairingKey:   key,
		PairingToken: token,
		Role:         role,
		Carrier:      protocol.CarrierTCP,
	}
	if err := protocol.WriteFrame(conn, protocol.MsgBind, bind); err != nil {
		t.Fatalf("写 MsgBind: %v", err)
	}
	return conn
}

// expectClosedNoBytes 断言连接在 timeout 内被对端关闭且未收到任何字节。
func expectClosedNoBytes(t *testing.T, conn net.Conn, timeout time.Duration, desc string) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatalf("%s: 设置读 deadline: %v", desc, err)
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if n != 0 {
		t.Fatalf("%s: 期望未收到任何字节，实际收到 %d 字节: %q", desc, n, buf[:n])
	}
	if err == nil {
		t.Fatalf("%s: 期望连接被关闭，Read 返回 nil 错误", desc)
	}
	if isTimeoutErr(err) {
		t.Fatalf("%s: 期望连接被关闭，实际等待 %s 后超时", desc, timeout)
	}
}

func isTimeoutErr(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// udpEchoEventually 反复发送同一个数据报直到收到期望回包（UDP 允许丢包；
// 数据面刚建立时首发可能因对端尚未登记而被丢弃，这里用有界重试消除该竞态）。
func udpEchoEventually(t *testing.T, conn *net.UDPConn, target *net.UDPAddr, payload, want string, timeout time.Duration) {
	t.Helper()
	if err := udpEchoEventuallyErr(conn, target, payload, want, timeout); err != nil {
		t.Fatalf("%v", err)
	}
}

// udpEchoEventuallyErr 是 udpEchoEventually 的非 fatal 版本，供 goroutine 使用。
func udpEchoEventuallyErr(conn *net.UDPConn, target *net.UDPAddr, payload, want string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if err := conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
			return fmt.Errorf("设置 UDP deadline: %w", err)
		}
		if _, err := conn.WriteToUDP([]byte(payload), target); err != nil {
			return fmt.Errorf("发送 UDP 数据报 %q: %w", payload, err)
		}
		buf := make([]byte, 65535)
		n, _, err := conn.ReadFromUDP(buf)
		if err == nil {
			got := string(buf[:n])
			if got != want {
				return fmt.Errorf("UDP 回包 = %q，期望 %q", got, want)
			}
			_ = conn.SetReadDeadline(time.Time{})
			return nil
		}
		lastErr = err
		if !isTimeoutErr(err) {
			return fmt.Errorf("读取 UDP 回包 %q: %w", want, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("等待 UDP 回包 %q 超时（%s），最后一次错误: %v", want, timeout, lastErr)
		}
		time.Sleep(60 * time.Millisecond)
	}
}

// sectionByHeading 截取管理页中包含 heading 的 <section>…</section> 文本。
func sectionByHeading(body, heading string) string {
	idx := strings.Index(body, heading)
	if idx < 0 {
		return ""
	}
	start := strings.LastIndex(body[:idx], "<section>")
	end := strings.Index(body[idx:], "</section>")
	if start < 0 || end < 0 {
		return ""
	}
	end += idx + len("</section>")
	return body[start:end]
}

var (
	pairingKeyInSection = regexp.MustCompile(`<td class="mono">([a-z0-9_-]+/[a-z0-9_-]+)</td>`)
	addrInSection       = regexp.MustCompile(`<td class="mono">(127\.0\.0\.1:\d+)</td>`)
)

// relayUDPPairingKeys 返回 relay UDP 会话表里的配对键（去重、保序）。
func relayUDPPairingKeys(body string) []string {
	sec := sectionByHeading(body, "UDP 会话表")
	var out []string
	seen := map[string]bool{}
	for _, m := range pairingKeyInSection.FindAllStringSubmatch(sec, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// relayUDPSessionRemotes 返回 UDP over TCP 会话表里出现过的承载对端地址集合。
func relayUDPSessionRemotes(body string) []string {
	sec := sectionByHeading(body, "UDP 会话表（UDP over TCP")
	var out []string
	seen := map[string]bool{}
	for _, m := range addrInSection.FindAllStringSubmatch(sec, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// rawEchoLoop 在 agent 侧数据连接上做回显，用于精确控制报到顺序的测试。
func rawEchoLoop(conn net.Conn) {
	buf := make([]byte, 32*1024)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			if _, werr := conn.Write(buf[:n]); werr != nil {
				_ = conn.Close()
				return
			}
		}
		if err != nil {
			_ = conn.Close()
			return
		}
	}
}

// rawEchoRoundTrip 向原始数据连接写入并断言回显。
func rawEchoRoundTrip(t *testing.T, conn net.Conn, payload string) {
	t.Helper()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("设置数据面 deadline: %v", err)
	}
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatalf("写数据面 %q: %v", payload, err)
	}
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("读数据面回显 %q: %v", payload, err)
	}
	if string(buf) != payload {
		t.Fatalf("数据面回显 = %q，期望 %q", buf, payload)
	}
	_ = conn.SetDeadline(time.Time{})
}

// ---------------------------------------------------------------------------
// goroutine 泄漏看护
// ---------------------------------------------------------------------------

// guardGoroutines 在测试开始时记录稳定的 goroutine 基数，并在所有其它
// t.Cleanup（栈关停、服务关闭）执行完之后检查是否回落到基数 +2 以内。
//
// 必须作为测试的第一个 Cleanup 注册（即最先调用），这样它最后执行。
func guardGoroutines(t *testing.T) {
	t.Helper()
	base := stableGoroutines(t)
	t.Logf("goroutine 基线: %d", base)
	t.Cleanup(func() {
		deadline := time.Now().Add(8 * time.Second)
		for {
			n := runtime.NumGoroutine()
			if n <= base+2 {
				t.Logf("goroutine 回落: base=%d now=%d（容差 +2）", base, n)
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("goroutine 泄漏: base=%d now=%d（容差 +2）\n%s",
					base, n, goroutineProfile())
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
}

// stableGoroutines 连续采样，返回一个稳定值（相邻差异不超过 1）。
func stableGoroutines(t *testing.T) int {
	t.Helper()
	last := runtime.NumGoroutine()
	for i := 0; i < 40; i++ {
		time.Sleep(25 * time.Millisecond)
		n := runtime.NumGoroutine()
		if absInt(n-last) <= 1 {
			return n
		}
		last = n
	}
	return last
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// goroutineProfile 返回全部 goroutine 的栈（调试泄漏用）。
func goroutineProfile() string {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return string(buf[:n])
}

// goroutineProfilePprof 用 runtime/pprof 采集 goroutine profile（debug=1）。
func goroutineProfilePprof() string {
	var b strings.Builder
	if err := pprofGoroutine(&b); err != nil {
		return "pprof 采集失败: " + err.Error()
	}
	return b.String()
}

// pprofGoroutine 把 goroutine profile 写入 w。
func pprofGoroutine(w io.Writer) error {
	p := pprof.Lookup("goroutine")
	if p == nil {
		return errors.New("runtime/pprof 中没有 goroutine profile")
	}
	return p.WriteTo(w, 1)
}
