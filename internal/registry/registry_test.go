package registry

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/pairing"
)

// fakeConn 是测试用的 Conn 实现。
type fakeConn struct {
	addr   string
	closed bool
	mu     sync.Mutex
}

func (c *fakeConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

func (c *fakeConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234}
}

func key(service, conn string) pairing.PairingKey {
	return pairing.PairingKey{ServiceID: service, ClientConnID: conn}
}

// drainEvents 收集订阅回调事件。
type eventSink struct {
	mu     sync.Mutex
	events []Event
}

func (s *eventSink) add(ev Event) {
	s.mu.Lock()
	s.events = append(s.events, ev)
	s.mu.Unlock()
}

func (s *eventSink) all() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.events...)
}

func TestServiceRegistryBasic(t *testing.T) {
	r := NewServiceRegistry()
	if r.Revision() != 0 || r.Len() != 0 {
		t.Fatalf("新注册表应为空: rev=%d len=%d", r.Revision(), r.Len())
	}
	if err := r.Register(ServiceEntry{ID: ""}); err == nil {
		t.Fatal("空服务ID 应报错")
	}

	e := ServiceEntry{ID: "web", Proto: "tcp", AgentID: "a1", AgentAddr: "1.2.3.4:1", Online: true}
	if err := r.Register(e); err != nil {
		t.Fatalf("Register 失败: %v", err)
	}
	if r.Revision() != 1 || r.Len() != 1 {
		t.Fatalf("注册后 rev=%d len=%d, want 1/1", r.Revision(), r.Len())
	}
	got, ok := r.Get("web")
	if !ok || got.ID != "web" || got.AgentID != "a1" || got.RegisteredAt.IsZero() {
		t.Fatalf("Get 结果错误: %+v ok=%v", got, ok)
	}
	if _, ok := r.Get("nope"); ok {
		t.Fatal("不存在的服务不应返回 ok")
	}

	if err := r.Register(ServiceEntry{ID: "db", Proto: "udp", AgentID: "a2"}); err != nil {
		t.Fatalf("Register 失败: %v", err)
	}
	list := r.List()
	if len(list) != 2 || list[0].ID != "db" || list[1].ID != "web" {
		t.Fatalf("List 应按 ID 排序: %+v", list)
	}
	byAgent := r.ListByAgent("a2")
	if len(byAgent) != 1 || byAgent[0].ID != "db" {
		t.Fatalf("ListByAgent 错误: %+v", byAgent)
	}

	// 重复注册同一服务产生 updated 事件。
	if err := r.Register(ServiceEntry{ID: "web", Proto: "tcp", AgentID: "a1", Online: false}); err != nil {
		t.Fatalf("重复注册失败: %v", err)
	}
	if r.Len() != 2 || r.Revision() != 3 {
		t.Fatalf("重复注册后 len=%d rev=%d", r.Len(), r.Revision())
	}
	got, _ = r.Get("web")
	if got.Online {
		t.Fatal("重复注册应覆盖字段")
	}
}

func TestServiceRegistryUnregister(t *testing.T) {
	r := NewServiceRegistry()
	if err := r.Register(ServiceEntry{ID: "web", Proto: "tcp", AgentID: "a1"}); err != nil {
		t.Fatalf("Register 失败: %v", err)
	}
	if err := r.Unregister("none", ""); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("注销不存在服务应返回 ErrServiceNotFound，实际: %v", err)
	}
	if err := r.Unregister("web", "a2"); !errors.Is(err, ErrServiceOwnerMismatch) {
		t.Fatalf("agent 不匹配应返回 ErrServiceOwnerMismatch，实际: %v", err)
	}
	if err := r.Unregister("web", "a1"); err != nil {
		t.Fatalf("注销失败: %v", err)
	}
	if r.Len() != 0 || r.Revision() != 2 {
		t.Fatalf("注销后 len=%d rev=%d", r.Len(), r.Revision())
	}
}

