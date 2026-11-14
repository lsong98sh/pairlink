package config

import (
	"strings"
	"testing"
	"time"
)

// TestReconnectDefaults 校验 [server]/[client] 的 reconnect 与
// reconnect_max_backoff 默认值（阶段 6C）。
func TestReconnectDefaults(t *testing.T) {
	agentINI := `
[server]
relay_addr = 127.0.0.1:7000
token = t
protocol = tcp
local = 127.0.0.1:8080
`
	cfg, err := Load(writeTempINI(t, agentINI))
	if err != nil {
		t.Fatalf("Load(agent): %v", err)
	}
	if !cfg.Server.Reconnect {
		t.Error("[server] reconnect 默认应为 true")
	}
	if got := cfg.Server.ReconnectMaxBackoff; got != 60*time.Second {
		t.Errorf("[server] reconnect_max_backoff 默认 = %s，期望 60s", got)
	}
	if err := cfg.Validate(ModeAgent); err != nil {
		t.Fatalf("Validate(agent): %v", err)
	}

	clientINI := `
[client]
relay_addr = 127.0.0.1:7000
token = t
`
	ccfg, err := Load(writeTempINI(t, clientINI))
	if err != nil {
		t.Fatalf("Load(client): %v", err)
	}
	if !ccfg.Client.Reconnect {
		t.Error("[client] reconnect 默认应为 true")
	}
	if got := ccfg.Client.ReconnectMaxBackoff; got != 60*time.Second {
		t.Errorf("[client] reconnect_max_backoff 默认 = %s，期望 60s", got)
	}
	if err := ccfg.Validate(ModeClient); err != nil {
		t.Fatalf("Validate(client): %v", err)
	}
}

// TestReconnectOverrides 校验显式配置 reconnect=false / reconnect_max_backoff。
func TestReconnectOverrides(t *testing.T) {
	agentINI := `
[server]
relay_addr = 127.0.0.1:7000
token = t
protocol = tcp
local = 127.0.0.1:8080
reconnect = false
reconnect_max_backoff = 5s
`
	cfg, err := Load(writeTempINI(t, agentINI))
	if err != nil {
		t.Fatalf("Load(agent): %v", err)
	}
	if cfg.Server.Reconnect {
		t.Error("[server] reconnect = false 未生效")
	}
	if got := cfg.Server.ReconnectMaxBackoff; got != 5*time.Second {
		t.Errorf("[server] reconnect_max_backoff = %s，期望 5s", got)
	}
	if hasWarning(cfg.Warnings, "reconnect") {
		t.Errorf("reconnect 字段不应产生未知配置警告: %v", cfg.Warnings)
	}
	if err := cfg.Validate(ModeAgent); err != nil {
		t.Fatalf("Validate(agent): %v", err)
	}

	clientINI := `
[client]
relay_addr = 127.0.0.1:7000
token = t
reconnect = false
reconnect_max_backoff = 3s
`
	ccfg, err := Load(writeTempINI(t, clientINI))
	if err != nil {
		t.Fatalf("Load(client): %v", err)
	}
	if ccfg.Client.Reconnect {
		t.Error("[client] reconnect = false 未生效")
	}
	if got := ccfg.Client.ReconnectMaxBackoff; got != 3*time.Second {
		t.Errorf("[client] reconnect_max_backoff = %s，期望 3s", got)
	}
	if err := ccfg.Validate(ModeClient); err != nil {
		t.Fatalf("Validate(client): %v", err)
	}
}

// TestReconnectMaxBackoffInvalid 校验 reconnect_max_backoff 必须 > 0。
func TestReconnectMaxBackoffInvalid(t *testing.T) {
	cases := []string{
		`
[server]
relay_addr = 127.0.0.1:7000
token = t
protocol = tcp
local = 127.0.0.1:8080
reconnect_max_backoff = 0s
`,
		`
[server]
relay_addr = 127.0.0.1:7000
token = t
protocol = tcp
local = 127.0.0.1:8080
reconnect_max_backoff = -1s
`,
		`
[server]
relay_addr = 127.0.0.1:7000
token = t
protocol = tcp
local = 127.0.0.1:8080
reconnect_max_backoff = abc
`,
	}
	for i, content := range cases {
		if _, err := Load(writeTempINI(t, content)); err == nil {
			t.Fatalf("case %d: 非法 reconnect_max_backoff 应返回错误", i)
		} else if !strings.Contains(err.Error(), "reconnect_max_backoff") {
			t.Fatalf("case %d: 错误信息应点名字段，实际: %v", i, err)
		}
	}
}

// TestReconnectInvalidBool 校验 reconnect 只接受合法布尔值。
func TestReconnectInvalidBool(t *testing.T) {
	content := `
[client]
relay_addr = 127.0.0.1:7000
token = t
reconnect = maybe
`
	if _, err := Load(writeTempINI(t, content)); err == nil {
		t.Fatal("reconnect = maybe 应返回错误")
	}
}
