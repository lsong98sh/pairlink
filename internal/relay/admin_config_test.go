package relay

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/pairing"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// writeRelayINI 在临时目录写一份 relay ini 并返回路径。
func writeRelayINI(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "relay.ini")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写 relay ini: %v", err)
	}
	return path
}

// TestSetTimeoutAndTTL 覆盖 PairingTable.SetTimeout / token.Store.SetTTL 的运行期生效。
func TestSetTimeoutAndTTL(t *testing.T) {
	cfg := testConfig(t, nil)
	s := newServer(cfg)

	if got := s.pairTimeout(); got != 2*time.Second {
		t.Fatalf("初始 pair_timeout = %v", got)
	}
	if err := s.pairs.SetTimeout(15 * time.Second); err != nil {
		t.Fatalf("SetTimeout: %v", err)
	}
	if got := s.pairs.Timeout(); got != 15*time.Second {
		t.Fatalf("SetTimeout 后 Timeout = %v", got)
	}
	if err := s.pairs.SetTimeout(0); err == nil {
		t.Fatal("SetTimeout(0) 应报错")
	}
	if got := s.pairs.Timeout(); got != 15*time.Second {
		t.Fatalf("非法 SetTimeout 不应改变值，实际 %v", got)
	}

	// 新占位使用新的超时：把时钟固定后 Claim，检查 deadline。
	now := time.Now()
	s.pairs.SetClock(func() time.Time { return now })
	key := pairing.PairingKey{ServiceID: "svc", ClientConnID: "c-1"}
	if _, err := s.pairs.Claim(key, pairing.SideAgent, nil); err != pairing.ErrWaiting {
		t.Fatalf("首次 Claim 应 ErrWaiting，实际 %v", err)
	}
	deadline := now.Add(15 * time.Second)
	found := false
	for _, snap := range s.pairs.Snapshot() {
		if snap.Key == key {
			found = true
			if !snap.Deadline.Equal(deadline) {
				t.Fatalf("占位 deadline = %v, want %v", snap.Deadline, deadline)
			}
		}
	}
	if !found {
		t.Fatal("未找到刚创建的配对占位")
	}

	if err := s.tokens.SetTTL(time.Second); err != nil {
		t.Fatalf("SetTTL: %v", err)
	}
	if got := s.tokens.TTL(); got != time.Second {
		t.Fatalf("SetTTL 后 TTL = %v", got)
	}
	if err := s.tokens.SetTTL(-1); err == nil {
		t.Fatal("SetTTL(-1) 应报错")
	}
	if got := s.tokens.TTL(); got != time.Second {
		t.Fatalf("非法 SetTTL 不应改变值，实际 %v", got)
	}

	tokNow := time.Now()
	s.tokens.SetClock(func() time.Time { return tokNow })
	tok, err := s.tokens.Issue(key)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := s.tokens.Validate(key, tok); err != nil {
		t.Fatalf("有效期内应校验通过: %v", err)
	}
	tokNow = tokNow.Add(2 * time.Second)
	if err := s.tokens.Validate(key, tok); err == nil {
		t.Fatal("超过新 TTL 后应校验失败")
	}
}

// TestUnregisterService 覆盖 MsgUnregisterService 正常路径与 removed 事件。
func TestUnregisterService(t *testing.T) {
	cfg := testConfig(t, nil)
	s := startTestServer(t, cfg)

	agent := dialControl(t, s.tunnelAddr)
	agent.hello(protocol.RoleAgent, testToken)
	agent.register("svc-a", "tcp")

	client := dialControl(t, s.tunnelAddr)
	client.hello(protocol.RoleClient, testToken)
	client.send(protocol.MsgSubscribe, &protocol.Subscribe{Enable: true})
	// 订阅基线：全量列表。
	if typ, msg := client.read(2 * time.Second); typ != protocol.MsgServiceList {
		t.Fatalf("订阅后应收到全量 ServiceList，实际 %s: %v", typ, msg)
	}

	agent.send(protocol.MsgUnregisterService, &protocol.UnregisterService{ServiceID: "svc-a"})
	typ, msg := agent.read(2 * time.Second)
	if typ != protocol.MsgServiceUnregistered {
		t.Fatalf("期望 ServiceUnregistered，实际 %s: %v", typ, msg)
	}
	unreg, ok := msg.(*protocol.ServiceUnregistered)
	if !ok || unreg.ServiceID != "svc-a" {
		t.Fatalf("注销确认内容错误: %+v", msg)
	}
	if _, exists := s.services.Get("svc-a"); exists {
		t.Fatal("注销后服务仍在注册表")
	}

	typ, msg = client.read(2 * time.Second)
	if typ != protocol.MsgServiceEvent {
		t.Fatalf("订阅者应收到 removed 事件，实际 %s: %v", typ, msg)
	}
	ev, ok := msg.(*protocol.ServiceEvent)
	if !ok || ev.Event != protocol.EventRemoved || ev.Service.ServiceID != "svc-a" {
		t.Fatalf("removed 事件内容错误: %+v", msg)
	}
}

