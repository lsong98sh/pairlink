package client

import (
	"io"
	"net"
	"regexp"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// connIDPattern 校验 client_conn_id 是 8 字节随机 hex。
var connIDPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// TestTCPEndToEnd 覆盖本地 TCP 接入 → ConnectRequest → PairResult → 数据面 Bind →
// 双向转发 → 断开清理的完整路径。
func TestTCPEndToEnd(t *testing.T) {
	r := newFakeRelay(t, fakeRelayOptions{
		services: []protocol.ServiceInfo{tcpService("svc-tcp")},
		revision: 1,
		udpMode:  protocol.UDPModeNative,
	})
	cfg := testConfig(r.addr())
	cfg.Client.Services = []config.ClientService{{ID: "svc-tcp", Listen: "127.0.0.1:0"}}
	tc := startTestClient(t, cfg)

	localAddr := tc.listenerAddr("svc-tcp")
	local, err := net.DialTimeout("tcp", localAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("连接本地监听失败: %v", err)
	}
	defer local.Close()

	cr, ok := r.expect(t, protocol.MsgConnectRequest).(*protocol.ConnectRequest)
	if !ok {
		t.Fatalf("期望 ConnectRequest")
	}
	if cr.ServiceID != "svc-tcp" {
		t.Fatalf("ConnectRequest.ServiceID = %q, 期望 svc-tcp", cr.ServiceID)
	}
	if !connIDPattern.MatchString(cr.ClientConnID) {
		t.Fatalf("client_conn_id = %q，期望 16 位小写 hex", cr.ClientConnID)
	}
	key := "svc-tcp/" + cr.ClientConnID

	// 本地 accepted 后应登记活跃连接（管理页可展示）。
	waitFor(t, 3*time.Second, "本地连接登记", func() bool { return tc.c.sessionCount() == 1 })

	r.send(t, protocol.MsgPairResult, &protocol.PairResult{
		PairingKey:   key,
		PairingToken: "token-abc",
		Proto:        "tcp",
		Role:         protocol.RoleClient,
	})

	dc := r.nextDataConn(t, 5*time.Second)
	if dc.bind.PairingKey != key {
		t.Fatalf("Bind.PairingKey = %q, 期望 %q", dc.bind.PairingKey, key)
	}
	if dc.bind.PairingToken != "token-abc" {
		t.Fatalf("Bind.PairingToken = %q", dc.bind.PairingToken)
	}
	if dc.bind.Role != protocol.RoleClient {
		t.Fatalf("Bind.Role = %q, 期望 client", dc.bind.Role)
	}
	if dc.bind.Carrier != "" && dc.bind.Carrier != protocol.CarrierTCP {
		t.Fatalf("Bind.Carrier = %q，期望空或 tcp", dc.bind.Carrier)
	}

	// 本地 → relay
	if _, err := local.Write([]byte("ping")); err != nil {
		t.Fatalf("本地写失败: %v", err)
	}
	got := make([]byte, 4)
	_ = dc.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(dc.conn, got); err != nil {
		t.Fatalf("relay 未收到转发数据: %v", err)
	}
	if string(got) != "ping" {
		t.Fatalf("relay 收到 %q, 期望 ping", got)
	}

	// relay → 本地
	if _, err := dc.conn.Write([]byte("pong")); err != nil {
		t.Fatalf("relay 写失败: %v", err)
	}
	_ = local.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(local, got); err != nil {
		t.Fatalf("本地未收到 relay 回包: %v", err)
	}
	if string(got) != "pong" {
		t.Fatalf("本地收到 %q, 期望 pong", got)
	}

	// 管理页可见的活跃配对：转发中。
	waitFor(t, 3*time.Second, "配对进入转发中", func() bool {
		for _, p := range tc.c.pairViews() {
			if p.Key == key && p.Status == "转发中" {
				return true
			}
		}
		return false
	})

	// 关闭第三方连接与 relay 数据连接 → 双向转发退出 → 表项清理。
	// （forward.Bidirectional 采用半关闭语义：一侧 EOF 后要等另一侧也结束。）
	_ = local.Close()
	_ = dc.conn.Close()
	waitFor(t, 5*time.Second, "配对表清理", func() bool { return tc.c.sessionCount() == 0 })
	tc.logContains(t, "event=bind")
	tc.logContains(t, "data_open")
	tc.logContains(t, "data_close")

	if err := tc.stop(); err != nil {
		t.Fatalf("Run 应返回 nil，实际 %v", err)
	}
}

// TestPairTimeoutClosesLocalConn 覆盖等待 PairResult 超时：关闭本地连接并清理表项，
// 但不影响 Run。
func TestPairTimeoutClosesLocalConn(t *testing.T) {
	r := newFakeRelay(t, fakeRelayOptions{
		services: []protocol.ServiceInfo{tcpService("svc-slow")},
		revision: 1,
	})
	cfg := testConfig(r.addr())
	cfg.Client.PairTimeout = 100 * time.Millisecond
	cfg.Client.Services = []config.ClientService{{ID: "svc-slow", Listen: "127.0.0.1:0"}}
	tc := startTestClient(t, cfg)

	localAddr := tc.listenerAddr("svc-slow")
	local, err := net.DialTimeout("tcp", localAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("连接本地监听失败: %v", err)
	}
	defer local.Close()

	// 消费 ConnectRequest 但不回 PairResult。
	r.expect(t, protocol.MsgConnectRequest)

	_ = local.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, readErr := local.Read(make([]byte, 1)); readErr == nil {
		t.Fatalf("配对超时后本地连接应被关闭")
	}
	waitFor(t, 3*time.Second, "超时后表项清理", func() bool { return tc.c.sessionCount() == 0 })
	tc.logContains(t, "pair_timeout")

	if runErr, done := tc.waitResult(100 * time.Millisecond); done {
		t.Fatalf("配对超时不应让 Run 退出: %v", runErr)
	}
	if err := tc.stop(); err != nil {
		t.Fatalf("Run 应返回 nil，实际 %v", err)
	}
}

// TestPairResultAfterTimeoutIgnored 覆盖「超时后 PairResult 才到」：本地连接已不存在，
// client 只记日志，不崩溃、不新建连接。
func TestPairResultAfterTimeoutIgnored(t *testing.T) {
	r := newFakeRelay(t, fakeRelayOptions{
		services: []protocol.ServiceInfo{tcpService("svc-late")},
		revision: 1,
	})
	cfg := testConfig(r.addr())
	cfg.Client.PairTimeout = 100 * time.Millisecond
	cfg.Client.Services = []config.ClientService{{ID: "svc-late", Listen: "127.0.0.1:0"}}
	tc := startTestClient(t, cfg)

	localAddr := tc.listenerAddr("svc-late")
	local, err := net.DialTimeout("tcp", localAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("连接本地监听失败: %v", err)
	}
	defer local.Close()

	cr := r.expect(t, protocol.MsgConnectRequest).(*protocol.ConnectRequest)
	waitFor(t, 3*time.Second, "超时后表项清理", func() bool { return tc.c.sessionCount() == 0 })

	// 迟到的配对结果：不应导致 panic / Run 退出。
	r.send(t, protocol.MsgPairResult, &protocol.PairResult{
		PairingKey:   "svc-late/" + cr.ClientConnID,
		PairingToken: "late",
		Proto:        "tcp",
		Role:         protocol.RoleClient,
	})
	time.Sleep(100 * time.Millisecond)
	if runErr, done := tc.waitResult(50 * time.Millisecond); done {
		t.Fatalf("迟到配对结果不应让 Run 退出: %v", runErr)
	}
	if err := tc.stop(); err != nil {
		t.Fatalf("Run 应返回 nil，实际 %v", err)
	}
}
