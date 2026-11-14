package client

import (
	"errors"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// tcpService 返回一个在线 TCP 服务的列表项。
func tcpService(id string) protocol.ServiceInfo {
	return protocol.ServiceInfo{ServiceID: id, Proto: "tcp", AgentID: "a-0123456789abcdef", Online: true}
}

// udpService 返回一个在线 UDP 服务的列表项。
func udpService(id string) protocol.ServiceInfo {
	return protocol.ServiceInfo{ServiceID: id, Proto: "udp", AgentID: "a-0123456789abcdef", Online: true}
}

// TestRunHandshake 覆盖 Hello 成功路径：role/token/version/主机名，以及后续 ListServices。
func TestRunHandshake(t *testing.T) {
	r := newFakeRelay(t, fakeRelayOptions{
		services: []protocol.ServiceInfo{tcpService("svc-a")},
		revision: 7,
		udpMode:  protocol.UDPModeNative,
	})
	cfg := testConfig(r.addr())
	tc := startTestClient(t, cfg)

	hello := r.helloMsg(t)
	if hello.Role != protocol.RoleClient {
		t.Fatalf("Hello.Role = %q, 期望 client", hello.Role)
	}
	if hello.Token != "test-token" {
		t.Fatalf("Hello.Token = %q, 期望 test-token", hello.Token)
	}
	if hello.Version != protocol.ProtocolVersion {
		t.Fatalf("Hello.Version = %d, 期望 %d", hello.Version, protocol.ProtocolVersion)
	}
	if strings.TrimSpace(hello.ClientName) == "" {
		t.Fatalf("Hello.ClientName 不应为空（应为 os.Hostname()）")
	}

	// 服务列表已被拉取（revision 基线 7）。
	waitFor(t, 3*time.Second, "服务列表基线", func() bool {
		svc, rev := tc.c.servicesView()
		_, ok := svc["svc-a"]
		return ok && rev == 7
	})

	if err := tc.stop(); err != nil {
		t.Fatalf("ctx 取消后 Run 应返回 nil，实际 %v", err)
	}
	for _, want := range []string{"control_connect", "hello_ok", "service_list", "shutdown", "exit"} {
		tc.logContains(t, want)
	}
}

// TestRunVersionMismatch 覆盖 HelloOK.Version 不匹配：Run 必须报错。
func TestRunVersionMismatch(t *testing.T) {
	r := newFakeRelay(t, fakeRelayOptions{helloVersion: protocol.ProtocolVersion + 1})
	cfg := testConfig(r.addr())
	tc := startTestClient(t, cfg)

	err := tc.requireErr(5 * time.Second)
	if !errors.Is(err, protocol.ErrUnsupportedVersion) {
		t.Fatalf("期望 ErrUnsupportedVersion，实际 %v", err)
	}
}

// TestRunAuthFailed 覆盖令牌被拒：relay 回 MsgError{auth_failed}，Run 必须报错。
func TestRunAuthFailed(t *testing.T) {
	r := newFakeRelay(t, fakeRelayOptions{
		helloErr: &protocol.Error{Code: protocol.ErrCodeAuthFailed, Message: "接入密钥错误"},
	})
	cfg := testConfig(r.addr())
	tc := startTestClient(t, cfg)

	err := tc.requireErr(5 * time.Second)
	if !strings.Contains(err.Error(), protocol.ErrCodeAuthFailed) {
		t.Fatalf("错误信息应包含 auth_failed，实际 %v", err)
	}
}

// TestServiceListSubscribeEvents 覆盖订阅：revision 单调递增，旧 revision 事件被丢弃。
func TestServiceListSubscribeEvents(t *testing.T) {
	r := newFakeRelay(t, fakeRelayOptions{
		services: []protocol.ServiceInfo{tcpService("svc-a")},
		revision: 7,
		udpMode:  protocol.UDPModeNative,
	})
	cfg := testConfig(r.addr())
	cfg.Client.Subscribe = true
	tc := startTestClient(t, cfg)

	r.waitReady(t)
	sub := r.expectSubscribe(t)
	if !sub.Enable {
		t.Fatalf("Subscribe.Enable 应为 true")
	}

	waitFor(t, 3*time.Second, "服务列表基线", func() bool {
		svc, rev := tc.c.servicesView()
		_, ok := svc["svc-a"]
		return ok && rev == 7
	})

	// revision 8：新增 svc-b。
	r.send(t, protocol.MsgServiceEvent, &protocol.ServiceEvent{
		Event:    protocol.EventAdded,
		Revision: 8,
		Service:  udpService("svc-b"),
	})
	waitFor(t, 3*time.Second, "svc-b 生效", func() bool {
		svc, rev := tc.c.servicesView()
		_, ok := svc["svc-b"]
		return ok && rev == 8
	})

	// revision 5：旧事件（删除 svc-a），必须被丢弃。
	r.send(t, protocol.MsgServiceEvent, &protocol.ServiceEvent{
		Event:    protocol.EventRemoved,
		Revision: 5,
		Service:  protocol.ServiceInfo{ServiceID: "svc-a"},
	})
	// revision 9：再发一个合法事件作为「顺序屏障」，确保旧事件已被处理。
	r.send(t, protocol.MsgServiceEvent, &protocol.ServiceEvent{
		Event:    protocol.EventUpdated,
		Revision: 9,
		Service:  protocol.ServiceInfo{ServiceID: "svc-a", Proto: "tcp", Online: false},
	})
	waitFor(t, 3*time.Second, "revision 9 生效", func() bool {
		_, rev := tc.c.servicesView()
		return rev == 9
	})

	svc, rev := tc.c.servicesView()
	if rev != 9 {
		t.Fatalf("revision = %d, 期望 9", rev)
	}
	if _, ok := svc["svc-a"]; !ok {
		t.Fatalf("旧 revision 事件不应删除 svc-a")
	}
	if svc["svc-a"].Online {
		t.Fatalf("svc-a 应被 revision 9 更新为离线")
	}
	if _, ok := svc["svc-b"]; !ok {
		t.Fatalf("svc-b 应仍然存在")
	}

	if err := tc.stop(); err != nil {
		t.Fatalf("Run 应返回 nil，实际 %v", err)
	}
}

// TestServiceMissingOpensTCPAndIsolatesError 覆盖服务缺失：
// 按 TCP 打开本地监听 + WARN；ConnectRequest 失败只关闭对应本地连接，Run 不退出。
func TestServiceMissingOpensTCPAndIsolatesError(t *testing.T) {
	r := newFakeRelay(t, fakeRelayOptions{revision: 1, udpMode: protocol.UDPModeNative})
	cfg := testConfig(r.addr())
	cfg.Client.Services = []config.ClientService{{ID: "ghost", Listen: "127.0.0.1:0"}}
	tc := startTestClient(t, cfg)

	addr := tc.listenerAddr("ghost")
	views := tc.c.listenerViews()
	if len(views) != 1 || views[0].Proto != "tcp" {
		t.Fatalf("服务缺失时应按 TCP 监听，实际 %+v", views)
	}
	tc.logContains(t, "service_missing")

	local, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("连接本地监听失败: %v", err)
	}
	defer local.Close()

	cr, ok := r.expect(t, protocol.MsgConnectRequest).(*protocol.ConnectRequest)
	if !ok {
		t.Fatalf("期望 ConnectRequest")
	}
	if cr.ServiceID != "ghost" {
		t.Fatalf("ConnectRequest.ServiceID = %q", cr.ServiceID)
	}

	// relay 回 service_not_found：只关闭对应本地连接。
	r.send(t, protocol.MsgError, &protocol.Error{
		Code:    protocol.ErrCodeServiceNotFound,
		Message: "服务不存在",
	})

	_ = local.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, readErr := local.Read(make([]byte, 1)); readErr == nil {
		t.Fatalf("relay 拒绝后本地连接应被关闭")
	}
	waitFor(t, 3*time.Second, "本地连接表清理", func() bool { return tc.c.sessionCount() == 0 })

	// Run 不受影响。
	if runErr, done := tc.waitResult(100 * time.Millisecond); done {
		t.Fatalf("单次接入失败不应让 Run 退出: %v", runErr)
	}
	if err := tc.stop(); err != nil {
		t.Fatalf("Run 应返回 nil，实际 %v", err)
	}
}