// TestUnregisterServiceOwnerMismatch 覆盖非 owner 注销被拒绝且服务保留。
func TestUnregisterServiceOwnerMismatch(t *testing.T) {
	cfg := testConfig(t, nil)
	s := startTestServer(t, cfg)

	owner := dialControl(t, s.tunnelAddr)
	owner.hello(protocol.RoleAgent, testToken)
	owner.register("svc-b", "tcp")

	other := dialControl(t, s.tunnelAddr)
	other.hello(protocol.RoleAgent, testToken)
	other.send(protocol.MsgUnregisterService, &protocol.UnregisterService{ServiceID: "svc-b"})
	other.expectError(protocol.ErrCodeBadRequest)

	if _, exists := s.services.Get("svc-b"); !exists {
		t.Fatal("owner 不符时服务不应被删除")
	}

	// 注销不存在的服务同样回 bad_request。
	owner.send(protocol.MsgUnregisterService, &protocol.UnregisterService{ServiceID: "nope"})
	owner.expectError(protocol.ErrCodeBadRequest)
}

// TestPairTimeoutRuntimeAffectsWaitingConn 覆盖运行期修改 pair_timeout 后，
// 只报到一侧的数据连接按新超时关闭。
func TestPairTimeoutRuntimeAffectsWaitingConn(t *testing.T) {
	cfg := testConfig(t, nil)
	s := startTestServer(t, cfg)

	agent := dialControl(t, s.tunnelAddr)
	agent.hello(protocol.RoleAgent, testToken)
	agent.register("svc-timeout", "tcp")
	client := dialControl(t, s.tunnelAddr)
	client.hello(protocol.RoleClient, testToken)

	incoming, _ := issuePair(t, agent, client, "svc-timeout", "conn-timeout")
	if err := s.pairs.SetTimeout(300 * time.Millisecond); err != nil {
		t.Fatalf("SetTimeout: %v", err)
	}
	clientData := bindData(t, "127.0.0.1", s.tcpPort, &protocol.Bind{
		PairingKey:   incoming.PairingKey,
		PairingToken: incoming.PairingToken,
		Role:         protocol.RoleClient,
	})
	start := time.Now()
	expectClosed(t, clientData, 3*time.Second)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("连接关闭耗时 %s，pair_timeout 似乎未生效", elapsed)
	}
}

