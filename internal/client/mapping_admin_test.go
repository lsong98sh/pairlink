package client

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// writeClientTestINI 为动态映射测试准备一份真实 [client] ini。
func writeClientTestINI(t *testing.T, cfg *config.Config) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("[client]\n")
	b.WriteString("relay_addr = " + cfg.Client.RelayAddr + "\n")
	b.WriteString("token = " + cfg.Client.Token + "\n")
	for _, svc := range cfg.Client.Services {
		b.WriteString("service." + svc.ID + " = " + svc.Listen + "\n")
	}
	path := filepath.Join(t.TempDir(), "client.ini")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("写 client 测试 ini: %v", err)
	}
	cfg.Path = path
	return path
}

// dialEventually 在超时内重试拨号，返回是否成功。
func dialEventually(addr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// saveMappingEventually 分配端口并重试保存映射，规避其它测试进程并发占用端口。
func saveMappingEventually(t *testing.T, tc *testClient, id string) string {
	t.Helper()
	var lastErr error
	for i := 0; i < 50; i++ {
		addr := freeTCPAddr(t)
		if err := tc.c.saveMapping(id, addr); err != nil {
			lastErr = err
			continue
		}
		return addr
	}
	t.Fatalf("保存映射 %s 连续失败: %v", id, lastErr)
	return ""
}

// TestClientAdminMappingAddModifyDelete 覆盖新增/修改地址/删除映射与端口状态。
func TestClientAdminMappingAddModifyDelete(t *testing.T) {
	fr := newFakeRelay(t, fakeRelayOptions{
		services: []protocol.ServiceInfo{{ServiceID: "web", Proto: "tcp", AgentID: "a-1", Online: true}},
		revision: 1,
	})
	cfg := testConfig(fr.addr())
	iniPath := writeClientTestINI(t, cfg)
	tc := startTestClient(t, cfg)
	waitFor(t, 5*time.Second, "控制面就绪", func() bool {
		_, revision := tc.c.servicesView()
		return revision >= 1 && tc.c.controlConn() != nil
	})

	addr := saveMappingEventually(t, tc, "web")
	l := tc.c.listenerByService("web")
	if l == nil {
		t.Fatal("新增映射后没有本地监听")
	}
	actual := l.actualAddr()
	if !dialEventually(actual, 3*time.Second) {
		t.Fatalf("新增映射后端口 %s 不可连", actual)
	}
	if content, err := os.ReadFile(iniPath); err != nil || !strings.Contains(string(content), "service.web = "+addr) {
		t.Fatalf("ini 未写回新映射: %s (err=%v)", content, err)
	}

	// 修改地址：旧端口关闭，新端口可连，ini 更新。
	addr2 := saveMappingEventually(t, tc, "web")
	l2 := tc.c.listenerByService("web")
	if l2 == nil {
		t.Fatal("修改映射后没有本地监听")
	}
	actual2 := l2.actualAddr()
	if dialEventually(actual, time.Second) {
		t.Fatalf("修改地址后旧端口 %s 仍可连", actual)
	}
	if !dialEventually(actual2, 3*time.Second) {
		t.Fatalf("修改映射后新端口 %s 不可连", actual2)
	}
	if content, _ := os.ReadFile(iniPath); !strings.Contains(string(content), "service.web = "+addr2) {
		t.Fatalf("ini 未更新为新地址: %s", content)
	}

	// 删除：端口关闭，ini 中该行消失。
	if err := tc.c.deleteMapping("web"); err != nil {
		t.Fatalf("删除映射: %v", err)
	}
	if dialEventually(actual2, time.Second) {
		t.Fatalf("删除映射后端口 %s 仍可连", actual2)
	}
	if content, _ := os.ReadFile(iniPath); strings.Contains(string(content), "service.web") {
		t.Fatalf("删除后 ini 仍有映射: %s", content)
	}
	if _, ok := tc.c.mappingByID("web"); ok {
		t.Fatal("删除后运行期映射表仍有 web")
	}
}

// TestClientAdminMappingMissingServiceUsesTCP 覆盖服务列表缺失时按 TCP 占位 + WARN。
func TestClientAdminMappingMissingServiceUsesTCP(t *testing.T) {
	fr := newFakeRelay(t, fakeRelayOptions{services: nil})
	cfg := testConfig(fr.addr())
	writeClientTestINI(t, cfg)
	tc := startTestClient(t, cfg)
	waitFor(t, 5*time.Second, "控制面就绪", func() bool { return tc.c.controlConn() != nil })

	addr := saveMappingEventually(t, tc, "ghost")
	if got := tc.c.listenerProto("ghost"); got != "tcp" {
		t.Fatalf("缺失服务应按 TCP 占位，实际 %q", got)
	}
	l := tc.c.listenerByService("ghost")
	if l == nil {
		t.Fatal("缺失服务映射没有监听")
	}
	if !dialEventually(l.actualAddr(), 3*time.Second) {
		t.Fatalf("按 TCP 占位后端口 %s 不可连", l.actualAddr())
	}
	if addr == "" {
		t.Fatal("映射地址为空")
	}
	tc.logContains(t, "service_missing")
}

// TestClientAdminMappingPatchFailureRollsBack 覆盖写 ini 失败时关闭新监听并回滚映射。
func TestClientAdminMappingPatchFailureRollsBack(t *testing.T) {
	fr := newFakeRelay(t, fakeRelayOptions{services: nil})
	cfg := testConfig(fr.addr())
	writeClientTestINI(t, cfg)
	cfg.Path = filepath.Join(t.TempDir(), "missing", "client.ini")
	tc := startTestClient(t, cfg)
	waitFor(t, 5*time.Second, "控制面就绪", func() bool { return tc.c.controlConn() != nil })

	addr := freeTCPAddr(t)
	if err := tc.c.saveMapping("web", addr); err == nil {
		t.Fatal("写 ini 失败时 saveMapping 应返回错误")
	}
	if _, ok := tc.c.mappingByID("web"); ok {
		t.Fatal("写盘失败后不应保留映射")
	}
	if dialEventually(addr, 500*time.Millisecond) {
		t.Fatalf("写盘失败后监听 %s 应已关闭", addr)
	}
}

// TestValidateMappingForm 覆盖映射表单校验边界。
func TestValidateMappingForm(t *testing.T) {
	cases := []struct {
		id, listen string
		ok         bool
	}{
		{"web", "8080", true},
		{"web", ":8080", true},
		{"web", "127.0.0.1:8080", true},
		{"WEB", "8080", false},
		{"default", "8080", false},
		{"web", "127.0.0.1:0", false},
		{"web", "127.0.0.1", false},
		{"", "8080", false},
		{"web", strings.Repeat("1", 300), false},
	}
	for _, tc := range cases {
		_, addr, err := validateMappingForm(tc.id, tc.listen)
		if tc.ok && err != nil {
			t.Errorf("validateMappingForm(%q,%q) 应成功: %v", tc.id, tc.listen, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("validateMappingForm(%q,%q) 应失败", tc.id, tc.listen)
		}
		if tc.ok && tc.listen == "8080" && addr != "127.0.0.1:8080" {
			t.Errorf("裸端口应补 127.0.0.1，实际 %q", addr)
		}
	}
}
