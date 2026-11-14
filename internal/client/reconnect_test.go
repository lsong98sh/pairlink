package client

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// freeTCPAddr 返回一个当前无人监听的 TCP 地址。
func freeTCPAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("分配端口: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("关闭临时监听: %v", err)
	}
	return addr
}

// TestReconnectAfterControlDrop 覆盖 A1 核心（client 侧）：控制连接断开 → 自动重连 →
// 重新 Hello/ListServices → 本地监听不重建 → 服务恢复可用。
func TestReconnectAfterControlDrop(t *testing.T) {
	r := newFakeRelay(t, fakeRelayOptions{
		services: []protocol.ServiceInfo{tcpService("svc-rc")},
		revision: 7,
	})
	cfg := testConfig(r.addr())
	cfg.Client.Reconnect = true
	cfg.Client.Services = []config.ClientService{{ID: "svc-rc", Listen: "127.0.0.1:0"}}
	tc := startTestClient(t, cfg)

	r.waitReady(t)
	localAddr := tc.listenerAddr("svc-rc")

	// 断开控制连接。
	r.dropControl(t)
	r.waitHelloCount(t, 2)
	waitFor(t, 5*time.Second, "重连完成", func() bool {
		return tc.c.controlConn() != nil && tc.c.negotiated().clientID != ""
	})
	tc.logContains(t, "reconnect_scheduled")
	// reconnect_ok 在重新 Hello/ListServices/订阅全部完成后才写日志，可能晚于
	// controlConn 恢复（race 下尤其明显），这里轮询等待，避免读日志早于写日志。
	waitFor(t, 5*time.Second, "重连成功日志 reconnect_ok", func() bool {
		return strings.Contains(tc.logs.String(), "reconnect_ok")
	})

	// 本地监听不重建：地址不变、协议不变。
	if got := tc.listenerAddr("svc-rc"); got != localAddr {
		t.Fatalf("重连后本地监听地址变化：%s → %s", localAddr, got)
	}
	if got := tc.c.listenerProto("svc-rc"); got != "tcp" {
		t.Fatalf("重连后本地监听协议 = %q", got)
	}

	// 服务恢复可用：本地接入 → ConnectRequest → PairResult → 数据面 echo。
	conn, err := net.DialTimeout("tcp", localAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("拨号本地监听: %v", err)
	}
	defer conn.Close()
	cr := r.expect(t, protocol.MsgConnectRequest).(*protocol.ConnectRequest)
	key := "svc-rc/" + cr.ClientConnID
	r.send(t, protocol.MsgPairResult, &protocol.PairResult{
		PairingKey:   key,
		PairingToken: "token-rc",
		Proto:        "tcp",
		Role:         protocol.RoleClient,
	})
	dc := r.nextDataConn(t, 5*time.Second)
	if dc.bind.PairingKey != key {
		t.Fatalf("数据面配对键 = %q", dc.bind.PairingKey)
	}
	payload := []byte("after-reconnect")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("写本地连接: %v", err)
	}
	if got := readExact(t, dc.conn, len(payload)); string(got) != string(payload) {
		t.Fatalf("relay 收到 %q，期望 %q", got, payload)
	}
	if _, err := dc.conn.Write(payload); err != nil {
		t.Fatalf("写数据面连接: %v", err)
	}
	if got := readExact(t, conn, len(payload)); string(got) != string(payload) {
		t.Fatalf("本地连接收到 %q，期望 %q", got, payload)
	}

	if err := tc.stop(); err != nil {
		t.Fatalf("Run 应返回 nil，实际 %v", err)
	}
}