func TestServiceRegistryUnregisterAgent(t *testing.T) {
	r := NewServiceRegistry()
	for _, e := range []ServiceEntry{
		{ID: "a1-svc", AgentID: "a1"},
		{ID: "a2-svc", AgentID: "a2"},
		{ID: "a1-svc2", AgentID: "a1"},
	} {
		if err := r.Register(e); err != nil {
			t.Fatalf("Register 失败: %v", err)
		}
	}
	removed := r.UnregisterAgent("a1")
	if len(removed) != 2 || removed[0] != "a1-svc" || removed[1] != "a1-svc2" {
		t.Fatalf("UnregisterAgent = %v, want [a1-svc a1-svc2]", removed)
	}
	if r.Len() != 1 {
		t.Fatalf("剩余服务数 = %d, want 1", r.Len())
	}
	if got := r.UnregisterAgent("missing"); len(got) != 0 {
		t.Fatalf("不存在的 agent 应返回空: %v", got)
	}
}

func TestServiceRegistrySubscribe(t *testing.T) {
	r := NewServiceRegistry()
	var sink eventSink
	cancel := r.Subscribe(sink.add)
	if err := r.Register(ServiceEntry{ID: "web", AgentID: "a1"}); err != nil {
		t.Fatalf("Register 失败: %v", err)
	}
	if err := r.Unregister("web", ""); err != nil {
		t.Fatalf("Unregister 失败: %v", err)
	}
	events := sink.all()
	if len(events) != 2 {
		t.Fatalf("事件数 = %d, want 2: %+v", len(events), events)
	}
	if events[0].Type != EventAdded || events[1].Type != EventRemoved {
		t.Fatalf("事件类型错误: %+v", events)
	}
	if events[0].Revision != 1 || events[1].Revision != 2 {
		t.Fatalf("事件版本号错误: %+v", events)
	}
	cancel()
	cancel()
	r.Register(ServiceEntry{ID: "db", AgentID: "a1"})
	if got := len(sink.all()); got != 2 {
		t.Fatalf("取消订阅后不应再收到事件，实际 %d", got)
	}
	if r.Subscribe(nil) == nil {
		t.Fatal("Subscribe(nil) 应返回可调用的取消函数")
	}
}

func TestServiceRegistryConcurrent(t *testing.T) {
	r := NewServiceRegistry()
	var wg sync.WaitGroup
	const n = 50
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("svc-%d", i)
			if err := r.Register(ServiceEntry{ID: id, Proto: "tcp", AgentID: "a1"}); err != nil {
				t.Errorf("Register 失败: %v", err)
			}
			_ = r.List()
			_ = r.ListByAgent("a1")
			_ = r.Revision()
			if err := r.Unregister(id, "a1"); err != nil {
				t.Errorf("Unregister 失败: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if r.Len() != 0 {
		t.Fatalf("并发注册/注销后应为空，实际 %d", r.Len())
	}
	if r.Revision() != uint64(2*n) {
		t.Fatalf("Revision = %d, want %d", r.Revision(), 2*n)
	}
}

func TestAgentRegistry(t *testing.T) {
	r := NewAgentRegistry()
	if r.Count() != 0 {
		t.Fatalf("新表应为空")
	}
	if err := r.Add(nil); err == nil {
		t.Fatal("Add(nil) 应报错")
	}
	if err := r.Add(&Agent{}); err == nil {
		t.Fatal("空 ID 应报错")
	}

	ctl := &fakeConn{}
	a := &Agent{ID: "a1", RemoteAddr: "10.0.0.1:5000", Services: []string{"web"}, Control: ctl}
	if err := r.Add(a); err != nil {
		t.Fatalf("Add 失败: %v", err)
	}
	if err := r.Add(a); !errors.Is(err, ErrAgentExists) {
		t.Fatalf("重复 Add 应返回 ErrAgentExists，实际: %v", err)
	}
	// 存储的是副本，修改入参不影响注册表。
	a.Services = append(a.Services, "hacked")
	if got, ok := r.Get("a1"); !ok || len(got.Services) != 1 || got.Services[0] != "web" {
		t.Fatalf("Add 应存储副本: %+v ok=%v", got, ok)
	}
	if got, _ := r.Get("a1"); got.ConnectedAt.IsZero() || got.LastSeen.IsZero() {
		t.Fatal("ConnectedAt/LastSeen 应自动填充")
	}
	if _, ok := r.Get("nope"); ok {
		t.Fatal("不存在的 agent 不应返回 ok")
	}
	if err := r.Touch("a1", time.Unix(1800000000, 0)); err != nil {
		t.Fatalf("Touch 失败: %v", err)
	}
	if got, _ := r.Get("a1"); !got.LastSeen.Equal(time.Unix(1800000000, 0)) {
		t.Fatalf("Touch 未生效: %v", got.LastSeen)
	}
	if err := r.Touch("nope", time.Time{}); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("Touch 不存在 agent 应报错，实际: %v", err)
	}
	if err := r.SetServices("a1", []string{"web", "db"}); err != nil {
		t.Fatalf("SetServices 失败: %v", err)
	}
	if got, _ := r.Get("a1"); len(got.Services) != 2 {
		t.Fatalf("SetServices 未生效: %+v", got.Services)
	}
	if err := r.SetServices("nope", nil); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("SetServices 不存在 agent 应报错，实际: %v", err)
	}

	if err := r.Add(&Agent{ID: "a0", Control: &fakeConn{}}); err != nil {
		t.Fatalf("Add 失败: %v", err)
	}
	list := r.List()
	if len(list) != 2 || list[0].ID != "a0" || list[1].ID != "a1" {
		t.Fatalf("List 应按 ID 排序: %+v", list)
	}
	if r.Count() != 2 {
		t.Fatalf("Count = %d, want 2", r.Count())
	}
	if err := r.Remove("a1"); err != nil {
		t.Fatalf("Remove 失败: %v", err)
	}
	if err := r.Remove("a1"); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("重复 Remove 应报错，实际: %v", err)
	}
	if r.Count() != 1 {
		t.Fatalf("Count = %d, want 1", r.Count())
	}
}