// TestPortConflictReturnsError 覆盖本地端口冲突：Run 必须返回错误，不能静默。
func TestPortConflictReturnsError(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占用端口失败: %v", err)
	}
	defer occupied.Close()

	r := newFakeRelay(t, fakeRelayOptions{
		services: []protocol.ServiceInfo{tcpService("busy")},
		revision: 1,
	})
	cfg := testConfig(r.addr())
	cfg.Client.Services = []config.ClientService{{ID: "busy", Listen: occupied.Addr().String()}}
	tc := startTestClient(t, cfg)

	runErr := tc.requireErr(8 * time.Second)
	if !strings.Contains(runErr.Error(), "监听") {
		t.Fatalf("期望监听失败错误，实际 %v", runErr)
	}
}

// TestKicked 覆盖 MsgKicked：Run 返回错误（不自动重连）。
func TestKicked(t *testing.T) {
	r := newFakeRelay(t, fakeRelayOptions{revision: 1})
	cfg := testConfig(r.addr())
	tc := startTestClient(t, cfg)

	r.waitReady(t)
	r.send(t, protocol.MsgKicked, &protocol.Kicked{Reason: "管理页踢出"})

	err := tc.requireErr(5 * time.Second)
	if !errors.Is(err, errKicked) {
		t.Fatalf("期望 errKicked，实际 %v", err)
	}
	tc.logContains(t, "kicked")
}

