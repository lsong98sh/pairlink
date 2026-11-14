package config

import (
	"strings"
	"testing"
)

// tlsMinimalINI 是覆盖三种模式的最小合法 ini（用于默认值断言）。
const tlsMinimalINI = `[relay]
tunnel_listen = 0.0.0.0:7000
tcp_listen = 0.0.0.0:7001

[server]
relay_addr = relay.example.com:7000
token = t
local = 127.0.0.1:8080

[client]
relay_addr = relay.example.com:7000
token = t
service.a = 127.0.0.1:8080
`

// TestTLSModeDefaultAuto 断言三个段未配置 tls 时默认 auto。
func TestTLSModeDefaultAuto(t *testing.T) {
	cfg, err := Load(writeTempINI(t, tlsMinimalINI))
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	for name, got := range map[string]TLSMode{
		"relay":  cfg.Relay.TLS,
		"server": cfg.Server.TLS,
		"client": cfg.Client.TLS,
	} {
		if got != TLSAuto {
			t.Errorf("%s.TLS = %q, want auto", name, got)
		}
	}
}

// TestParseTLSMode 覆盖大小写/空白归一化与非法值。
func TestParseTLSMode(t *testing.T) {
	valid := map[string]TLSMode{
		"auto":   TLSAuto,
		"AUTO":   TLSAuto,
		" Auto ": TLSAuto,
		"true":   TLSTrue,
		"TRUE":   TLSTrue,
		" True":  TLSTrue,
		"false":  TLSFalse,
		"FALSE":  TLSFalse,
		"":       TLSAuto, // 空串按默认 auto
	}
	for in, want := range valid {
		got, err := ParseTLSMode(in)
		if err != nil {
			t.Errorf("ParseTLSMode(%q) 失败: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseTLSMode(%q) = %q, want %q", in, got, want)
		}
	}

	for _, in := range []string{"on", "yes", "1", "0", "off", "required", "tls"} {
		if _, err := ParseTLSMode(in); err == nil {
			t.Errorf("ParseTLSMode(%q) 应报错（只允许 auto/true/false）", in)
		}
	}
}

