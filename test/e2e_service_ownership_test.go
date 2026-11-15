package e2e

import (
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
)

// waitEchoState 轮询经 client 本地端口能否真正 echo（want=true）或已经不可用（want=false）。
//
// 服务从订阅列表移除后 client 的本地监听可能仍在（只是配对会被 relay 拒绝），
// 因此不能用"端口可连"判断可用性，必须以是否收到回显为准。
func waitEchoState(t *testing.T, addr string, want bool, desc string) {
	t.Helper()
	waitUntil(t, 8*time.Second, desc, nil, func() bool {
		conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err != nil {
			return !want
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(800 * time.Millisecond))
		if _, err := conn.Write([]byte("probe")); err != nil {
			return !want
		}
		buf := make([]byte, 5)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return !want
		}
		return want && string(buf) == "probe"
	})
}

// TestE2EServiceOwnership 覆盖服务名归属（SEC-A2）的端到端行为：
// 同名服务被在线 agent 占用时第二个 agent 被拒；原 owner 下线后允许接管，
// client 的服务列表随之热更新（本地监听关闭/恢复）。
func TestE2EServiceOwnership(t *testing.T) {
	guardGoroutines(t)
	echoA := startTCPEchoServer(t)
	echoB := startTCPEchoServer(t)

	relayManage := hostPort(freeTCPPort(t))
	relayCfg, _ := writeRelayINI(t, relaySpec{
		Tunnel:       hostPort(freeTCPPort(t)),
		TCP:          hostPort(freeTCPPort(t)),
		Manage:       relayManage,
		ServiceAllow: []string{"default"},
		PairTimeout:  5 * time.Second,
	})
	relayH := startRelay(t, relayCfg)

	newAgentCfg := func(echoAddr string) *config.Config {
		cfg, _ := writeAgentINI(t, agentSpec{
			RelayAddr: relayCfg.Relay.TunnelListen,
			Manage:    hostPort(freeTCPPort(t)),
			Protocol:  "tcp",
			Local:     echoAddr,
		})
		return cfg
	}

	// 1) agent-A 注册 default，client 建立本地监听并可 echo。
	agentA := startAgent(t, newAgentCfg(echoA))
	waitServicesRegistered(t, relayH, relayManage, "default")

	clientPort := freeTCPPort(t)
	clientCfg, _ := writeClientINI(t, clientSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    hostPort(freeTCPPort(t)),
		Services:  []clientService{{ID: "default", Listen: hostPort(clientPort)}},
	})
	clientH := startClient(t, clientCfg)
	waitClientMappings(t, clientH, clientCfg.Common.ManageListen, map[string]string{
		"default": hostPort(clientPort),
	})
	conn := dialWithRetry(t, hostPort(clientPort))
	echoText(t, conn, "owner-a")
	_ = conn.Close()

	// 2) agent-B 用同名服务注册：必须被拒（原 owner 在线），且不挂起。
	agentB := startAgent(t, newAgentCfg(echoB))
	err := waitRunError(t, agentB, 8*time.Second)
	if err == nil {
		t.Fatal("同名服务在原 owner 在线时必须拒绝第二个 agent")
	}
	if !strings.Contains(err.Error(), "已被另一个在线 agent 占用") {
		t.Errorf("拒绝原因应说明服务被占用，实际 %q", err.Error())
	}
	if !strings.Contains(err.Error(), "service_allow") && !strings.Contains(err.Error(), "重复部署") {
		t.Errorf("拒绝提示应给出可执行方向，实际 %q", err.Error())
	}

	// 3) 原 owner 下线 → client 的本地监听关闭（服务列表热更新）。
	agentA.stop(t, 15*time.Second)
	waitEchoState(t, hostPort(clientPort), false, "原 owner 下线后服务不可用")

	// 4) agent-B 重新启动：原 owner 已离线，允许接管；client 监听恢复。
	agentB2 := startAgent(t, newAgentCfg(echoB))
	waitServicesRegistered(t, relayH, relayManage, "default")
	waitEchoState(t, hostPort(clientPort), true, "接管后服务恢复")
	conn = dialWithRetry(t, hostPort(clientPort))
	echoText(t, conn, "owner-b")
	_ = conn.Close()

	// 5) 反向：agent-A 在 agent-B 在线时重新启动 → 仍被拒。
	agentA2 := startAgent(t, newAgentCfg(echoA))
	err = waitRunError(t, agentA2, 8*time.Second)
	if err == nil {
		t.Fatal("接管者在位时原 owner 重启也必须被拒绝")
	}

	// 6) agent-B 下线后再启动 agent-A → 再次接管并恢复服务。
	agentB2.stop(t, 15*time.Second)
	waitEchoState(t, hostPort(clientPort), false, "接管者下线后服务不可用")
	agentA3 := startAgent(t, newAgentCfg(echoA))
	waitServicesRegistered(t, relayH, relayManage, "default")
	waitEchoState(t, hostPort(clientPort), true, "再次接管后服务恢复")
	conn = dialWithRetry(t, hostPort(clientPort))
	echoText(t, conn, "owner-a-again")
	_ = conn.Close()

	stopAll(t, clientH, agentA3, relayH)
}

// TestE2EServiceWhitelist 覆盖 [relay] service_allow：未列入白名单的服务名注册失败，
// agent 得到可读提示；已列入的主服务仍可正常注册。
func TestE2EServiceWhitelist(t *testing.T) {
	guardGoroutines(t)
	echoAddr := startTCPEchoServer(t)
	extraEcho := startTCPEchoServer(t)

	relayManage := hostPort(freeTCPPort(t))
	relayCfg, _ := writeRelayINI(t, relaySpec{
		Tunnel:       hostPort(freeTCPPort(t)),
		TCP:          hostPort(freeTCPPort(t)),
		Manage:       relayManage,
		ServiceAllow: []string{"default"},
	})
	relayH := startRelay(t, relayCfg)

	agentCfg, _ := writeAgentINI(t, agentSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Protocol:  "tcp",
		Local:     echoAddr,
		Services: []serverService{
			{ID: "svc-secret", Spec: "tcp:" + extraEcho},
		},
	})
	agentH := startAgent(t, agentCfg)

	err := waitRunError(t, agentH, 8*time.Second)
	if err == nil {
		t.Fatal("服务名不在 service_allow 白名单时必须注册失败")
	}
	if !strings.Contains(err.Error(), "service_allow") || !strings.Contains(err.Error(), "白名单") {
		t.Errorf("错误应指向 service_allow 白名单，实际 %q", err.Error())
	}
	if !strings.Contains(err.Error(), "svc-secret") {
		t.Errorf("错误应包含被拒绝的服务名，实际 %q", err.Error())
	}
	if !strings.Contains(err.Error(), "已被另一个") && strings.Contains(err.Error(), "占用") {
		t.Errorf("白名单拒绝不应误报为归属冲突，实际 %q", err.Error())
	}

	stopAll(t, relayH)
}
