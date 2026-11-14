package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
)

// writeAgentTestINI 为动态服务测试准备一份真实 ini，并返回与之一致的配置。
//
// cfg.Path 指向该文件，PatchValue/DeleteKey 会真实改写它。
func writeAgentTestINI(t *testing.T, relayAddr string, services ...config.ServerService) (*config.Config, string) {
	t.Helper()
	cfg := newTestConfig(relayAddr, services...)
	var b strings.Builder
	b.WriteString("[server]\n")
	b.WriteString("relay_addr = " + cfg.Server.RelayAddr + "\n")
	b.WriteString("token = " + cfg.Server.Token + "\n")
	if cfg.Server.Local != "" {
		b.WriteString("protocol = " + cfg.Server.Protocol + "\n")
		b.WriteString("local = " + cfg.Server.Local + "\n")
	}
	for _, svc := range cfg.Server.Services {
		b.WriteString("service." + svc.ID + " = " + svc.Proto + ":" + svc.Addr + "\n")
	}
	path := filepath.Join(t.TempDir(), "server.ini")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("写 agent 测试 ini: %v", err)
	}
	cfg.Path = path
	return cfg, path
}

// readFile 读取测试 ini。
func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 ini: %v", err)
	}
	return string(data)
}

// startAgentAndWait 启动 agent，并等待 relay 收到全部初始注册。
func startAgentAndWait(t *testing.T, cfg *config.Config, fr *fakeRelay) *agentRun {
	t.Helper()
	run := startAgent(t, cfg)
	for i := 0; i < len(cfg.Server.All()); i++ {
		select {
		case <-fr.registrations:
		case <-time.After(5 * time.Second):
			t.Fatalf("等待第 %d 个初始服务注册超时", i+1)
		}
	}
	waitCond(t, 5*time.Second, func() bool { return run.a.ctrlReady.Load() }, "控制连接就绪")
	return run
}

