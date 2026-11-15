package config

import (
	"strings"
	"testing"
)

// TestUDPDTLSDefaults 三端 udp_dtls 默认都是 true（v0.5.0 默认开启 DTLS-PSK）。
func TestUDPDTLSDefaults(t *testing.T) {
	relay, err := Load(writeTempINI(t, "[relay]\ntunnel_listen = 127.0.0.1:17000\ntcp_listen = 127.0.0.1:17001\nauth_token = t\n"))
	if err != nil {
		t.Fatalf("Load(relay): %v", err)
	}
	if !relay.Relay.UDPDTLS {
		t.Fatal("relay udp_dtls 默认应为 true")
	}

	agent, err := Load(writeTempINI(t, "[server]\nrelay_addr = 127.0.0.1:17000\ntoken = t\n"))
	if err != nil {
		t.Fatalf("Load(agent): %v", err)
	}
	if !agent.Server.UDPDTLS {
		t.Fatal("agent udp_dtls 默认应为 true")
	}

	client, err := Load(writeTempINI(t, "[client]\nrelay_addr = 127.0.0.1:17000\ntoken = t\nservice.default = 127.0.0.1:18000\n"))
	if err != nil {
		t.Fatalf("Load(client): %v", err)
	}
	if !client.Client.UDPDTLS {
		t.Fatal("client udp_dtls 默认应为 true")
	}
}

// TestUDPDTLSParse 覆盖三端 udp_dtls 的显式取值与严格解析。
func TestUDPDTLSParse(t *testing.T) {
	cases := []struct {
		name    string
		section string
		body    string
		want    bool
		wantErr string
	}{
		{name: "relay_false", section: "relay", body: "tunnel_listen = 127.0.0.1:17000\ntcp_listen = 127.0.0.1:17001\nauth_token = t\nudp_dtls = false", want: false},
		{name: "relay_true", section: "relay", body: "tunnel_listen = 127.0.0.1:17000\ntcp_listen = 127.0.0.1:17001\nauth_token = t\nudp_dtls = true", want: true},
		{name: "relay_大写", section: "relay", body: "tunnel_listen = 127.0.0.1:17000\ntcp_listen = 127.0.0.1:17001\nauth_token = t\nudp_dtls = FALSE", want: false},
		{name: "relay_非法", section: "relay", body: "tunnel_listen = 127.0.0.1:17000\ntcp_listen = 127.0.0.1:17001\nauth_token = t\nudp_dtls = 0", wantErr: "udp_dtls"},
		{name: "agent_false", section: "server", body: "relay_addr = 127.0.0.1:17000\ntoken = t\nudp_dtls = false", want: false},
		{name: "agent_非法", section: "server", body: "relay_addr = 127.0.0.1:17000\ntoken = t\nudp_dtls = yes", wantErr: "udp_dtls"},
		{name: "client_false", section: "client", body: "relay_addr = 127.0.0.1:17000\ntoken = t\nservice.default = 127.0.0.1:18000\nudp_dtls = false", want: false},
		{name: "client_非法", section: "client", body: "relay_addr = 127.0.0.1:17000\ntoken = t\nservice.default = 127.0.0.1:18000\nudp_dtls = on", wantErr: "udp_dtls"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeTempINI(t, "["+tc.section+"]\n"+tc.body+"\n"))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("期望解析失败（包含 %q），实际 err=%v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			var got bool
			switch tc.section {
			case "relay":
				got = cfg.Relay.UDPDTLS
			case "server":
				got = cfg.Server.UDPDTLS
			default:
				got = cfg.Client.UDPDTLS
			}
			if got != tc.want {
				t.Fatalf("udp_dtls = %v，期望 %v", got, tc.want)
			}
		})
	}
}

// TestValidateUDPDTLS 覆盖 udp_dtls 的 WARN 与 token 速率限制的校验规则。
func TestValidateUDPDTLS(t *testing.T) {
	base := "[relay]\ntunnel_listen = 127.0.0.1:17000\ntcp_listen = 127.0.0.1:17001\nauth_token = t\n"
	cases := []struct {
		name    string
		extra   string
		wantErr string
		wantWrn []string
	}{
		{name: "默认_dtls_无警告", extra: ""},
		{name: "默认_dtls_native_无警告", extra: "udp_listen = 127.0.0.1:17002\n"},
		{name: "关闭_dtls_native_有警告", extra: "udp_dtls = false\nudp_listen = 127.0.0.1:17002\n",
			wantWrn: []string{"udp_dtls = false"}},
		{name: "关闭_dtls_tcp_tunnel_无数据面警告", extra: "udp_dtls = false\n"},

		{name: "token_rate_负数", extra: "token_rate_per_ip = -1\n", wantErr: "token_rate_per_ip"},
		{name: "token_burst_负数", extra: "token_burst_per_ip = -1\n", wantErr: "token_burst_per_ip"},
		{name: "开启限流但_burst_为0", extra: "token_rate_per_ip = 10\ntoken_burst_per_ip = 0\n", wantErr: "token_burst_per_ip"},
		{name: "默认值合法", extra: ""},
		{name: "rate0_忽略_burst", extra: "token_rate_per_ip = 0\ntoken_burst_per_ip = 0\n"},
		{name: "rate0_忽略负_burst", extra: "token_rate_per_ip = 0\ntoken_burst_per_ip = -5\n", wantErr: "token_burst_per_ip"},
		{name: "自定义合法", extra: "token_rate_per_ip = 5\ntoken_burst_per_ip = 5\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeTempINI(t, base+tc.extra))
			if tc.wantErr != "" {
				// 部分规则在解析阶段就拒绝（Load 内部会跑 Validate）。
				if err == nil {
					err = cfg.Validate(ModeRelay)
				}
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("期望校验失败（包含 %q），实际 err=%v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if err := cfg.Validate(ModeRelay); err != nil {
				t.Fatalf("期望校验通过，实际 %v", err)
			}
			// 默认值：rate=10 / burst=30。
			if tc.extra == "" {
				if cfg.Relay.TokenRatePerIP != 10 || cfg.Relay.TokenBurstPerIP != 30 {
					t.Fatalf("token 速率限制默认值 = %d/%d，期望 10/30",
						cfg.Relay.TokenRatePerIP, cfg.Relay.TokenBurstPerIP)
				}
			}
			if tc.wantWrn == nil {
				if hasWarningContaining(cfg, "udp_dtls = false") {
					t.Fatalf("udp_dtls 开启（或未配置 udp_listen）时不应出现数据面明文 WARN: %v", cfg.Warnings)
				}
				return
			}
			for _, want := range tc.wantWrn {
				if !hasWarningContaining(cfg, want) {
					t.Fatalf("缺少包含 %q 的 WARN，实际 %v", want, cfg.Warnings)
				}
			}
		})
	}
}

// TestValidateUDPDTLSKnownKeys 三个新键不算未知键（不产生 unknown 警告）。
func TestValidateUDPDTLSKnownKeys(t *testing.T) {
	cfg, err := Load(writeTempINI(t, "[relay]\ntunnel_listen = 127.0.0.1:17000\ntcp_listen = 127.0.0.1:17001\n"+
		"auth_token = t\nudp_dtls = true\ntoken_rate_per_ip = 3\ntoken_burst_per_ip = 6\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.Validate(ModeRelay); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	for _, w := range cfg.Warnings {
		if strings.Contains(w, "未知") {
			t.Fatalf("新键被当成未知键: %s", w)
		}
	}
}