// TestReconnectForcesNewRevisionBaseline 覆盖 relay 重启导致 revision 归零：
// 重连后必须**强制接受**新基线，否则本地视图会永久卡死。
func TestReconnectForcesNewRevisionBaseline(t *testing.T) {
	r := newFakeRelay(t, fakeRelayOptions{
		udpMode: protocol.UDPModeNative,
		listFn: func(connIndex int) ([]protocol.ServiceInfo, uint64) {
			if connIndex == 1 {
				return []protocol.ServiceInfo{tcpService("old-svc")}, 42
			}
			// relay 重启后 revision 归零，且服务列表已变化。
			return []protocol.ServiceInfo{tcpService("new-svc")}, 0
		},
	})
	cfg := testConfig(r.addr())
	cfg.Client.Reconnect = true
	tc := startTestClient(t, cfg)

	r.waitReady(t)
	waitFor(t, 3*time.Second, "首次服务列表", func() bool {
		services, rev := tc.c.servicesView()
		_, ok := services["old-svc"]
		return ok && rev == 42
	})

	r.dropControl(t)
	r.waitHelloCount(t, 2)
	waitFor(t, 5*time.Second, "重连后强制接受新基线", func() bool {
		services, rev := tc.c.servicesView()
		_, oldOK := services["old-svc"]
		_, newOK := services["new-svc"]
		return newOK && !oldOK && rev == 0
	})
	tc.logContains(t, "reconnect_ok")

	if err := tc.stop(); err != nil {
		t.Fatalf("Run 应返回 nil，实际 %v", err)
	}
}

// TestReconnectDisabledReturnsError 覆盖 reconnect=false 的旧行为。
func TestReconnectDisabledReturnsError(t *testing.T) {
	r := newFakeRelay(t, fakeRelayOptions{revision: 1})
	cfg := testConfig(r.addr())
	cfg.Client.Reconnect = false
	tc := startTestClient(t, cfg)

	r.waitReady(t)
	r.dropControl(t)

	err := tc.requireErr(8 * time.Second)
	if err == nil || !strings.Contains(err.Error(), "控制连接") {
		t.Fatalf("期望控制连接断开错误，实际 %v", err)
	}
	if r.helloCount() != 1 {
		t.Fatalf("reconnect=false 时不应重连：Hello 次数 = %d", r.helloCount())
	}
}

// TestReconnectStopsOnKicked 覆盖踢出不重连。
func TestReconnectStopsOnKicked(t *testing.T) {
	r := newFakeRelay(t, fakeRelayOptions{revision: 1})
	cfg := testConfig(r.addr())
	cfg.Client.Reconnect = true
	tc := startTestClient(t, cfg)

	r.waitReady(t)
	r.send(t, protocol.MsgKicked, &protocol.Kicked{Reason: "管理页踢出"})

	err := tc.requireErr(5 * time.Second)
	if !errors.Is(err, errKicked) {
		t.Fatalf("期望 errKicked，实际 %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if got := r.helloCount(); got != 1 {
		t.Fatalf("踢出后不应重连：Hello 次数 = %d", got)
	}
	tc.logContains(t, "kicked")
}

// TestReconnectSurvivesUnreachableRelay 覆盖 relay 不可达时进程存活、ctx 可打断退避。
func TestReconnectSurvivesUnreachableRelay(t *testing.T) {
	cfg := testConfig(freeTCPAddr(t))
	cfg.Client.Reconnect = true
	tc := startTestClient(t, cfg)

	waitFor(t, 5*time.Second, "记录 reconnect_failed", func() bool {
		return strings.Contains(tc.logs.String(), "reconnect_failed")
	})
	waitFor(t, 5*time.Second, "记录 reconnect_scheduled", func() bool {
		return strings.Contains(tc.logs.String(), "reconnect_scheduled")
	})
	if _, done := tc.waitResult(100 * time.Millisecond); done {
		t.Fatalf("relay 不可达且 reconnect=true 时 Run 不应退出（err=%v）", tc.err)
	}
	if err := tc.stop(); err != nil {
		t.Fatalf("ctx 取消后 Run 应返回 nil，实际 %v", err)
	}
}

// TestHotSwitchTCPToUDP 覆盖 A2：订阅到 proto 变化 → 原地址热切换 TCP→UDP。
func TestHotSwitchTCPToUDP(t *testing.T) {
	r := newFakeRelay(t, fakeRelayOptions{
		services: []protocol.ServiceInfo{tcpService("svc-hot")},
		revision: 1,
	})
	cfg := testConfig(r.addr())
	cfg.Client.Services = []config.ClientService{{ID: "svc-hot", Listen: "127.0.0.1:0"}}
	tc := startTestClient(t, cfg)

	r.waitReady(t)
	addr := tc.listenerAddr("svc-hot")
	if got := tc.c.listenerProto("svc-hot"); got != "tcp" {
		t.Fatalf("初始监听协议 = %q，期望 tcp", got)
	}
	before := tc.c.listenerByService("svc-hot")

	// relay 推送 proto=udp 的 updated 事件。
	r.send(t, protocol.MsgServiceEvent, &protocol.ServiceEvent{
		Event:    protocol.EventUpdated,
		Revision: 2,
		Service:  protocol.ServiceInfo{ServiceID: "svc-hot", Proto: "udp", Online: true},
	})
	waitFor(t, 5*time.Second, "热切换为 UDP", func() bool {
		return tc.c.listenerProto("svc-hot") == "udp"
	})
	tc.logContains(t, "listen_hot_switch")

	// 地址不变（第三方不需要改端口）。
	if got := tc.listenerAddr("svc-hot"); got != addr {
		t.Fatalf("热切换后地址变化：%s → %s", addr, got)
	}
	if tc.c.listenerByService("svc-hot") == before {
		t.Fatal("热切换后监听器对象应已替换")
	}

	// 新监听确实按 UDP 工作：本地数据报触发 ConnectRequest。
	third, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatalf("拨号本地 UDP 监听: %v", err)
	}
	defer third.Close()
	if _, err := third.Write([]byte("udp-after-switch")); err != nil {
		t.Fatalf("发送本地数据报: %v", err)
	}
	cr := r.expect(t, protocol.MsgConnectRequest).(*protocol.ConnectRequest)
	if cr.ServiceID != "svc-hot" {
		t.Fatalf("ConnectRequest.ServiceID = %q", cr.ServiceID)
	}

	if err := tc.stop(); err != nil {
		t.Fatalf("Run 应返回 nil，实际 %v", err)
	}
}