// TestAdminConfigAction 覆盖管理页修改 relay 参数并写回 ini。
func TestAdminConfigAction(t *testing.T) {
	path := writeRelayINI(t, "; relay 配置\n[relay]\ntunnel_listen = 127.0.0.1:0\ntcp_listen = 127.0.0.1:0\npair_timeout      = 2s\n")
	cfg := testConfig(t, func(c *config.Config) {
		c.Common.ManageListen = "127.0.0.1:0"
		c.Path = path
	})
	s := startTestServer(t, cfg)

	status, body := httpPostForm(t, s.admin.Addr(), "/action/relay-config", map[string][]string{
		"csrf_token": {s.admin.CSRFToken()},
		"key":        {"pair_timeout"},
		"value":      {"30s"},
	})
	if status != 200 {
		t.Fatalf("POST 状态码 = %d", status)
	}
	if !strings.Contains(body, "操作成功") || !strings.Contains(body, "30s") {
		t.Fatalf("页面缺少成功提示: %s", body)
	}
	if got := s.pairTimeout(); got != 30*time.Second {
		t.Fatalf("运行时 pair_timeout = %v, want 30s", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// v0.5.0 起写回用 gopkg.in/ini.v1 重新生成文件：不再保持手工对齐，
	// 但必须写入新值，且注释与其它键仍在（格式归一，见 docs/contract.md §3.10）。
	if !strings.Contains(string(data), "pair_timeout = 30s") {
		t.Fatalf("ini 未写回新值:\n%s", data)
	}
	if !strings.Contains(string(data), "; relay 配置") {
		t.Fatalf("注释丢失:\n%s", data)
	}

	// 设置 idle/heartbeat/udp/token 参数，验证运行时读取点。
	for _, tc := range []struct {
		key, value string
		check      func() time.Duration
		want       time.Duration
	}{
		{"idle_timeout", "45s", s.idleTimeoutValue, 45 * time.Second},
		{"heartbeat_timeout", "20s", s.heartbeatTimeoutValue, 20 * time.Second},
		{"udp_session_timeout", "11s", s.udpSessionTimeoutValue, 11 * time.Second},
		{"token_ttl", "7s", s.tokenTTL, 7 * time.Second},
	} {
		status, body := httpPostForm(t, s.admin.Addr(), "/action/relay-config", map[string][]string{
			"csrf_token": {s.admin.CSRFToken()},
			"key":        {tc.key},
			"value":      {tc.value},
		})
		if status != 200 || !strings.Contains(body, "操作成功") {
			t.Fatalf("修改 %s 失败: status=%d body=%s", tc.key, status, body)
		}
		if got := tc.check(); got != tc.want {
			t.Fatalf("%s 运行时 = %v, want %v", tc.key, got, tc.want)
		}
	}
}

// TestAdminConfigActionValidation 覆盖白名单/非法值/写盘失败时不改运行时。
func TestAdminConfigActionValidation(t *testing.T) {
	path := writeRelayINI(t, "[relay]\npair_timeout = 2s\n")
	cfg := testConfig(t, func(c *config.Config) {
		c.Common.ManageListen = "127.0.0.1:0"
		c.Path = path
	})
	s := startTestServer(t, cfg)

	// 非白名单键 → 400。
	status, _ := httpPostForm(t, s.admin.Addr(), "/action/relay-config", map[string][]string{
		"csrf_token": {s.admin.CSRFToken()},
		"key":        {"auth_token"},
		"value":      {"x"},
	})
	if status != 400 {
		t.Fatalf("非白名单键状态码 = %d, want 400", status)
	}
	// 未知表单字段 → 400。
	status, _ = httpPostForm(t, s.admin.Addr(), "/action/relay-config", map[string][]string{
		"csrf_token": {s.admin.CSRFToken()},
		"key":        {"pair_timeout"},
		"value":      {"5s"},
		"evil":       {"1"},
	})
	if status != 400 {
		t.Fatalf("未知字段状态码 = %d, want 400", status)
	}
	// 非法 duration → 页面报错且运行时/ini 不变。
	status, body := httpPostForm(t, s.admin.Addr(), "/action/relay-config", map[string][]string{
		"csrf_token": {s.admin.CSRFToken()},
		"key":        {"pair_timeout"},
		"value":      {"0s"},
	})
	if status != 200 || !strings.Contains(body, "操作失败") {
		t.Fatalf("非法 duration 应显示失败: status=%d body=%s", status, body)
	}
	if got := s.pairTimeout(); got != 2*time.Second {
		t.Fatalf("非法 duration 不应改变运行时: %v", got)
	}

	// 写 ini 失败（路径不存在）→ 运行时不变。
	s.cfg.Path = filepath.Join(t.TempDir(), "missing", "relay.ini")
	status, body = httpPostForm(t, s.admin.Addr(), "/action/relay-config", map[string][]string{
		"csrf_token": {s.admin.CSRFToken()},
		"key":        {"pair_timeout"},
		"value":      {"9s"},
	})
	if status != 200 || !strings.Contains(body, "操作失败") || !strings.Contains(body, "运行时未改变") {
		t.Fatalf("写盘失败应提示运行时未改变: status=%d body=%s", status, body)
	}
	if got := s.pairTimeout(); got != 2*time.Second {
		t.Fatalf("写盘失败后运行时 = %v, want 2s", got)
	}
}
