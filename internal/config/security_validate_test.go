package config

import (
	"strings"
	"testing"
)

// TestValidateRelayNonLoopbackSecurity 覆盖 SEC-A1 / SEC-T2 的启动校验矩阵：
//
//	                    | 空 auth_token        | 配了 auth_token
//	回环 + auto 无证书  | 通过（仅本机调试）    | 通过
//	非回环 + auto 无证书 | 失败（auth + tls）  | 失败（tls，必须显式 tls=false）
//	非回环 + tls=false   | 失败（auth）         | 通过（显式承担明文风险）
//	非回环 + tls=true    | 失败（auth）         | 通过
func TestValidateRelayNonLoopbackSecurity(t *testing.T) {
	cases := []struct {
		name     string
		ini      string
		wantErr  []string // 非空表示必须失败，且错误信息包含全部子串
		wantWarn []string // 通过时警告中必须出现的子串
	}{
		{
			name: "回环_auto_无证书_空token_通过",
			ini: `[relay]
tunnel_listen = 127.0.0.1:7000
tcp_listen = 127.0.0.1:7001
tls = auto
`,
			wantWarn: []string{"明文", "auth_token 为空", "仅允许回环"},
		},
		{
			name: "localhost_auto_无证书_通过",
			ini: `[relay]
tunnel_listen = localhost:7000
tcp_listen = localhost:7001
`,
			wantWarn: []string{"明文"},
		},
		{
			name: "非回环_auto_无证书_空token_失败",
			ini: `[relay]
tunnel_listen = 0.0.0.0:7000
tcp_listen = 0.0.0.0:7001
tls = auto
`,
			wantErr: []string{"公网监听必须配置证书", "tls = false"},
		},
		{
			name: "非回环_auto_无证书_有token_失败",
			ini: `[relay]
tunnel_listen = 0.0.0.0:7000
tcp_listen = 0.0.0.0:7001
tls = auto
auth_token = s3cret
`,
			wantErr: []string{"公网监听必须配置证书", "tls = false"},
		},
		{
			name: "非回环_未写tls_默认auto_失败",
			ini: `[relay]
tunnel_listen = 192.0.2.10:7000
tcp_listen = 0.0.0.0:7001
auth_token = s3cret
`,
			wantErr: []string{"公网监听必须配置证书"},
		},
		{
			name: "非回环_auto_只有证书_失败",
			ini: `[relay]
tunnel_listen = 0.0.0.0:7000
tcp_listen = 0.0.0.0:7001
tls = auto
tls_cert = certs/cert.pem
auth_token = s3cret
`,
			wantErr: []string{"未成对配置"},
		},
		{
			name: "非回环_tls_false_空token_失败",
			ini: `[relay]
tunnel_listen = 0.0.0.0:7000
tcp_listen = 0.0.0.0:7001
tls = false
`,
			wantErr: []string{"auth_token 为空"},
		},
		{
			name: "非回环_tls_false_有token_通过",
			ini: `[relay]
tunnel_listen = 0.0.0.0:7000
tcp_listen = 0.0.0.0:7001
tls = false
auth_token = s3cret
`,
			wantWarn: []string{"明文"},
		},
		{
			name: "IPv6任意地址_tls_false_空token_失败",
			ini: `[relay]
tunnel_listen = [::]:7000
tcp_listen = [::1]:7001
tls = false
`,
			wantErr: []string{"auth_token 为空", "非回环"},
		},
		{
			name: "非回环_tls_true_有证书_空token_失败",
			ini: `[relay]
tunnel_listen = 0.0.0.0:7000
tcp_listen = 0.0.0.0:7001
tls = true
tls_cert = certs/cert.pem
tls_key = certs/key.pem
`,
			wantErr: []string{"auth_token 为空"},
		},
		{
			name: "非回环_tls_true_有证书_有token_通过",
			ini: `[relay]
tunnel_listen = 0.0.0.0:7000
tcp_listen = 0.0.0.0:7001
tls = true
tls_cert = certs/cert.pem
tls_key = certs/key.pem
auth_token = s3cret
`,
			// 证书文件不存在只警告，不报错（允许先写配置后拷证书）。
			wantWarn: []string{"tls_cert"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeTempINI(t, tc.ini))
			if err != nil {
				t.Fatalf("Load 失败: %v", err)
			}
			err = cfg.Validate(ModeRelay)
			if len(tc.wantErr) > 0 {
				if err == nil {
					t.Fatalf("应校验失败，实际通过；warnings=%v", cfg.Warnings)
				}
				for _, sub := range tc.wantErr {
					if !strings.Contains(err.Error(), sub) {
						t.Errorf("错误信息缺少 %q: %v", sub, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("应校验通过，实际失败: %v", err)
			}
			for _, sub := range tc.wantWarn {
				if !hasWarning(cfg.Warnings, sub) {
					t.Errorf("警告缺少 %q: %v", sub, cfg.Warnings)
				}
			}
		})
	}
}

// TestLoadRelayLimits 覆盖 SEC-D2 新增的 [relay] 连接/待配对上限解析。
func TestLoadRelayLimits(t *testing.T) {
	cfg, err := Load(writeTempINI(t, `[relay]
tunnel_listen = 127.0.0.1:7000
tcp_listen = 127.0.0.1:7001
max_conns = 64
max_conns_per_ip = 8
max_pending_pairs = 32
`))
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if cfg.Relay.MaxConns != 64 || cfg.Relay.MaxConnsPerIP != 8 || cfg.Relay.MaxPendingPairs != 32 {
		t.Fatalf("解析结果错误: max_conns=%d max_conns_per_ip=%d max_pending_pairs=%d",
			cfg.Relay.MaxConns, cfg.Relay.MaxConnsPerIP, cfg.Relay.MaxPendingPairs)
	}
	if err := cfg.Validate(ModeRelay); err != nil {
		t.Fatalf("合法上限应通过校验: %v", err)
	}

	// 0 = 不限制，必须允许。
	cfg, err = Load(writeTempINI(t, `[relay]
tunnel_listen = 127.0.0.1:7000
tcp_listen = 127.0.0.1:7001
max_conns = 0
max_conns_per_ip = 0
max_pending_pairs = 0
`))
	if err != nil {
		t.Fatalf("0 应被接受（表示不限制）: %v", err)
	}

	// 负数、非整数必须拒绝。
	for _, bad := range []string{"-1", "abc", "1.5"} {
		if _, err := Load(writeTempINI(t, `[relay]
tunnel_listen = 127.0.0.1:7000
tcp_listen = 127.0.0.1:7001
max_conns = `+bad+`
`)); err == nil {
			t.Errorf("max_conns = %q 应解析失败", bad)
		}
	}
}

// TestValidateRelayLimitsNegative 兜住「绕过 ini 直接构造 Config」的负值上限。
func TestValidateRelayLimitsNegative(t *testing.T) {
	cfg, err := Load(writeTempINI(t, `[relay]
tunnel_listen = 127.0.0.1:7000
tcp_listen = 127.0.0.1:7001
`))
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	cfg.Relay.MaxConns = -1
	err = cfg.Validate(ModeRelay)
	if err == nil || !strings.Contains(err.Error(), "max_conns") {
		t.Fatalf("负值上限应校验失败并提示字段名: %v", err)
	}
}