// TestHotSwitchUDPToTCP 覆盖 A2：原地址热切换 UDP→TCP。
func TestHotSwitchUDPToTCP(t *testing.T) {
	r := newFakeRelay(t, fakeRelayOptions{
		services: []protocol.ServiceInfo{udpService("svc-hot2")},
		revision: 1,
		udpMode:  protocol.UDPModeNative,
		withUDP:  true,
	})
	cfg := testConfig(r.addr())
	cfg.Client.Services = []config.ClientService{{ID: "svc-hot2", Listen: "127.0.0.1:0"}}
	tc := startTestClient(t, cfg)

	r.waitReady(t)
	addr := tc.listenerAddr("svc-hot2")
	if got := tc.c.listenerProto("svc-hot2"); got != "udp" {
		t.Fatalf("初始监听协议 = %q，期望 udp", got)
	}

	r.send(t, protocol.MsgServiceEvent, &protocol.ServiceEvent{
		Event:    protocol.EventUpdated,
		Revision: 2,
		Service:  protocol.ServiceInfo{ServiceID: "svc-hot2", Proto: "tcp", Online: true},
	})
	waitFor(t, 5*time.Second, "热切换为 TCP", func() bool {
		return tc.c.listenerProto("svc-hot2") == "tcp"
	})
	if got := tc.listenerAddr("svc-hot2"); got != addr {
		t.Fatalf("热切换后地址变化：%s → %s", addr, got)
	}

	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("拨号本地 TCP 监听: %v", err)
	}
	defer conn.Close()
	cr := r.expect(t, protocol.MsgConnectRequest).(*protocol.ConnectRequest)
	if cr.ServiceID != "svc-hot2" {
		t.Fatalf("ConnectRequest.ServiceID = %q", cr.ServiceID)
	}

	if err := tc.stop(); err != nil {
		t.Fatalf("Run 应返回 nil，实际 %v", err)
	}
}

