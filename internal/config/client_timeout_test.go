package config

import (
	"strings"
	"testing"
	"time"
)

// TestClientUDPSessionTimeout 覆盖 [client] udp_session_timeout 的解析、默认值与校验。
//
// 该字段是阶段 4 的契约变更：阶段 2/3 的 client 侧 UDP 空闲清理写死 60s 常量，
// 现在与 [server] 段对齐为可配置项（默认仍为 60s，必须为正）。
func TestClientUDPSessionTimeout(t *testing.T) {
	t.Run("默认 60s", func(t *testing.T) {
		path := writeTempINI(t, `[client]
relay_addr = relay.example.com:7000
token = t0k
`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got, want := cfg.Client.UDPSessionTimeout, 60*time.Second; got != want {
			t.Fatalf("udp_session_timeout 默认值 = %v, want %v", got, want)
		}
	})

	t.Run("显式配置", func(t *testing.T) {
		path := writeTempINI(t, `[client]
relay_addr = relay.example.com:7000
token = t0k
udp_session_timeout = 250ms
`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got, want := cfg.Client.UDPSessionTimeout, 250*time.Millisecond; got != want {
			t.Fatalf("udp_session_timeout = %v, want %v", got, want)
		}
		if err := cfg.Validate(ModeClient); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("非正值报错", func(t *testing.T) {
		for _, bad := range []string{"0s", "-5s"} {
			path := writeTempINI(t, `[client]
relay_addr = relay.example.com:7000
token = t0k
udp_session_timeout = `+bad+`
`)
			if _, err := Load(path); err == nil {
				t.Fatalf("udp_session_timeout = %q 应报错", bad)
			} else if !strings.Contains(err.Error(), "必须大于 0") {
				t.Fatalf("udp_session_timeout = %q 的错误信息不可读: %v", bad, err)
			}
		}
	})

	t.Run("绕过 ini 的负值被 Validate 拒绝", func(t *testing.T) {
		cfg := newDefault()
		cfg.Client.RelayAddr = "relay.example.com:7000"
		cfg.Client.Token = "t0k"
		cfg.Client.UDPSessionTimeout = -time.Second
		err := cfg.Validate(ModeClient)
		if err == nil || !strings.Contains(err.Error(), "udp_session_timeout") {
			t.Fatalf("负值应被 Validate 拒绝，实际 %v", err)
		}
	})
}
