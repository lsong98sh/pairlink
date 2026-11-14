package e2e

import (
	"fmt"
	"net"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

var goroutineTotalRe = regexp.MustCompile(`goroutine profile: total (\d+)`)

// TestE2EGoroutineNoLeak 验收项（task.md §10）：整轮流量后无僵尸配对与
// goroutine 泄漏；用 runtime.NumGoroutine + runtime/pprof 佐证。
func TestE2EGoroutineNoLeak(t *testing.T) {
	guardGoroutines(t)
	base := runtime.NumGoroutine()

	echoAddr := startTCPEchoServer(t)

	relayManage := hostPort(freeTCPPort(t))
	relayCfg, _ := writeRelayINI(t, relaySpec{
		Tunnel:      hostPort(freeTCPPort(t)),
		TCP:         hostPort(freeTCPPort(t)),
		Manage:      relayManage,
		Pprof:       true,
		PairTimeout: 5 * time.Second,
		IdleTimeout: 30 * time.Second,
	})
	relayH := startRelay(t, relayCfg)

	agentManage := hostPort(freeTCPPort(t))
	agentCfg, _ := writeAgentINI(t, agentSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    agentManage,
		Pprof:     true,
		Protocol:  "tcp",
		Local:     echoAddr,
	})
	agentH := startAgent(t, agentCfg)
	waitServicesRegistered(t, relayH, relayManage, "default")

	clientManage := hostPort(freeTCPPort(t))
	clientPort := freeTCPPort(t)
	clientCfg, _ := writeClientINI(t, clientSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    clientManage,
		Pprof:     true,
		Services:  []clientService{{ID: "default", Listen: hostPort(clientPort)}},
	})
	clientH := startClient(t, clientCfg)
	waitClientMappings(t, clientH, clientManage, map[string]string{
		"default": hostPort(clientPort),
	})

	// 一轮完整流量：建立/断开 5 次 TCP 配对，每次多轮回显。
	for i := 0; i < 5; i++ {
		conn, err := net.DialTimeout("tcp", hostPort(clientPort), 5*time.Second)
		if err != nil {
			t.Fatalf("第 %d 次连接本地端口: %v", i, err)
		}
		for round := 0; round < 3; round++ {
			echoText(t, conn, fmt.Sprintf("goroutine-round-%d-%d", i, round))
		}
		_ = conn.Close()
	}
	waitRelayPairCount(t, relayH, relayManage, 0)
	waitUntil(t, 8*time.Second, "client 活跃配对回落", nil, func() bool {
		code, body, err := adminGet(clientManage, "/client?refresh=0")
		return err == nil && code == 200 && strings.Contains(body, "当前没有活跃配对")
	})
	waitUntil(t, 8*time.Second, "agent 活跃配对回落", nil, func() bool {
		code, body, err := adminGet(agentManage, "/status?refresh=0")
		return err == nil && code == 200 && strings.Contains(body, "当前没有活跃配对")
	})

	// 运行中的 pprof 应可访问，且不应残留 forward.Bidirectional 栈帧。
	var runningProfile string
	waitUntil(t, 8*time.Second, "运行中 pprof 无残留转发 goroutine", nil, func() bool {
		code, body, err := adminGet(relayManage, "/debug/pprof/goroutine?debug=1")
		if err != nil || code != 200 {
			return false
		}
		runningProfile = body
		return strings.Contains(body, "goroutine profile: total") &&
			!strings.Contains(body, "internal/forward.Bidirectional")
	})
	total := "?"
	if m := goroutineTotalRe.FindStringSubmatch(runningProfile); len(m) == 2 {
		total = m[1]
	}
	t.Logf("运行中 pprof: relay goroutine total=%s，无 internal/forward.Bidirectional 残留", total)

	// 显式优雅关停（cleanup 会再次调用，幂等），并断言三个 Run 都返回 nil。
	for _, h := range []*runHandle{clientH, agentH, relayH} {
		h.stop(t, 15*time.Second)
		if err := h.result(); err != nil {
			t.Fatalf("%s.Run 在 ctx 取消后返回 %v，期望 nil", h.name, err)
		}
	}
	t.Log("优雅关停：relay/agent/client 三个 Run 在 ctx 取消后均返回 nil")

	// Run 退出后：runtime/pprof 中不应再有 PairLink 任何包的栈帧。
	var afterProfile string
	waitUntil(t, 8*time.Second, "Run 退出后 pprof 无 PairLink 残留", nil, func() bool {
		afterProfile = goroutineProfilePprof()
		for _, pkg := range []string{"/internal/relay", "/internal/agent", "/internal/client", "/internal/forward"} {
			if strings.Contains(afterProfile, pkg) {
				return false
			}
		}
		return true
	})
	for _, pkg := range []string{"/internal/relay", "/internal/agent", "/internal/client", "/internal/forward"} {
		if strings.Contains(afterProfile, pkg) {
			t.Fatalf("Run 退出后 goroutine profile 仍包含 %s:\n%s", pkg, afterProfile)
		}
	}

	after := runtime.NumGoroutine()
	if after > base+2 {
		t.Fatalf("goroutine 计数 base=%d after=%d 超过容差 +2", base, after)
	}
	t.Logf("goroutine 实测: 基线=%d Run 全部退出后=%d（容差 +2）", base, after)
	t.Logf("pprof 结论: 运行中无转发 goroutine 残留；退出后无 internal/relay|agent|client|forward 栈帧")
}

// TestE2EWSLOrCrossCompileNote 记录 WSL 可用性：本机若没有可用 WSL 发行版，
// 只记录检测结果（Linux 二进制仅交叉编译验证，见 README 已知限制）。
func TestE2EWSLOrCrossCompileNote(t *testing.T) {
	if ok, detail := wslAvailable(); ok {
		t.Logf("WSL 可用（%s）；本机未在 WSL 中额外执行三进程 echo（见 README 说明）", detail)
	} else {
		t.Logf("WSL 不可用（%s）；Linux 二进制仅做交叉编译验证", detail)
	}
}
