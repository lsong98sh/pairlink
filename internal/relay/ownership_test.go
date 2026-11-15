package relay

import (
	"strings"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// TestRelayServiceAllowWhitelist 覆盖 [relay] service_allow 白名单（SEC-A2）。
func TestRelayServiceAllowWhitelist(t *testing.T) {
	logs := &lockedBuffer{}
	cfg := testConfig(t, func(c *config.Config) {
		c.Relay.ServiceAllow = []string{"svc-ok", "svc-two"}
	})
	s := startRelayWithLogs(t, cfg, logs)

	agent := dialControl(t, s.tunnelAddr)
	agent.hello(protocol.RoleAgent, testToken)

	denied := agent.registerExpectError("svc-other", "tcp", protocol.ErrDetailServiceDenied)
	if !strings.Contains(denied.Message, "service_allow") {
		t.Errorf("白名单拒绝的 message 应说明 service_allow，实际 %q", denied.Message)
	}
	if _, ok := s.services.Get("svc-other"); ok {
		t.Fatal("被白名单拒绝的服务不得进入注册表")
	}

	agent.register("svc-ok", "tcp")
	if entry, ok := s.services.Get("svc-ok"); !ok || entry.AgentID == "" {
		t.Fatal("白名单内的服务应注册成功")
	}

	out := logs.String()
	if !strings.Contains(out, "service_denied") {
		t.Errorf("日志应记录 service_denied 事件:\n%s", out)
	}
	if !strings.Contains(out, "svc-other") {
		t.Errorf("日志应包含被拒绝的服务名:\n%s", out)
	}
}

// TestRelayMaxServicesPerAgent 覆盖单 agent 服务数上限（SEC-A2）。
func TestRelayMaxServicesPerAgent(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Relay.ServiceAllow = []string{"svc-1", "svc-2", "svc-3", "svc-4"}
		c.Relay.MaxServicesPerAgent = 2
	})
	s := startTestServer(t, cfg)

	agent1 := dialControl(t, s.tunnelAddr)
	agent1.hello(protocol.RoleAgent, testToken)
	agent1.register("svc-1", "tcp")
	agent1.register("svc-2", "udp")

	// 同一 owner 覆盖同一个服务不重复计数。
	agent1.register("svc-1", "tcp")

	limited := agent1.registerExpectError("svc-3", "tcp", protocol.ErrDetailServiceLimit)
	if !strings.Contains(limited.Message, "2") {
		t.Errorf("上限错误应带上限数值，实际 %q", limited.Message)
	}
	if _, ok := s.services.Get("svc-3"); ok {
		t.Fatal("超限服务不得进入注册表")
	}

	// 上限按 agent 计算：另一个 agent 仍可注册。
	agent2 := dialControl(t, s.tunnelAddr)
	agent2.hello(protocol.RoleAgent, testToken)
	agent2.register("svc-4", "tcp")
}

// TestRelayServiceOwnershipConflict 覆盖"异 owner 在线 → 拒绝"。
func TestRelayServiceOwnershipConflict(t *testing.T) {
	logs := &lockedBuffer{}
	cfg := testConfig(t, func(c *config.Config) {
		c.Relay.ServiceAllow = []string{"svc-owner"}
	})
	s := startRelayWithLogs(t, cfg, logs)

	agent1 := dialControl(t, s.tunnelAddr)
	ok1 := agent1.hello(protocol.RoleAgent, testToken)
	agent1.register("svc-owner", "tcp")

	agent2 := dialControl(t, s.tunnelAddr)
	ok2 := agent2.hello(protocol.RoleAgent, testToken)

	conflict := agent2.registerExpectError("svc-owner", "tcp", protocol.ErrDetailServiceConflict)
	if !strings.Contains(conflict.Message, "其它 agent") {
		t.Errorf("冲突错误应说明服务已由其它 agent 注册，实际 %q", conflict.Message)
	}
	entry, _ := s.services.Get("svc-owner")
	if entry.AgentID != ok1.AgentID {
		t.Fatalf("服务归属被改写：owner=%q，期望 %q", entry.AgentID, ok1.AgentID)
	}

	// 原 owner 自己重复注册（覆盖）仍允许。
	agent1.register("svc-owner", "tcp")

	out := logs.String()
	if !strings.Contains(out, "service_conflict") {
		t.Errorf("日志应记录 service_conflict 事件:\n%s", out)
	}
	if !strings.Contains(out, ok1.AgentID) || !strings.Contains(out, ok2.AgentID) {
		t.Errorf("冲突事件应同时记录两个 agentID（%s / %s）:\n%s", ok1.AgentID, ok2.AgentID, out)
	}
}