// TestAgentAdminAddAndDeleteService 覆盖新增/删除服务 → 假 relay 收到请求 + ini 变化。
func TestAgentAdminAddAndDeleteService(t *testing.T) {
	mainAddr := startTCPEcho(t)
	extraAddr := startTCPEcho(t)
	fr := newFakeRelay(t, relayOptions{})
	cfg, iniPath := writeAgentTestINI(t, fr.tunnelAddr(), config.ServerService{ID: config.DefaultServiceID, Proto: "tcp", Addr: mainAddr})
	run := startAgentAndWait(t, cfg, fr)

	if err := run.a.saveService("web", "tcp", extraAddr); err != nil {
		t.Fatalf("saveService: %v", err)
	}
	select {
	case req := <-fr.registrations:
		if req.ServiceID != "web" || req.Proto != "tcp" {
			t.Fatalf("假 relay 收到的注册请求不正确: %+v", req)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("假 relay 未收到新服务的注册请求")
	}
	if svc, ok := run.a.svcStore.get("web"); !ok || svc.Addr != extraAddr {
		t.Fatalf("本地服务表未包含新服务: %+v ok=%v", svc, ok)
	}
	content := readFile(t, iniPath)
	if !strings.Contains(content, "service.web = tcp:"+extraAddr) {
		t.Fatalf("ini 未写回新服务:\n%s", content)
	}

	if err := run.a.deleteService("web"); err != nil {
		t.Fatalf("deleteService: %v", err)
	}
	select {
	case req := <-fr.unregistrations:
		if req.ServiceID != "web" {
			t.Fatalf("假 relay 收到的注销请求不正确: %+v", req)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("假 relay 未收到注销请求")
	}
	if _, ok := run.a.svcStore.get("web"); ok {
		t.Fatal("删除后本地服务表仍有该服务")
	}
	if content := readFile(t, iniPath); strings.Contains(content, "service.web") {
		t.Fatalf("删除后 ini 仍有该服务:\n%s", content)
	}
}

// TestAgentAdminServiceFailuresKeepState 覆盖 relay 拒绝/超时/注销失败时内存与 ini 不变。
func TestAgentAdminServiceFailuresKeepState(t *testing.T) {
	mainAddr := startTCPEcho(t)
	extraAddr := startTCPEcho(t)
	fr := newFakeRelay(t, relayOptions{
		failServiceID:    "rejected",
		silentServiceID:  "slow",
		failUnregisterID: "keep",
	})
	cfg, iniPath := writeAgentTestINI(t, fr.tunnelAddr(),
		config.ServerService{ID: config.DefaultServiceID, Proto: "tcp", Addr: mainAddr},
		config.ServerService{ID: "keep", Proto: "tcp", Addr: extraAddr},
	)
	run := startAgentAndWait(t, cfg, fr)
	before := readFile(t, iniPath)

	// relay 明确拒绝（MsgError）。
	if err := run.a.saveService("rejected", "tcp", extraAddr); err == nil {
		t.Fatal("relay 拒绝时 saveService 应返回错误")
	}
	if _, ok := run.a.svcStore.get("rejected"); ok {
		t.Fatal("失败后本地服务表不应包含 rejected")
	}
	if got := readFile(t, iniPath); got != before {
		t.Fatalf("失败后 ini 不应改变:\n%s", got)
	}

	// 超时（假 relay 不回应）。
	start := time.Now()
	if err := run.a.saveService("slow", "tcp", extraAddr); err == nil {
		t.Fatal("等待确认超时时 saveService 应返回错误")
	}
	if elapsed := time.Since(start); elapsed < serviceOpTimeout/2 {
		t.Fatalf("超时用例返回过早: %s", elapsed)
	}
	if _, ok := run.a.svcStore.get("slow"); ok {
		t.Fatal("超时后本地服务表不应包含 slow")
	}
	if got := readFile(t, iniPath); got != before {
		t.Fatalf("超时后 ini 不应改变:\n%s", got)
	}

	// 注销被拒：本地表与 ini 都保留。
	if err := run.a.deleteService("keep"); err == nil {
		t.Fatal("relay 拒绝注销时 deleteService 应返回错误")
	}
	if _, ok := run.a.svcStore.get("keep"); !ok {
		t.Fatal("注销失败后本地服务表应保留 keep")
	}
	if got := readFile(t, iniPath); got != before {
		t.Fatalf("注销失败后 ini 应保留:\n%s", got)
	}
}

// TestAgentAdminMainService 覆盖主服务 protocol/local 修改并写回 ini。
func TestAgentAdminMainService(t *testing.T) {
	mainAddr := startTCPEcho(t)
	newAddr := startTCPEcho(t)
	fr := newFakeRelay(t, relayOptions{})
	cfg, iniPath := writeAgentTestINI(t, fr.tunnelAddr(), config.ServerService{ID: config.DefaultServiceID, Proto: "tcp", Addr: mainAddr})
	run := startAgentAndWait(t, cfg, fr)

	if err := run.a.saveMainService("udp", newAddr); err != nil {
		t.Fatalf("saveMainService: %v", err)
	}
	select {
	case req := <-fr.registrations:
		if req.ServiceID != config.DefaultServiceID || req.Proto != "udp" {
			t.Fatalf("主服务注册请求不正确: %+v", req)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("假 relay 未收到主服务注册请求")
	}
	if svc, ok := run.a.svcStore.get(config.DefaultServiceID); !ok || svc.Proto != "udp" || svc.Addr != newAddr {
		t.Fatalf("主服务运行时未更新: %+v ok=%v", svc, ok)
	}
	content := readFile(t, iniPath)
	if !strings.Contains(content, "protocol = udp") || !strings.Contains(content, "local = "+newAddr) {
		t.Fatalf("ini 未写回主服务:\n%s", content)
	}

	// 再改回 tcp，确认重复修改也正常。
	if err := run.a.saveMainService("tcp", mainAddr); err != nil {
		t.Fatalf("再次 saveMainService: %v", err)
	}
	select {
	case <-fr.registrations:
	case <-time.After(3 * time.Second):
		t.Fatal("假 relay 未收到第二次主服务注册请求")
	}
	content = readFile(t, iniPath)
	if !strings.Contains(content, "protocol = tcp") || !strings.Contains(content, "local = "+mainAddr) {
		t.Fatalf("ini 未写回第二次主服务:\n%s", content)
	}
}

// TestAgentAdminPatchFailureRollsBack 覆盖写 ini 失败时回滚本地表与 relay 注册。
func TestAgentAdminPatchFailureRollsBack(t *testing.T) {
	mainAddr := startTCPEcho(t)
	extraAddr := startTCPEcho(t)
	fr := newFakeRelay(t, relayOptions{})
	cfg, _ := writeAgentTestINI(t, fr.tunnelAddr(), config.ServerService{ID: config.DefaultServiceID, Proto: "tcp", Addr: mainAddr})
	cfg.Path = filepath.Join(t.TempDir(), "missing", "server.ini") // 目录不存在 → PatchValue 失败
	run := startAgentAndWait(t, cfg, fr)

	if err := run.a.saveService("web", "tcp", extraAddr); err == nil {
		t.Fatal("写 ini 失败时 saveService 应返回错误")
	}
	if _, ok := run.a.svcStore.get("web"); ok {
		t.Fatal("写盘失败后应回滚本地服务表")
	}
	// 回滚会向 relay 发注销请求。
	select {
	case req := <-fr.unregistrations:
		if req.ServiceID != "web" {
			t.Fatalf("回滚注销请求不正确: %+v", req)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("写盘失败后未向 relay 注销")
	}
}

// TestValidateServiceForm 覆盖表单校验边界。
func TestValidateServiceForm(t *testing.T) {
	if _, err := validateServiceForm("WEB", "tcp", "127.0.0.1:80"); err == nil {
		t.Error("大写服务ID应被拒绝")
	}
	if _, err := validateServiceForm("default", "tcp", "127.0.0.1:80"); err == nil {
		t.Error("default 应被拒绝")
	}
	if _, err := validateServiceForm("web", "http", "127.0.0.1:80"); err == nil {
		t.Error("非法协议应被拒绝")
	}
	if _, err := validateServiceForm("web", "tcp", "127.0.0.1"); err == nil {
		t.Error("缺端口应被拒绝")
	}
	if _, err := validateServiceForm("web", "tcp", "127.0.0.1:0"); err == nil {
		t.Error("端口 0 应被拒绝")
	}
	if _, err := validateServiceForm("web", "tcp", strings.Repeat("a", 300)); err == nil {
		t.Error("超长地址应被拒绝")
	}
	svc, err := validateServiceForm("web", "TCP", "8080")
	if err != nil || svc.Addr != "127.0.0.1:8080" {
		t.Fatalf("合法输入应规范化: %+v %v", svc, err)
	}
}