// TestHotSwitchRollbackOnFailure 覆盖 A2 回滚：新协议监听打不开（端口被占等）时
// 保留/恢复旧监听，本地服务不消失。
func TestHotSwitchRollbackOnFailure(t *testing.T) {
	r := newFakeRelay(t, fakeRelayOptions{
		services: []protocol.ServiceInfo{tcpService("svc-rb")},
		revision: 1,
	})
	cfg := testConfig(r.addr())
	cfg.Client.Services = []config.ClientService{{ID: "svc-rb", Listen: "127.0.0.1:0"}}
	tc := startTestClientWith(t, cfg, func(c *client) {
		c.listenFn = func(proto, addr string) (*localListener, error) {
			if proto == "udp" {
				return nil, errors.New("模拟 UDP 端口被占用")
			}
			return openLocal(proto, addr)
		}
	})

	r.waitReady(t)
	addr := tc.listenerAddr("svc-rb")

	r.send(t, protocol.MsgServiceEvent, &protocol.ServiceEvent{
		Event:    protocol.EventUpdated,
		Revision: 2,
		Service:  protocol.ServiceInfo{ServiceID: "svc-rb", Proto: "udp", Online: true},
	})
	waitFor(t, 5*time.Second, "回滚日志", func() bool {
		return strings.Contains(tc.logs.String(), "rollback=true")
	})
	// 本地服务没有消失：仍是 TCP，且可接入。
	if got := tc.c.listenerProto("svc-rb"); got != "tcp" {
		t.Fatalf("回滚后监听协议 = %q，期望 tcp", got)
	}
	if got := tc.listenerAddr("svc-rb"); got != addr {
		t.Fatalf("回滚后地址变化：%s → %s", addr, got)
	}
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("回滚后本地 TCP 应仍可接入: %v", err)
	}
	defer conn.Close()
	cr := r.expect(t, protocol.MsgConnectRequest).(*protocol.ConnectRequest)
	if cr.ServiceID != "svc-rb" {
		t.Fatalf("ConnectRequest.ServiceID = %q", cr.ServiceID)
	}

	if err := tc.stop(); err != nil {
		t.Fatalf("Run 应返回 nil，实际 %v", err)
	}
}

// TestHotSwitchMissingServiceThenAppears 覆盖与 service_missing 占位的交互：
// 启动时列表里没有该服务 → 按 TCP 占位；服务出现且 proto=udp → 热切换。
func TestHotSwitchMissingServiceThenAppears(t *testing.T) {
	r := newFakeRelay(t, fakeRelayOptions{revision: 1})
	cfg := testConfig(r.addr())
	cfg.Client.Services = []config.ClientService{{ID: "late", Listen: "127.0.0.1:0"}}
	tc := startTestClient(t, cfg)

	r.waitReady(t)
	addr := tc.listenerAddr("late")
	tc.logContains(t, "service_missing")
	if got := tc.c.listenerProto("late"); got != "tcp" {
		t.Fatalf("占位监听协议 = %q，期望 tcp", got)
	}

	r.send(t, protocol.MsgServiceEvent, &protocol.ServiceEvent{
		Event:    protocol.EventAdded,
		Revision: 2,
		Service:  protocol.ServiceInfo{ServiceID: "late", Proto: "udp", Online: true},
	})
	waitFor(t, 5*time.Second, "服务出现后热切换为 UDP", func() bool {
		return tc.c.listenerProto("late") == "udp"
	})
	if got := tc.listenerAddr("late"); got != addr {
		t.Fatalf("热切换后地址变化：%s → %s", addr, got)
	}
	if err := tc.stop(); err != nil {
		t.Fatalf("Run 应返回 nil，实际 %v", err)
	}
}

// TestHotSwitchRepeatedNoLeak 覆盖 proto 反复变化不泄漏 listener/goroutine。
func TestHotSwitchRepeatedNoLeak(t *testing.T) {
	r := newFakeRelay(t, fakeRelayOptions{
		services: []protocol.ServiceInfo{tcpService("svc-flap")},
		revision: 1,
	})
	cfg := testConfig(r.addr())
	cfg.Client.Services = []config.ClientService{{ID: "svc-flap", Listen: "127.0.0.1:0"}}
	tc := startTestClient(t, cfg)
	r.waitReady(t)
	addr := tc.listenerAddr("svc-flap")

	rev := uint64(1)
	for i := 0; i < 5; i++ {
		for _, proto := range []string{"udp", "tcp"} {
			rev++
			r.send(t, protocol.MsgServiceEvent, &protocol.ServiceEvent{
				Event:    protocol.EventUpdated,
				Revision: rev,
				Service:  protocol.ServiceInfo{ServiceID: "svc-flap", Proto: proto, Online: true},
			})
			want := proto
			waitFor(t, 5*time.Second, "切换到 "+want, func() bool {
				return tc.c.listenerProto("svc-flap") == want
			})
		}
	}

	// 只有 1 个映射、地址不变（没有泄漏旧 listener）。
	views := tc.c.listenerViews()
	if len(views) != 1 {
		t.Fatalf("监听映射数量 = %d，期望 1：%+v", len(views), views)
	}
	if views[0].Addr != addr {
		t.Fatalf("监听地址 = %q，期望 %q", views[0].Addr, addr)
	}
	if got := tc.c.listenerProto("svc-flap"); got != "tcp" {
		t.Fatalf("最终监听协议 = %q，期望 tcp", got)
	}
	if err := tc.stop(); err != nil {
		t.Fatalf("Run 应返回 nil，实际 %v", err)
	}
}