func TestAgentRegistryConcurrent(t *testing.T) {
	r := NewAgentRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("agent-%d", i)
			if err := r.Add(&Agent{ID: id, Control: &fakeConn{}}); err != nil {
				t.Errorf("Add 失败: %v", err)
			}
			_ = r.List()
			_ = r.Count()
			_ = r.Touch(id, time.Now())
			if err := r.Remove(id); err != nil {
				t.Errorf("Remove 失败: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if r.Count() != 0 {
		t.Fatalf("并发后应为空，实际 %d", r.Count())
	}
}

func TestUDPEndpoint(t *testing.T) {
	ep, err := UDPEndpointFromString("127.0.0.1:5000")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !ep.IsValid() || ep.String() != "127.0.0.1:5000" {
		t.Fatalf("端点错误: %+v %q", ep, ep.String())
	}
	if got := ep.AddrPort(); got != netip.MustParseAddrPort("127.0.0.1:5000") {
		t.Fatalf("AddrPort 错误: %v", got)
	}
	if (UDPEndpoint{}).IsValid() || (UDPEndpoint{}).String() != "<invalid>" {
		t.Fatal("零值端点应为非法")
	}
	if _, err := UDPEndpointFromString("bad"); err == nil {
		t.Fatal("非法端点应报错")
	}
	if _, err := UDPEndpointFromNetAddr(nil); !errors.Is(err, ErrUDPEndpointInvalid) {
		t.Fatalf("nil 地址应报错，实际: %v", err)
	}
	ua := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 6000}
	if ep, err := UDPEndpointFromNetAddr(ua); err != nil || ep.String() != "127.0.0.1:6000" {
		t.Fatalf("从 net.UDPAddr 构造失败: %v %v", ep, err)
	}
}

