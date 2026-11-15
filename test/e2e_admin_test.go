package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/version"
)

// TestE2EAdminPagesThreeModes 验收项（task.md §10）：三种模式的 manage_listen
// 页面均可访问并展示各自状态；页面不依赖任何外部脚本；pprof=true 时可用。
func TestE2EAdminPagesThreeModes(t *testing.T) {
	echoAddr := startTCPEchoServer(t)

	relayManage := hostPort(freeTCPPort(t))
	relayCfg, _ := writeRelayINI(t, relaySpec{
		Tunnel:      hostPort(freeTCPPort(t)),
		TCP:         hostPort(freeTCPPort(t)),
		Manage:      relayManage,
		Pprof:       true,
		PairTimeout: 5 * time.Second,
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
	startAgent(t, agentCfg)
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

	cases := []struct {
		name    string
		addr    string
		path    string
		title   string
		headers []string
	}{
		{
			name:  "relay",
			addr:  relayManage,
			path:  "/relay?refresh=0",
			title: "PairLink relay 管理页",
			headers: []string{
				"服务注册表", "在线 agent", "当前配对表", "UDP 会话表",
			},
		},
		{
			name:  "agent",
			addr:  agentManage,
			path:  "/status?refresh=0",
			title: "PairLink agent 管理页",
			headers: []string{
				"本机已注册服务", "控制连接", "当前活跃数据连接",
			},
		},
		{
			name:  "client",
			addr:  clientManage,
			path:  "/client?refresh=0",
			title: "PairLink client 管理页",
			headers: []string{
				"relay 返回的可用服务列表", "本地端口映射表", "当前活跃配对",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body, err := adminGet(tc.addr, tc.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			if code != 200 {
				t.Fatalf("GET %s 状态码 = %d，期望 200", tc.path, code)
			}
			if !strings.Contains(body, `<title>`+tc.title+`</title>`) {
				t.Fatalf("%s 页面标题不是 %q", tc.name, tc.title)
			}
			if !strings.Contains(body, `<html lang="zh-CN">`) {
				t.Fatalf("%s 页面缺少中文 lang 声明", tc.name)
			}
			for _, h := range tc.headers {
				if !strings.Contains(body, h) {
					t.Fatalf("%s 页面缺少关键表头/分区: %q", tc.name, h)
				}
			}
			if strings.Contains(body, "<script src=") {
				t.Fatalf("%s 页面引用了外部脚本（应全部内联）", tc.name)
			}
			// 阶段 4：三种模式的顶栏都必须显示统一版本号（internal/version）。
			if !strings.Contains(body, version.Version) {
				t.Fatalf("%s 页面顶栏缺少版本号 %s", tc.name, version.Version)
			}
			// 管理页首页也应可访问并列出页面入口。
			idx := mustAdminGet(t, tc.addr, "/?refresh=0")
			if !strings.Contains(idx, "页面") || !strings.Contains(idx, "运行时长") {
				t.Fatalf("%s 管理页首页内容不完整", tc.name)
			}
			t.Logf("%s 管理页 200，标题=%q，关键表头齐全，无外部脚本", tc.name, tc.title)

			// pprof 只在 pprof=true 时挂载。
			code, profile, err := adminGet(tc.addr, "/debug/pprof/goroutine?debug=1")
			if err != nil {
				t.Fatalf("%s GET /debug/pprof/goroutine?debug=1: %v", tc.name, err)
			}
			if code != 200 {
				t.Fatalf("%s pprof 状态码 = %d，期望 200", tc.name, code)
			}
			if !strings.Contains(profile, "goroutine profile: total") {
				t.Fatalf("%s pprof 输出不像 goroutine profile: %.200q", tc.name, profile)
			}
			index := mustAdminGet(t, tc.addr, "/debug/pprof/")
			if !strings.Contains(index, "Types of profiles available") {
				t.Fatalf("%s /debug/pprof/ 未展示 profile 列表", tc.name)
			}
		})
	}
}