// TestLoadTLSModeExplicitValues 覆盖 ini 中三段各自的显式取值。
func TestLoadTLSModeExplicitValues(t *testing.T) {
	path := writeTempINI(t, `[relay]
tunnel_listen = 0.0.0.0:7000
tcp_listen = 0.0.0.0:7001
tls = AUTO

[server]
relay_addr = relay.example.com:7000
token = t
local = 127.0.0.1:8080
tls = True

[client]
relay_addr = relay.example.com:7000
token = t
service.a = 127.0.0.1:8080
tls = FALSE
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if cfg.Relay.TLS != TLSAuto {
		t.Errorf("relay.TLS = %q, want auto", cfg.Relay.TLS)
	}
	if cfg.Server.TLS != TLSTrue {
		t.Errorf("server.TLS = %q, want true", cfg.Server.TLS)
	}
	if cfg.Client.TLS != TLSFalse {
		t.Errorf("client.TLS = %q, want false", cfg.Client.TLS)
	}
}

// TestLoadInvalidTLSValue 断言非法 tls 值在 Load 阶段直接报错（三段都覆盖）。
func TestLoadInvalidTLSValue(t *testing.T) {
	cases := []struct {
		name, content string
	}{
		{"relay", "[relay]\ntunnel_listen = 0.0.0.0:7000\ntcp_listen = 0.0.0.0:7001\ntls = on\n"},
		{"server", "[server]\nrelay_addr = relay.example.com:7000\ntoken = t\nlocal = 127.0.0.1:8080\ntls = yes\n"},
		{"client", "[client]\nrelay_addr = relay.example.com:7000\ntoken = t\nservice.a = 127.0.0.1:8080\ntls = 1\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeTempINI(t, tc.content))
			if err == nil {
				t.Fatal("非法 tls 值应报错")
			}
			if !strings.Contains(err.Error(), "tls") {
				t.Fatalf("错误信息应提到 tls，实际 %q", err.Error())
			}
		})
	}
}

// TestValidateInvalidTLSModeDirect 覆盖绕过 ini、直接构造 Config 时的非法值。
func TestValidateInvalidTLSModeDirect(t *testing.T) {
	cfg := &Config{
		Common: CommonConfig{LogLevel: "info", LogFormat: "text"},
		Relay:  RelayConfig{TunnelListen: "127.0.0.1:7000", TCPListen: "127.0.0.1:7001"},
		Server: ServerConfig{
			RelayAddr: "relay.example.com:7000", Token: "t",
			Protocol: "tcp", Local: "127.0.0.1:8080", TLS: TLSMode("on"),
		},
		Client: ClientConfig{
			RelayAddr: "relay.example.com:7000", Token: "t",
			Services: []ClientService{{ID: "a", Listen: "127.0.0.1:8080"}}, TLS: TLSMode("on"),
		},
	}
	if err := cfg.Validate(ModeAgent); err == nil || !strings.Contains(err.Error(), "tls") {
		t.Fatalf("直接构造的非法 [server] tls 应报错，实际 %v", err)
	}
	if err := cfg.Validate(ModeClient); err == nil || !strings.Contains(err.Error(), "tls") {
		t.Fatalf("直接构造的非法 [client] tls 应报错，实际 %v", err)
	}
	cfg.Relay.TLS = TLSMode("yes")
	if err := cfg.Validate(ModeRelay); err == nil || !strings.Contains(err.Error(), "tls") {
		t.Fatalf("直接构造的非法 [relay] tls 应报错，实际 %v", err)
	}
}

// TestValidateRelayTLSRequired 断言 tls=true 时证书/私钥必须成对非空。
func TestValidateRelayTLSRequired(t *testing.T) {
	cases := []struct {
		name, extra string
	}{
		{"无证书无私钥", ""},
		{"只有证书", "tls_cert = /etc/pairlink/tls/relay.crt\n"},
		{"只有私钥", "tls_key = /etc/pairlink/tls/relay.key\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content := "[relay]\ntunnel_listen = 0.0.0.0:7000\ntcp_listen = 0.0.0.0:7001\ntls = true\n" + tc.extra
			cfg, err := Load(writeTempINI(t, content))
			if err != nil {
				t.Fatalf("Load 失败: %v", err)
			}
			err = cfg.Validate(ModeRelay)
			if err == nil {
				t.Fatal("tls=true 缺少 tls_cert/tls_key 应校验失败")
			}
			for _, want := range []string{"tls = true", "tls_cert", "tls_key"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("错误信息 %q 应包含 %q", err.Error(), want)
				}
			}
		})
	}
}

// TestValidateRelayTLSFalseWithCert 断言 tls=false + 证书配置通过校验且 WARN。
func TestValidateRelayTLSFalseWithCert(t *testing.T) {
	cfg, err := Load(writeTempINI(t, `[relay]
tunnel_listen = 0.0.0.0:7000
tcp_listen = 0.0.0.0:7001
tls = false
tls_cert = /nonexistent/relay.crt
tls_key = /nonexistent/relay.key
`))
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if err := cfg.Validate(ModeRelay); err != nil {
		t.Fatalf("tls=false + 证书应通过校验: %v", err)
	}
	if !hasWarning(cfg.Warnings, "tls = false") {
		t.Errorf("应提示 tls=false 证书被忽略: %v", cfg.Warnings)
	}
	if !hasWarning(cfg.Warnings, "明文") {
		t.Errorf("应提示按明文监听: %v", cfg.Warnings)
	}
	// tls=false 时证书文件被忽略，不应再提示文件不可访问。
	if hasWarning(cfg.Warnings, "不可访问") {
		t.Errorf("tls=false 时不应检查证书文件是否存在: %v", cfg.Warnings)
	}
}

// TestValidateDialTLSOnWithoutCA 断言 [server]/[client] tls=true 无 ca 也通过。
func TestValidateDialTLSOnWithoutCA(t *testing.T) {
	cfg, err := Load(writeTempINI(t, `[server]
relay_addr = relay.example.com:7000
token = t
local = 127.0.0.1:8080
tls = true

[client]
relay_addr = relay.example.com:7000
token = t
service.a = 127.0.0.1:8080
tls = true
`))
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if err := cfg.Validate(ModeAgent); err != nil {
		t.Fatalf("[server] tls=true 无 tls_ca 应通过（使用系统根证书）: %v", err)
	}
	if err := cfg.Validate(ModeClient); err != nil {
		t.Fatalf("[client] tls=true 无 tls_ca 应通过（使用系统根证书）: %v", err)
	}
	if hasWarning(cfg.Warnings, "tls_insecure_skip_verify") {
		t.Errorf("未配置 insecure 不应告警: %v", cfg.Warnings)
	}
}

// TestValidateDialTLSFalseWithMaterials 断言 tls=false 时 CA/insecure 被忽略并 WARN。
func TestValidateDialTLSFalseWithMaterials(t *testing.T) {
	cfg, err := Load(writeTempINI(t, `[server]
relay_addr = relay.example.com:7000
token = t
local = 127.0.0.1:8080
tls = false
tls_ca = /nonexistent/ca.pem
tls_insecure_skip_verify = true

[client]
relay_addr = relay.example.com:7000
token = t
service.a = 127.0.0.1:8080
tls = false
tls_ca = /nonexistent/ca.pem
tls_insecure_skip_verify = true
`))
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if err := cfg.Validate(ModeAgent); err != nil {
		t.Fatalf("[server] tls=false 应通过校验: %v", err)
	}
	if err := cfg.Validate(ModeClient); err != nil {
		t.Fatalf("[client] tls=false 应通过校验: %v", err)
	}
	for _, want := range []string{"[server]", "[client]"} {
		var found bool
		for _, w := range cfg.Warnings {
			if strings.HasPrefix(w, want) && strings.Contains(w, "tls = false") && strings.Contains(w, "被忽略") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("缺少 %s tls=false 忽略材料的警告: %v", want, cfg.Warnings)
		}
	}
}

// TestValidateDialTLSOnWithInsecureWarns 断言 tls=true + insecure 允许但 WARN。
func TestValidateDialTLSOnWithInsecureWarns(t *testing.T) {
	cfg, err := Load(writeTempINI(t, `[server]
relay_addr = relay.example.com:7000
token = t
local = 127.0.0.1:8080
tls = true
tls_insecure_skip_verify = true

[client]
relay_addr = relay.example.com:7000
token = t
service.a = 127.0.0.1:8080
tls = true
tls_insecure_skip_verify = true
`))
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if err := cfg.Validate(ModeAgent); err != nil {
		t.Fatalf("[server] tls=true + insecure 应通过校验: %v", err)
	}
	if err := cfg.Validate(ModeClient); err != nil {
		t.Fatalf("[client] tls=true + insecure 应通过校验: %v", err)
	}
	for _, want := range []string{"[server]", "[client]"} {
		var found bool
		for _, w := range cfg.Warnings {
			if strings.HasPrefix(w, want) && strings.Contains(w, "tls = true") && strings.Contains(w, "多余") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("缺少 %s tls=true 时 insecure 多余的警告: %v", want, cfg.Warnings)
		}
	}
}

// TestTLSModeDecisionTable 固化 DialTLS / ListenTLS 的决策表。
func TestTLSModeDecisionTable(t *testing.T) {
	dial := []struct {
		mode     TLSMode
		ca       string
		insecure bool
		want     bool
	}{
		{TLSAuto, "", false, false},
		{TLSAuto, "ca.pem", false, true},
		{TLSAuto, "", true, true},
		{TLSTrue, "", false, true},
		{TLSTrue, "ca.pem", false, true},
		{TLSTrue, "", true, true},
		{TLSFalse, "", false, false},
		{TLSFalse, "ca.pem", true, false},
		{"", "ca.pem", false, true}, // 零值按 auto
	}
	for _, tc := range dial {
		if got := tc.mode.DialTLS(tc.ca, tc.insecure); got != tc.want {
			t.Errorf("%q.DialTLS(ca=%q, insecure=%v) = %v, want %v", tc.mode, tc.ca, tc.insecure, got, tc.want)
		}
	}

	listen := []struct {
		mode      TLSMode
		cert, key string
		want      bool
	}{
		{TLSAuto, "", "", false},
		{TLSAuto, "cert.pem", "", false},
		{TLSAuto, "cert.pem", "key.pem", true},
		{TLSTrue, "cert.pem", "key.pem", true},
		{TLSTrue, "", "", false}, // Validate 会报错；这里只表达材料不足
		{TLSFalse, "cert.pem", "key.pem", false},
		{"", "cert.pem", "key.pem", true},
	}
	for _, tc := range listen {
		if got := tc.mode.ListenTLS(tc.cert, tc.key); got != tc.want {
			t.Errorf("%q.ListenTLS(cert=%q, key=%q) = %v, want %v", tc.mode, tc.cert, tc.key, got, tc.want)
		}
	}
}