func TestUDPTableHalfToFull(t *testing.T) {
	tbl := NewUDPTable()
	k := key("game", "c-1")
	agentEP := UDPEndpoint{IP: netip.MustParseAddr("10.0.0.1"), Port: 1111}
	clientEP := UDPEndpoint{IP: netip.MustParseAddr("10.0.0.2"), Port: 2222}

	full, peer, err := tbl.RegisterHalf(k, pairing.SideAgent, agentEP)
	if err != nil {
		t.Fatalf("RegisterHalf 失败: %v", err)
	}
	if full {
		t.Fatal("只有一侧时 full 应为 false")
	}
	if peer.IsValid() {
		t.Fatalf("对端未到时 peer 应为零值: %v", peer)
	}
	if tbl.Len() != 1 {
		t.Fatalf("Len = %d, want 1", tbl.Len())
	}
	// 同侧幂等。
	if full, _, err := tbl.RegisterHalf(k, pairing.SideAgent, agentEP); err != nil || full {
		t.Fatalf("幂等登记失败: full=%v err=%v", full, err)
	}

	full, peer, err = tbl.RegisterHalf(k, pairing.SideClient, clientEP)
	if err != nil {
		t.Fatalf("RegisterHalf 失败: %v", err)
	}
	if !full {
		t.Fatal("两侧到齐后 full 应为 true")
	}
	if peer != agentEP {
		t.Fatalf("client 侧看到的 peer = %v, want %v", peer, agentEP)
	}

	// 反向查询。
	gotKey, gotPeer, ok := tbl.LookupByEndpoint(clientEP)
	if !ok || gotKey != k || gotPeer != agentEP {
		t.Fatalf("LookupByEndpoint(client) = %v %v %v", gotKey, gotPeer, ok)
	}
	gotKey, gotPeer, ok = tbl.LookupByEndpoint(agentEP)
	if !ok || gotKey != k || gotPeer != clientEP {
		t.Fatalf("LookupByEndpoint(agent) = %v %v %v", gotKey, gotPeer, ok)
	}
	if _, _, ok := tbl.LookupByEndpoint(UDPEndpoint{IP: netip.MustParseAddr("10.9.9.9"), Port: 1}); ok {
		t.Fatal("未登记端点不应查到")
	}

	entry, ok := tbl.LookupByKey(k)
	if !ok || !entry.Complete || entry.Agent != agentEP || entry.Client != clientEP {
		t.Fatalf("LookupByKey 错误: %+v ok=%v", entry, ok)
	}
	snap := tbl.Snapshot()
	if len(snap) != 1 || !snap[0].Complete {
		t.Fatalf("Snapshot 错误: %+v", snap)
	}
}

func TestUDPTableConflicts(t *testing.T) {
	tbl := NewUDPTable()
	k1, k2 := key("a", "1"), key("b", "2")
	ep1 := UDPEndpoint{IP: netip.MustParseAddr("10.0.0.1"), Port: 1000}
	ep2 := UDPEndpoint{IP: netip.MustParseAddr("10.0.0.1"), Port: 1001}

	if _, _, err := tbl.RegisterHalf(k1, pairing.SideAgent, ep1); err != nil {
		t.Fatalf("RegisterHalf 失败: %v", err)
	}
	// 同侧换地址。
	if _, _, err := tbl.RegisterHalf(k1, pairing.SideAgent, ep2); !errors.Is(err, ErrUDPEndpointConflict) {
		t.Fatalf("同侧换端点应返回 ErrUDPEndpointConflict，实际: %v", err)
	}
	// 端点被其它配对键占用。
	if _, _, err := tbl.RegisterHalf(k2, pairing.SideClient, ep1); !errors.Is(err, ErrUDPEndpointConflict) {
		t.Fatalf("端点冲突应报错，实际: %v", err)
	}
	// 非法端点与非法侧别。
	if _, _, err := tbl.RegisterHalf(k1, pairing.SideClient, UDPEndpoint{}); !errors.Is(err, ErrUDPEndpointInvalid) {
		t.Fatalf("非法端点应报错，实际: %v", err)
	}
	if _, _, err := tbl.RegisterHalf(k1, pairing.SideUnknown, ep1); !errors.Is(err, pairing.ErrInvalidSide) {
		t.Fatalf("非法侧别应报错，实际: %v", err)
	}
}

