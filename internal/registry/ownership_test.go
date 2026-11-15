package registry

import (
	"testing"
	"time"
)

// TestRecentOwnerRecordedOnUnregister 覆盖注销后的"最近归属"记录。
func TestRecentOwnerRecordedOnUnregister(t *testing.T) {
	r := NewServiceRegistry()
	now := time.Unix(1700000000, 0)
	r.now = func() time.Time { return now }

	if err := r.Register(ServiceEntry{ID: "svc", Proto: "tcp", AgentID: "a-1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, _, ok := r.RecentOwner("svc"); ok {
		t.Fatal("注册不应产生最近归属记录")
	}
	if err := r.Unregister("svc", "a-1"); err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	owner, at, ok := r.RecentOwner("svc")
	if !ok || owner != "a-1" {
		t.Fatalf("最近归属 = %q ok=%v，期望 a-1", owner, ok)
	}
	if !at.Equal(now) {
		t.Errorf("记录时间 = %v，期望 %v", at, now)
	}
	if r.RecentOwnerCount() != 1 {
		t.Errorf("记录数 = %d，期望 1", r.RecentOwnerCount())
	}
	if _, ok := r.Get("svc"); ok {
		t.Error("注销后在线表不应保留该服务（原有语义不变）")
	}
}

// TestRecentOwnerOnOwnershipChange 覆盖换主时保留旧 owner 痕迹。
func TestRecentOwnerOnOwnershipChange(t *testing.T) {
	r := NewServiceRegistry()
	if err := r.Register(ServiceEntry{ID: "svc", Proto: "tcp", AgentID: "a-1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// relay 判定原 owner 离线后才允许接管，这里模拟 registry 层收到换主注册。
	if err := r.Register(ServiceEntry{ID: "svc", Proto: "tcp", AgentID: "a-2"}); err != nil {
		t.Fatalf("Register 接管: %v", err)
	}
	owner, _, ok := r.RecentOwner("svc")
	if !ok || owner != "a-1" {
		t.Fatalf("换主后最近归属 = %q ok=%v，期望旧 owner a-1", owner, ok)
	}
	entry, _ := r.Get("svc")
	if entry.AgentID != "a-2" {
		t.Fatalf("在线表 owner = %q，期望 a-2", entry.AgentID)
	}
}

// TestRecentOwnerCapacity 覆盖容量上限（1024）与最旧淘汰。
func TestRecentOwnerCapacity(t *testing.T) {
	r := NewServiceRegistry()
	base := time.Unix(1700000000, 0)
	for i := 0; i < maxRecentOwners+1; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		r.RememberOwner("svc-"+itoa(i), "a-1", at)
	}
	if got := r.RecentOwnerCount(); got != maxRecentOwners {
		t.Fatalf("记录数 = %d，期望上限 %d", got, maxRecentOwners)
	}
	if _, _, ok := r.RecentOwner("svc-0"); ok {
		t.Fatal("最旧记录应被淘汰")
	}
	if owner, _, ok := r.RecentOwner("svc-" + itoa(maxRecentOwners)); !ok || owner != "a-1" {
		t.Fatal("最新记录必须保留")
	}
}

// itoa 是 strconv.Itoa 的本地别名（避免测试文件再多一个 import 分组）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
