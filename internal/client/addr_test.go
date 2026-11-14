package client

import (
	"strings"
	"testing"
	"time"
)

// TestNegotiateHeartbeat 覆盖心跳间隔取 min(本地, relay 建议)。
func TestNegotiateHeartbeat(t *testing.T) {
	cases := []struct {
		name      string
		local     time.Duration
		suggested int
		want      time.Duration
	}{
		{name: "suggested-smaller", local: 30 * time.Second, suggested: 5, want: 5 * time.Second},
		{name: "local-smaller", local: 2 * time.Second, suggested: 30, want: 2 * time.Second},
		{name: "no-suggestion", local: 30 * time.Second, suggested: 0, want: 30 * time.Second},
		{name: "zero-local", local: 0, suggested: 10, want: 10 * time.Second},
		{name: "floor-one-second", local: 0, suggested: 0, want: 1 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := negotiateHeartbeat(c.local, c.suggested); got != c.want {
				t.Fatalf("negotiateHeartbeat = %v, 期望 %v", got, c.want)
			}
		})
	}
}

// TestDataAddr 覆盖 §4.5 的数据面地址推导：IPv4 / IPv6 / 域名 / ini 回退 / 错误分支。
func TestDataAddr(t *testing.T) {
	cases := []struct {
		name     string
		host     string
		port     int
		fallback string
		want     string
		wantErr  bool
	}{
		{name: "ipv4-hello-port", host: "127.0.0.1", port: 7001, want: "127.0.0.1:7001"},
		{name: "ipv6-hello-port", host: "::1", port: 7001, want: "[::1]:7001"},
		{name: "domain-hello-port", host: "relay.example.com", port: 7001, want: "relay.example.com:7001"},
		{name: "ini-fallback-ipv4", host: "10.0.0.1", fallback: "0.0.0.0:7001", want: "10.0.0.1:7001"},
		{name: "ini-fallback-ipv6", host: "2001:db8::1", fallback: "0.0.0.0:7001", want: "[2001:db8::1]:7001"},
		{name: "no-port", host: "10.0.0.1", wantErr: true},
		{name: "no-host", port: 7001, wantErr: true},
		{name: "port-out-of-range", host: "10.0.0.1", port: 70000, wantErr: true},
		{name: "bad-fallback", host: "10.0.0.1", fallback: "not-an-addr", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := dataAddr(c.host, c.port, c.fallback)
			if c.wantErr {
				if err == nil {
					t.Fatalf("期望错误，实际得到 %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("dataAddr 返回错误: %v", err)
			}
			if got != c.want {
				t.Fatalf("dataAddr = %q, 期望 %q", got, c.want)
			}
		})
	}
}

// TestNewConnID 覆盖 client_conn_id 的格式与唯一性。
func TestNewConnID(t *testing.T) {
	seen := make(map[string]bool, 64)
	for i := 0; i < 64; i++ {
		id, err := newConnID()
		if err != nil {
			t.Fatalf("newConnID 失败: %v", err)
		}
		if len(id) != 16 {
			t.Fatalf("client_conn_id 长度 = %d, 期望 16: %q", len(id), id)
		}
		if id != strings.ToLower(id) {
			t.Fatalf("client_conn_id 应是小写 hex: %q", id)
		}
		if seen[id] {
			t.Fatalf("client_conn_id 重复: %q", id)
		}
		seen[id] = true
	}
}