// TestRelayServiceTakeoverAfterDisconnect 覆盖"异 owner 离线 → 允许接管"。
func TestRelayServiceTakeoverAfterDisconnect(t *testing.T) {
	logs := &lockedBuffer{}
	cfg := testConfig(t, func(c *config.Config) {
		c.Relay.ServiceAllow = []string{"svc-owner"}
		c.Relay.HeartbeatTimeout = 1 * time.Second
	})
	s := startRelayWithLogs(t, cfg, logs)

	agent1 := dialControl(t, s.tunnelAddr)
	ok1 := agent1.hello(protocol.RoleAgent, testToken)
	agent1.register("svc-owner", "tcp")
	agent1.sync()

	agent2 := dialControl(t, s.tunnelAddr)
	ok2 := agent2.hello(protocol.RoleAgent, testToken)
	agent2.registerExpectError("svc-owner", "tcp", protocol.ErrDetailServiceConflict)

	// 断开原 owner：relay 注销其全部服务并记录"最近归属"。
	_ = agent1.conn.Close()
	waitFor(t, 3*time.Second, "原 owner 的服务被注销", func() bool { return s.services.Len() == 0 })

	owner, at, ok := s.services.RecentOwner("svc-owner")
	if !ok || owner != ok1.AgentID {
		t.Fatalf("最近归属 = %q ok=%v，期望 %q", owner, ok, ok1.AgentID)
	}
	if time.Since(at) > 5*time.Second {
		t.Errorf("最近归属记录时间过旧: %v", at)
	}

	agent2.register("svc-owner", "tcp")
	entry, exists := s.services.Get("svc-owner")
	if !exists || entry.AgentID != ok2.AgentID {
		t.Fatalf("接管后归属 = %+v，期望 %q", entry, ok2.AgentID)
	}
	if got := s.services.ListByAgent(ok2.AgentID); len(got) != 1 || got[0].ID != "svc-owner" {
		t.Errorf("接管者的服务列表未更新: %+v", got)
	}

	out := logs.String()
	if !strings.Contains(out, "service_takeover") {
		t.Errorf("日志应记录 service_takeover 事件:\n%s", out)
	}
	if !strings.Contains(out, ok1.AgentID) || !strings.Contains(out, ok2.AgentID) {
		t.Errorf("接管事件应同时记录旧/新 owner（%s / %s）:\n%s", ok1.AgentID, ok2.AgentID, out)
	}
}

// TestRelayServiceRegistryTakeover 覆盖"原 owner 在线但主动注销 → 允许新 agent 注册"。
//
// 主动注销等于释放服务名：不产生 service_takeover（不是抢占离线 owner 的服务）。
func TestRelayServiceRegistryTakeover(t *testing.T) {
	logs := &lockedBuffer{}
	cfg := testConfig(t, func(c *config.Config) {
		c.Relay.ServiceAllow = []string{"svc-release"}
	})
	s := startRelayWithLogs(t, cfg, logs)

	agent1 := dialControl(t, s.tunnelAddr)
	agent1.hello(protocol.RoleAgent, testToken)
	agent1.register("svc-release", "tcp")

	agent1.send(protocol.MsgUnregisterService, &protocol.UnregisterService{ServiceID: "svc-release"})
	typ, msg := agent1.read(2 * time.Second)
	if typ != protocol.MsgServiceUnregistered {
		t.Fatalf("期望 ServiceUnregistered，实际 %s: %v", typ, msg)
	}

	agent2 := dialControl(t, s.tunnelAddr)
	agent2.hello(protocol.RoleAgent, testToken)
	agent2.register("svc-release", "tcp")

	if out := logs.String(); strings.Contains(out, "service_takeover") {
		t.Errorf("主动注销释放不应产生 service_takeover 事件:\n%s", out)
	}
}