// TestNativeUDPBindOKLostRetry 覆盖 A4：BindOK 丢失后每 ~300ms 重发报到帧。
func TestNativeUDPBindOKLostRetry(t *testing.T) {
	r := newFakeRelay(t, fakeRelayOptions{
		services: []protocol.ServiceInfo{udpService("svc-udp-retry")},
		revision: 1,
		udpMode:  protocol.UDPModeNative,
		withUDP:  true,
	})
	cfg := testConfig(r.addr())
	cfg.Client.PairTimeout = 2 * time.Second
	cfg.Client.Services = []config.ClientService{{ID: "svc-udp-retry", Listen: "127.0.0.1:0"}}
	tc := startTestClient(t, cfg)

	listenAddr := tc.listenerAddr("svc-udp-retry")
	third, err := net.Dial("udp", listenAddr)
	if err != nil {
		t.Fatalf("拨号本地 UDP: %v", err)
	}
	defer third.Close()
	if _, err := third.Write([]byte("lost-bindok")); err != nil {
		t.Fatalf("发送首包: %v", err)
	}
	cr := r.expect(t, protocol.MsgConnectRequest).(*protocol.ConnectRequest)
	key := "svc-udp-retry/" + cr.ClientConnID
	r.send(t, protocol.MsgPairResult, &protocol.PairResult{
		PairingKey: key, PairingToken: "tok-retry", Proto: "udp", Role: protocol.RoleClient,
	})

	// 第一次报到：丢弃不回。
	first := r.nextDatagram(t, 3*time.Second)
	start := time.Now()
	// 客户端应在约 300ms 后重发同一报到帧。
	second := r.nextDatagram(t, 3*time.Second)
	if string(first.data) != string(second.data) {
		t.Fatalf("重发的报到帧与首次不同")
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("重发间隔过长：%s", elapsed)
	}

	// 回应 BindOK → 首包补发 → 数据面可用。
	okFrame, err := protocol.EncodeFrame(protocol.MsgBindOK, &protocol.BindOK{PairingKey: key})
	if err != nil {
		t.Fatalf("编码 BindOK: %v", err)
	}
	r.udpSend(t, second.from, okFrame)
	flushed := r.nextDatagram(t, 3*time.Second)
	if string(flushed.data) != "lost-bindok" {
		t.Fatalf("首包补发 = %q，期望 lost-bindok", flushed.data)
	}
	r.udpSend(t, flushed.from, []byte("reply"))
	conn := third.(*net.UDPConn)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 128)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("第三方读取回包: %v", err)
	}
	if string(buf[:n]) != "reply" {
		t.Fatalf("第三方收到 %q，期望 reply", buf[:n])
	}

	if err := tc.stop(); err != nil {
		t.Fatalf("Run 应返回 nil，实际 %v", err)
	}
}

// readExact 带 deadline 读取恰好 n 字节。
func readExact(t *testing.T, conn net.Conn, n int) []byte {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("设置读 deadline: %v", err)
	}
	buf := make([]byte, n)
	got := 0
	for got < n {
		m, err := conn.Read(buf[got:])
		if err != nil {
			t.Fatalf("读取 %d 字节（已读 %d）: %v", n, got, err)
		}
		got += m
	}
	return buf
}
