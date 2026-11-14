package relay

import (
	"net"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/protocol"
	"github.com/lsong98sh/pairlink/internal/registry"
)

// readCtrlFrame 从控制连接读一帧并解码。
func readCtrlFrame(t *testing.T, conn net.Conn, timeout time.Duration) (protocol.MessageType, any) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	typ, payload, err := protocol.ReadFrame(conn)
	if err != nil {
		t.Fatalf("读取控制帧失败: %v", err)
	}
	msg, err := protocol.Decode(typ, payload)
	if err != nil {
		t.Fatalf("解析控制帧 %s: %v", typ, err)
	}
	return typ, msg
}

// addServiceForTest 直接向注册表写入一个服务，返回其条目与版本号。
func addServiceForTest(t *testing.T, s *server, id string) (registry.ServiceEntry, uint64) {
	t.Helper()
	entry := registry.ServiceEntry{
		ID:      id,
		Proto:   "tcp",
		AgentID: "a-0123456789abcdef",
		Online:  true,
	}
	if err := s.services.Register(entry); err != nil {
		t.Fatalf("注册服务 %s: %v", id, err)
	}
	return entry, s.services.Revision()
}

// newPushTestConn 造一条只跑 pushLoop 的假控制连接（net.Pipe，测试侧读帧）。
func newPushTestConn(t *testing.T, s *server) (*tunnelConn, net.Conn) {
	t.Helper()
	srvConn, cliConn := net.Pipe()
	tc := &tunnelConn{
		srv:   s,
		conn:  srvConn,
		log:   s.log,
		done:  make(chan struct{}),
		subCh: make(chan registry.Event, 8),
	}
	t.Cleanup(func() {
		tc.close()
		_ = cliConn.Close()
	})
	go tc.pushLoop()
	return tc, cliConn
}

// TestSubscribeRevisionResync 覆盖"订阅推送丢事件/乱序"的兜底逻辑。
//
// 关键不变量：client 的视图不能被永久卡在旧版本。pushLoop 只在 revision **严格连续**时
// 推单条 ServiceEvent；一旦发现断档（队列满丢事件、多个写入者并发 dispatch 导致乱序），
// 就改发一次全量 MsgServiceList，让 client 整体重同步。
func TestSubscribeRevisionResync(t *testing.T) {
	s := startTestServer(t, testConfig(t, nil))
	tc, conn := newPushTestConn(t, s)

	// 1) revision 1：正常单条事件。
	svcA, revA := addServiceForTest(t, s, "svc-a")
	tc.subCh <- registry.Event{Type: registry.EventAdded, Service: svcA, Revision: revA}
	typ, msg := readCtrlFrame(t, conn, 2*time.Second)
	if typ != protocol.MsgServiceEvent {
		t.Fatalf("连续 revision 应推 ServiceEvent，实际 %s", typ)
	}
	ev, ok := msg.(*protocol.ServiceEvent)
	if !ok || ev.Revision != revA || ev.Service.ServiceID != "svc-a" {
		t.Fatalf("事件内容异常: %+v", msg)
	}

	// 2) revision 2 的事件被"丢弃"（pushLoop 没收到），revision 3 到达 → 断档。
	_, _ = addServiceForTest(t, s, "svc-b")
	svcC, revC := addServiceForTest(t, s, "svc-c")
	tc.subCh <- registry.Event{Type: registry.EventAdded, Service: svcC, Revision: revC}

	typ, msg = readCtrlFrame(t, conn, 2*time.Second)
	if typ != protocol.MsgServiceList {
		t.Fatalf("revision 断档应改发全量 ServiceList，实际 %s: %+v", typ, msg)
	}
	list, ok := msg.(*protocol.ServiceList)
	if !ok {
		t.Fatalf("ServiceList 类型错误: %T", msg)
	}
	got := map[string]bool{}
	for _, svc := range list.Services {
		got[svc.ServiceID] = true
	}
	for _, want := range []string{"svc-a", "svc-b", "svc-c"} {
		if !got[want] {
			t.Fatalf("全量重同步缺少 %q，实际 %+v", want, list.Services)
		}
	}
	if list.Revision < revC {
		t.Fatalf("重同步 revision = %d, 期望 >= %d", list.Revision, revC)
	}

	// 3) 迟到的旧事件必须被丢弃，不能把 client 的视图推回去。
	tc.subCh <- registry.Event{Type: registry.EventAdded, Service: svcA, Revision: revA + 1}
	svcD, revD := addServiceForTest(t, s, "svc-d")
	tc.subCh <- registry.Event{Type: registry.EventAdded, Service: svcD, Revision: revD}

	typ, msg = readCtrlFrame(t, conn, 2*time.Second)
	ev, ok = msg.(*protocol.ServiceEvent)
	if typ != protocol.MsgServiceEvent || !ok || ev.Revision != revD {
		t.Fatalf("旧事件应被丢弃、只应收到 revision %d 的新事件，实际 %s: %+v", revD, typ, msg)
	}

	// 4) 乱序：先到 revision 6、后到 revision 5（两个写入者并发 dispatch 的情形）。
	_, _ = addServiceForTest(t, s, "svc-e")
	svcF, revF := addServiceForTest(t, s, "svc-f")
	tc.subCh <- registry.Event{Type: registry.EventAdded, Service: svcF, Revision: revF}
	typ, msg = readCtrlFrame(t, conn, 2*time.Second)
	if typ != protocol.MsgServiceList {
		t.Fatalf("乱序（跳号）应改发全量 ServiceList，实际 %s: %+v", typ, msg)
	}
	list, ok = msg.(*protocol.ServiceList)
	if !ok {
		t.Fatalf("ServiceList 类型错误: %T", msg)
	}
	if len(list.Services) < 6 {
		t.Fatalf("全量重同步服务数 = %d, 期望 >= 6", len(list.Services))
	}

	// 后到的 revision 5 属于旧事件：必须被静默丢弃（不能被当作正常事件推送）。
	tc.subCh <- registry.Event{Type: registry.EventAdded, Service: svcF, Revision: revF - 1}
	_ = conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
	if typ, payload, err := protocol.ReadFrame(conn); err == nil {
		t.Fatalf("旧事件不应产生任何帧，实际收到 %s: %s", typ, payload)
	}
	_ = conn.SetReadDeadline(time.Time{})
}