// TestServerShutdown 覆盖 MsgServerShutdown：Run 返回 nil。
func TestServerShutdown(t *testing.T) {
	r := newFakeRelay(t, fakeRelayOptions{revision: 1})
	cfg := testConfig(r.addr())
	tc := startTestClient(t, cfg)

	r.waitReady(t)
	r.send(t, protocol.MsgServerShutdown, &protocol.ServerShutdown{Reason: "维护"})

	tc.requireNil(5 * time.Second)
	tc.logContains(t, "server_shutdown")
}

// TestControlConnClosedReturnsError 覆盖控制连接意外断开 + reconnect=false（旧行为）：
// Run 返回错误。reconnect=true 的自愈语义见 reconnect_test.go。
func TestControlConnClosedReturnsError(t *testing.T) {
	r := newFakeRelay(t, fakeRelayOptions{revision: 1})
	cfg := testConfig(r.addr())
	// 直接构造 Config 时 Reconnect 零值为 false（= 阶段 6C 之前的旧行为）。
	cfg.Client.Reconnect = false
	tc := startTestClient(t, cfg)

	r.waitReady(t)
	r.mu.Lock()
	conn := r.ctrl
	r.mu.Unlock()
	if conn == nil {
		t.Fatalf("控制连接未建立")
	}
	_ = conn.Close()

	err := tc.requireErr(8 * time.Second)
	if !strings.Contains(err.Error(), "控制连接") {
		t.Fatalf("期望控制连接断开错误，实际 %v", err)
	}
}

// TestAdminPage 覆盖管理页：服务列表、本地端口映射、活跃配对均可展示。
func TestAdminPage(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("分配管理端口失败: %v", err)
	}
	manageAddr := probe.Addr().String()
	_ = probe.Close()

	r := newFakeRelay(t, fakeRelayOptions{
		services: []protocol.ServiceInfo{tcpService("svc-x")},
		revision: 3,
		udpMode:  protocol.UDPModeNative,
	})
	cfg := testConfig(r.addr())
	cfg.Common.ManageListen = manageAddr
	cfg.Client.Services = []config.ClientService{{ID: "svc-x", Listen: "127.0.0.1:0"}}
	tc := startTestClient(t, cfg)

	localAddr := tc.listenerAddr("svc-x")

	var body string
	waitFor(t, 5*time.Second, "管理页可访问", func() bool {
		resp, getErr := http.Get("http://" + manageAddr + "/client")
		if getErr != nil {
			return false
		}
		defer resp.Body.Close()
		raw, readErr := io.ReadAll(resp.Body)
		if readErr != nil || resp.StatusCode != http.StatusOK {
			return false
		}
		body = string(raw)
		return true
	})

	for _, want := range []string{"relay 返回的可用服务列表", "svc-x", "本地端口映射表", localAddr, "当前活跃配对"} {
		if !strings.Contains(body, want) {
			t.Fatalf("管理页缺少 %q；页面:\n%s", want, body)
		}
	}

	// 首页也能访问（内置页面列表）。
	resp, err := http.Get("http://" + manageAddr + "/")
	if err != nil {
		t.Fatalf("访问管理页首页失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("管理页首页状态码 = %d", resp.StatusCode)
	}

	if err := tc.stop(); err != nil {
		t.Fatalf("Run 应返回 nil，实际 %v", err)
	}
}

