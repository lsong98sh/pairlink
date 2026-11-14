package client

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// dialLocal 连接本地监听并返回连接。
func dialLocal(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("连接本地监听 %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// expectLocalClosed 断言本地连接已被关闭（读得到非超时错误）。
func expectLocalClosed(t *testing.T, conn net.Conn, what string) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := conn.Read(make([]byte, 1))
	if err == nil {
		t.Fatalf("%s：本地连接本应被关闭，却读到 %d 字节", what, n)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatalf("%s：本地连接未关闭（读超时）", what)
	}
}

// expectLocalOpen 断言本地连接在给定时间内仍保持打开（读超时）。
func expectLocalOpen(t *testing.T, conn net.Conn, wait time.Duration, what string) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(wait))
	_, err := conn.Read(make([]byte, 1))
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("%s：本地连接本应保持打开，实际错误 %v", what, err)
	}
	_ = conn.SetReadDeadline(time.Time{})
}

// TestErrorMatchingByClientConnID 覆盖 MsgError 的**精确关联**与 FIFO 回退。
//
// 两条本地连接同时在等 PairResult：
//   - relay 的错误帧带 client_conn_id → 只关闭该字段指向的连接（另一条不受影响）；
//   - relay 的错误帧不带 client_conn_id（旧 relay）→ 回退 FIFO，关闭最早等待的那条。
func TestErrorMatchingByClientConnID(t *testing.T) {
	r := newFakeRelay(t, fakeRelayOptions{
		services: []protocol.ServiceInfo{tcpService("svc-a"), tcpService("svc-b")},
		revision: 1,
		udpMode:  protocol.UDPModeNative,
	})
	cfg := testConfig(r.addr())
	cfg.Client.Services = []config.ClientService{
		{ID: "svc-a", Listen: "127.0.0.1:0"},
		{ID: "svc-b", Listen: "127.0.0.1:0"},
	}
	tc := startTestClient(t, cfg)

	addrA := tc.listenerAddr("svc-a")
	addrB := tc.listenerAddr("svc-b")
	localA := dialLocal(t, addrA)
	localB := dialLocal(t, addrB)

	// 两个 ConnectRequest 都到达后再发错误，保证两条连接都处于"等待配对"状态。
	connIDs := map[string]string{}
	for i := 0; i < 2; i++ {
		cr, ok := r.expect(t, protocol.MsgConnectRequest).(*protocol.ConnectRequest)
		if !ok {
			t.Fatalf("期望 ConnectRequest")
		}
		connIDs[cr.ServiceID] = cr.ClientConnID
	}
	if connIDs["svc-a"] == "" || connIDs["svc-b"] == "" {
		t.Fatalf("ConnectRequest 未带 client_conn_id: %+v", connIDs)
	}

	// 1) 精确关联：错误只指向 svc-b。
	r.send(t, protocol.MsgError, &protocol.Error{
		Code:         protocol.ErrCodeServiceNotFound,
		Message:      "服务未注册",
		ClientConnID: connIDs["svc-b"],
	})
	expectLocalClosed(t, localB, "带 client_conn_id 的错误应关闭 svc-b 的连接")
	expectLocalOpen(t, localA, 250*time.Millisecond, "未被关联的 svc-a 连接不应被关闭")
	if got := tc.c.sessionCount(); got != 1 {
		t.Fatalf("本地连接数 = %d, 期望 1（只清理被关联的那条）", got)
	}

	// 2) 旧 relay（无 client_conn_id）：回退 FIFO，关闭剩下的 svc-a。
	r.send(t, protocol.MsgError, &protocol.Error{
		Code:    protocol.ErrCodeServiceNotFound,
		Message: "服务未注册",
	})
	expectLocalClosed(t, localA, "无 client_conn_id 的错误应按 FIFO 关闭最早等待的连接")
	waitFor(t, 3*time.Second, "本地连接表清理", func() bool { return tc.c.sessionCount() == 0 })

	// 单次接入失败不影响 Run。
	if runErr, done := tc.waitResult(100 * time.Millisecond); done {
		t.Fatalf("接入失败不应让 Run 退出: %v", runErr)
	}
	if err := tc.stop(); err != nil {
		t.Fatalf("Run 应返回 nil，实际 %v", err)
	}
}

// TestServiceListFullResync 覆盖 relay 主动下发的全量 MsgServiceList：
// 订阅事件断档时 relay 会改发完整列表，client 必须整体替换本地视图。
func TestServiceListFullResync(t *testing.T) {
	r := newFakeRelay(t, fakeRelayOptions{
		services: []protocol.ServiceInfo{tcpService("svc-a")},
		revision: 5,
		udpMode:  protocol.UDPModeNative,
	})
	cfg := testConfig(r.addr())
	tc := startTestClient(t, cfg)

	r.waitReady(t)
	waitFor(t, 3*time.Second, "服务列表基线", func() bool {
		svc, rev := tc.c.servicesView()
		_, ok := svc["svc-a"]
		return ok && rev == 5
	})

	// 全量重同步：svc-a 消失、svc-b 出现，revision 前进到 9。
	r.send(t, protocol.MsgServiceList, &protocol.ServiceList{
		Services: []protocol.ServiceInfo{tcpService("svc-b")},
		Revision: 9,
		UDPMode:  protocol.UDPModeNative,
	})
	waitFor(t, 3*time.Second, "全量重同步生效", func() bool {
		svc, rev := tc.c.servicesView()
		_, hasB := svc["svc-b"]
		_, hasA := svc["svc-a"]
		return hasB && !hasA && rev == 9
	})
	tc.logContains(t, "全量重同步")

	// 更旧的 revision 的全量列表不得把视图推回去（整份忽略）。
	r.send(t, protocol.MsgServiceList, &protocol.ServiceList{
		Services: []protocol.ServiceInfo{tcpService("svc-old")},
		Revision: 3,
		UDPMode:  protocol.UDPModeNative,
	})
	time.Sleep(100 * time.Millisecond)
	svc, rev := tc.c.servicesView()
	if rev != 9 {
		t.Fatalf("旧 revision 的全量列表不应覆盖 revision：rev=%d", rev)
	}
	if _, hasOld := svc["svc-old"]; hasOld {
		t.Fatalf("旧 revision 的全量列表不应覆盖服务视图: %+v", svc)
	}
	if _, hasB := svc["svc-b"]; !hasB {
		t.Fatalf("旧 revision 的全量列表不应移除 svc-b: %+v", svc)
	}

	if err := tc.stop(); err != nil {
		t.Fatalf("Run 应返回 nil，实际 %v", err)
	}
}