func TestUDPTableSweep(t *testing.T) {
	clock := time.Unix(1700000000, 0)
	tbl := NewUDPTable()
	tbl.SetClock(func() time.Time { return clock })

	k1, k2 := key("a", "1"), key("b", "2")
	ep := func(port uint16) UDPEndpoint {
		return UDPEndpoint{IP: netip.MustParseAddr("10.0.0.1"), Port: port}
	}
	if _, _, err := tbl.RegisterHalf(k1, pairing.SideAgent, ep(1000)); err != nil {
		t.Fatalf("RegisterHalf 失败: %v", err)
	}
	if _, _, err := tbl.RegisterHalf(k1, pairing.SideClient, ep(1001)); err != nil {
		t.Fatalf("RegisterHalf 失败: %v", err)
	}
	clock = clock.Add(10 * time.Second)
	if _, _, err := tbl.RegisterHalf(k2, pairing.SideAgent, ep(2000)); err != nil {
		t.Fatalf("RegisterHalf 失败: %v", err)
	}

	// k1 已空闲 10s，k2 空闲 0s。
	removed := tbl.Sweep(clock, 5*time.Second)
	if len(removed) != 1 || removed[0] != k1 {
		t.Fatalf("Sweep = %v, want [%v]", removed, k1)
	}
	if tbl.Len() != 1 {
		t.Fatalf("剩余会话数 = %d, want 1", tbl.Len())
	}
	if _, _, ok := tbl.LookupByEndpoint(ep(1000)); ok {
		t.Fatal("被清理会话的端点不应仍可查到")
	}
	// 重复 Sweep 幂等。
	if got := tbl.Sweep(clock, 5*time.Second); len(got) != 0 {
		t.Fatalf("重复 Sweep 应为空: %v", got)
	}
	// timeout <= 0 不清理。
	if got := tbl.Sweep(clock.Add(time.Hour), 0); len(got) != 0 {
		t.Fatalf("timeout<=0 不应清理: %v", got)
	}
	if tbl.Len() != 1 {
		t.Fatalf("timeout<=0 后会话数 = %d, want 1", tbl.Len())
	}
}

func TestUDPTableTouchAndRemove(t *testing.T) {
	tbl := NewUDPTable()
	k := key("a", "1")
	ep1 := UDPEndpoint{IP: netip.MustParseAddr("10.0.0.1"), Port: 1000}
	ep2 := UDPEndpoint{IP: netip.MustParseAddr("10.0.0.1"), Port: 1001}
	if _, _, err := tbl.RegisterHalf(k, pairing.SideAgent, ep1); err != nil {
		t.Fatalf("RegisterHalf 失败: %v", err)
	}
	if _, _, err := tbl.RegisterHalf(k, pairing.SideClient, ep2); err != nil {
		t.Fatalf("RegisterHalf 失败: %v", err)
	}
	later := time.Now().Add(time.Minute)
	tbl.Touch(k, later)
	entry, ok := tbl.LookupByKey(k)
	if !ok || !entry.LastSeen.Equal(later) {
		t.Fatalf("Touch 未生效: %+v ok=%v", entry, ok)
	}
	tbl.Touch(key("none", "x"), later)
	tbl.Remove(k)
	tbl.Remove(k)
	if tbl.Len() != 0 {
		t.Fatalf("Remove 后应为空: %d", tbl.Len())
	}
	if _, _, ok := tbl.LookupByEndpoint(ep1); ok {
		t.Fatal("Remove 后端点索引应清理")
	}
}

func TestUDPTableConcurrent(t *testing.T) {
	tbl := NewUDPTable()
	var wg sync.WaitGroup
	const n = 50
	ep := func(i int) UDPEndpoint {
		return UDPEndpoint{IP: netip.MustParseAddr("10.0.0.1"), Port: uint16(10000 + i)}
	}
	clientEP := func(i int) UDPEndpoint {
		return UDPEndpoint{IP: netip.MustParseAddr("10.0.0.2"), Port: uint16(20000 + i)}
	}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k := key("svc", fmt.Sprintf("c-%d", i))
			if _, _, err := tbl.RegisterHalf(k, pairing.SideAgent, ep(i)); err != nil {
				t.Errorf("RegisterHalf 失败: %v", err)
				return
			}
			if _, _, err := tbl.RegisterHalf(k, pairing.SideClient, clientEP(i)); err != nil {
				t.Errorf("RegisterHalf 失败: %v", err)
				return
			}
			_, _, _ = tbl.LookupByEndpoint(ep(i))
			_ = tbl.Snapshot()
			tbl.Touch(k, time.Now())
			tbl.Remove(k)
		}(i)
	}
	wg.Wait()
	if tbl.Len() != 0 {
		t.Fatalf("并发后应为空，实际 %d", tbl.Len())
	}
}