// TestTLSMisconfigurationFails 覆盖对齐后的 TLS 触发规则：
// 显式配置 TLS（tls_insecure_skip_verify=true）而 relay 是明文时，
// 握手必须失败并给出可排查的错误（不做协议探测/降级）。
func TestTLSMisconfigurationFails(t *testing.T) {
	r := newFakeRelay(t, fakeRelayOptions{revision: 1})
	cfg := testConfig(r.addr())
	cfg.Client.TLSInsecureSkipVerify = true
	tc := startTestClient(t, cfg)

	err := tc.requireErr(8 * time.Second)
	if !strings.Contains(err.Error(), "TLS") {
		t.Fatalf("期望 TLS 握手失败错误，实际 %v", err)
	}
	tc.logContains(t, "tls_insecure_skip_verify")
}

// TestDataAddrUnavailableDoesNotFailRun 覆盖数据面地址推导失败：
// Run 继续运行（本地监听照常打开），只在配对阶段放弃该次接入并关闭本地连接。
func TestDataAddrUnavailableDoesNotFailRun(t *testing.T) {
	r := newFakeRelay(t, fakeRelayOptions{
		services:    []protocol.ServiceInfo{tcpService("svc-noport")},
		revision:    1,
		noDataPorts: true,
	})
	cfg := testConfig(r.addr())
	cfg.Client.Services = []config.ClientService{{ID: "svc-noport", Listen: "127.0.0.1:0"}}
	tc := startTestClient(t, cfg)

	addr := tc.listenerAddr("svc-noport")
	tc.logContains(t, "tcp_data_addr_unavailable")
	if runErr, done := tc.waitResult(100 * time.Millisecond); done {
		t.Fatalf("数据面地址不可用不应让 Run 退出: %v", runErr)
	}

	local, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("连接本地监听失败: %v", err)
	}
	defer local.Close()

	cr := r.expect(t, protocol.MsgConnectRequest).(*protocol.ConnectRequest)
	r.send(t, protocol.MsgPairResult, &protocol.PairResult{
		PairingKey:   "svc-noport/" + cr.ClientConnID,
		PairingToken: "tok",
		Proto:        "tcp",
		Role:         protocol.RoleClient,
	})

	// 无法推导数据面地址 → 放弃本次配对、关闭本地连接。
	_ = local.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, readErr := local.Read(make([]byte, 1)); readErr == nil {
		t.Fatalf("数据面地址不可用时本地连接应被关闭")
	}
	waitFor(t, 3*time.Second, "本地连接表清理", func() bool { return tc.c.sessionCount() == 0 })
	if runErr, done := tc.waitResult(100 * time.Millisecond); done {
		t.Fatalf("配对失败不应让 Run 退出: %v", runErr)
	}
	if err := tc.stop(); err != nil {
		t.Fatalf("Run 应返回 nil，实际 %v", err)
	}
}

// TestGoroutineCleanup 覆盖关停后 goroutine 不增长（±2）。
func TestGoroutineCleanup(t *testing.T) {
	r := newFakeRelay(t, fakeRelayOptions{
		services: []protocol.ServiceInfo{tcpService("svc-g")},
		revision: 1,
	})
	cfg := testConfig(r.addr())
	cfg.Client.Services = []config.ClientService{{ID: "svc-g", Listen: "127.0.0.1:0"}}

	base := runtime.NumGoroutine()

	// 先建立一次完整配对与转发，再关停，确保转发 goroutine 也被回收。
	tc := startTestClient(t, cfg)
	addr := tc.listenerAddr("svc-g")
	local, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("连接本地监听失败: %v", err)
	}
	cr := r.expect(t, protocol.MsgConnectRequest).(*protocol.ConnectRequest)
	key := "svc-g/" + cr.ClientConnID
	r.send(t, protocol.MsgPairResult, &protocol.PairResult{
		PairingKey:   key,
		PairingToken: "tok",
		Proto:        "tcp",
		Role:         protocol.RoleClient,
	})
	dc := r.nextDataConn(t, 5*time.Second)
	if _, err := local.Write([]byte("x")); err != nil {
		t.Fatalf("写本地连接失败: %v", err)
	}
	buf := make([]byte, 1)
	_ = dc.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(dc.conn, buf); err != nil {
		t.Fatalf("relay 未收到转发数据: %v", err)
	}

	if err := tc.stop(); err != nil {
		t.Fatalf("Run 应返回 nil，实际 %v", err)
	}
	_ = local.Close()
	_ = dc.conn.Close()

	deadline := time.Now().Add(5 * time.Second)
	for {
		now := runtime.NumGoroutine()
		if now <= base+2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("关停后 goroutine 未回落: base=%d now=%d", base, now)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
