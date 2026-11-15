package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTempCA 生成一个自签 CA 文件（仅用于确认 RootCAs 被填充）。
func writeTempCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成 CA 私钥: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "PairLink config test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("生成 CA 证书: %v", err)
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("创建 CA 文件: %v", err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("写 CA PEM: %v", err)
	}
	return path
}

// hasWarningContaining 报告 cfg.Warnings 中是否存在包含全部子串的一条。
func hasWarningContaining(cfg *Config, subs ...string) bool {
	for _, w := range cfg.Warnings {
		ok := true
		for _, sub := range subs {
			if !strings.Contains(w, sub) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// TestValidateRelayDataTLSMatrix 覆盖 [relay] data_tls × tls × 证书材料 的组合表。
//
// 断言两件事：是否通过校验；通过时数据面最终是否 TLS（auto 跟随隧道判定）。
func TestValidateRelayDataTLSMatrix(t *testing.T) {
	cases := []struct {
		name       string
		tls        string // 写入 ini 的 tls 原文（空 = 不写，默认 auto）
		cert       bool   // 是否写 tls_cert/tls_key
		dataTLS    string // 写入 ini 的 data_tls 原文（空 = 不写，默认 auto）
		wantErr    string // 期望错误包含的子串；空表示通过
		wantTunnel bool
		wantData   bool
		wantWarn   []string
	}{
		{name: "auto无证书_data_auto", wantData: false, wantWarn: nil},
		{name: "auto无证书_data_true", dataTLS: "true", wantErr: "data_tls = true 但缺少 tls_cert / tls_key"},
		{name: "auto有证书_data_auto", cert: true, wantTunnel: true, wantData: true},
		{name: "auto有证书_data_true", cert: true, dataTLS: "true", wantTunnel: true, wantData: true},
		{name: "auto有证书_data_false", cert: true, dataTLS: "false", wantTunnel: true, wantData: false, wantWarn: []string{"data_tls = false"}},
		{name: "true有证书_data_auto", tls: "true", cert: true, wantTunnel: true, wantData: true},
		{name: "true有证书_data_false", tls: "true", cert: true, dataTLS: "false", wantTunnel: true, wantData: false, wantWarn: []string{"data_tls = false"}},
		{name: "true无证书", tls: "true", wantErr: "tls = true 但缺少 tls_cert / tls_key"},
		{name: "false有证书_data_true", tls: "false", cert: true, dataTLS: "true", wantTunnel: false, wantData: true, wantWarn: []string{"data_tls = true", "隧道"}},
		{name: "false无证书_data_true", tls: "false", dataTLS: "true", wantErr: "data_tls = true 但缺少"},
		{name: "false无证书_data_auto", tls: "false", wantTunnel: false, wantData: false},
		{name: "auto证书不成对_data_true", dataTLS: "true", wantErr: "data_tls = true 但缺少"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var b strings.Builder
			b.WriteString("[relay]\n")
			b.WriteString("tunnel_listen = 127.0.0.1:17000\n")
			b.WriteString("tcp_listen = 127.0.0.1:17001\n")
			b.WriteString("auth_token = test-token\n")
			if tc.tls != "" {
				b.WriteString("tls = " + tc.tls + "\n")
			}
			if tc.cert {
				b.WriteString("tls_cert = " + t.TempDir() + "/relay.crt\n")
				b.WriteString("tls_key = " + t.TempDir() + "/relay.key\n")
			} else if tc.name == "auto证书不成对_data_true" {
				b.WriteString("tls_cert = " + t.TempDir() + "/relay.crt\n")
			}
			if tc.dataTLS != "" {
				b.WriteString("data_tls = " + tc.dataTLS + "\n")
			}
			b.WriteString("service_allow = svc-a,svc-b\n")

			cfg, err := Load(writeTempINI(t, b.String()))
			if tc.wantErr != "" {
				// 部分规则在解析/校验阶段就会拒绝（Load 内部会跑 Validate）。
				if err == nil {
					err = cfg.Validate(ModeRelay)
				}
				if err == nil {
					t.Fatalf("期望校验失败（%s），实际通过", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("错误 = %q，期望包含 %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load/Validate: %v", err)
			}
			if err := cfg.Validate(ModeRelay); err != nil {
				t.Fatalf("期望校验通过，实际 %v", err)
			}
			tunnel := cfg.Relay.TLS.ListenTLS(cfg.Relay.TLSCert, cfg.Relay.TLSKey)
			data := cfg.Relay.DataTLS.RelayDataTLS(tunnel)
			if tunnel != tc.wantTunnel {
				t.Errorf("隧道 TLS = %v，期望 %v（tls=%q cert=%v）", tunnel, tc.wantTunnel, cfg.Relay.TLS, tc.cert)
			}
			if data != tc.wantData {
				t.Errorf("数据面 TLS = %v，期望 %v（tls=%q data_tls=%q）", data, tc.wantData, cfg.Relay.TLS, cfg.Relay.DataTLS)
			}
			for _, want := range tc.wantWarn {
				if !hasWarningContaining(cfg, want) {
					t.Errorf("缺少包含 %q 的 WARN，实际 %v", want, cfg.Warnings)
				}
			}
		})
	}
}

// TestValidateRelayServiceOwnership 覆盖 service_allow / max_services_per_agent。
func TestValidateRelayServiceOwnership(t *testing.T) {
	base := "[relay]\ntunnel_listen = 127.0.0.1:17000\ntcp_listen = 127.0.0.1:17001\nauth_token = t\n"

	t.Run("去重与顺序", func(t *testing.T) {
		cfg, err := Load(writeTempINI(t, base+"service_allow = svc-b, svc-a ,svc-b\n"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if err := cfg.Validate(ModeRelay); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		got := strings.Join(cfg.Relay.ServiceAllow, ",")
		if got != "svc-b,svc-a" {
			t.Fatalf("service_allow = %q，期望去重且保序 svc-b,svc-a", got)
		}
		if hasWarningContaining(cfg, "未配置 service_allow") {
			t.Errorf("已配置白名单不应出现未配置告警: %v", cfg.Warnings)
		}
	})

	t.Run("非法服务名", func(t *testing.T) {
		cfg, err := Load(writeTempINI(t, base+"service_allow = svc-a,Bad_Name\n"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		err = cfg.Validate(ModeRelay)
		if err == nil || !strings.Contains(err.Error(), "service_allow") {
			t.Fatalf("期望 service_allow 非法错误，实际 %v", err)
		}
	})

	t.Run("未配置白名单告警", func(t *testing.T) {
		cfg, err := Load(writeTempINI(t, base))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if err := cfg.Validate(ModeRelay); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if !hasWarningContaining(cfg, "未配置 service_allow", "任何持有 auth_token") {
			t.Errorf("空白名单必须 WARN 风险，实际 %v", cfg.Warnings)
		}
	})

	t.Run("默认上限64", func(t *testing.T) {
		cfg, err := Load(writeTempINI(t, base+"service_allow = svc-a\n"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Relay.MaxServicesPerAgent != 64 {
			t.Fatalf("max_services_per_agent 默认 = %d，期望 64", cfg.Relay.MaxServicesPerAgent)
		}
	})

	t.Run("负数上限", func(t *testing.T) {
		// 负数由 integer 解析器拒绝（Load 即失败）；Validate 的同类检查覆盖直构场景。
		_, err := Load(writeTempINI(t, base+"service_allow = svc-a\nmax_services_per_agent = -1\n"))
		if err == nil || !strings.Contains(err.Error(), "max_services_per_agent") {
			t.Fatalf("负数上限应校验失败，实际 %v", err)
		}
	})

	t.Run("零表示不限", func(t *testing.T) {
		cfg, err := Load(writeTempINI(t, base+"service_allow = svc-a\nmax_services_per_agent = 0\n"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if err := cfg.Validate(ModeRelay); err != nil {
			t.Fatalf("0 应通过（不限制）: %v", err)
		}
	})
}

// TestValidateDialDataTLSWarnings 覆盖 [server]/[client] data_tls 的校验与告警。
func TestValidateDialDataTLSWarnings(t *testing.T) {
	cfg, err := Load(writeTempINI(t, `[server]
relay_addr = relay.example.com:7000
token = t
local = 127.0.0.1:8080
data_tls = true

[client]
relay_addr = relay.example.com:7000
token = t
service.a = 127.0.0.1:8080
data_tls = false
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.Validate(ModeAgent); err != nil {
		t.Fatalf("[server] data_tls=true 无 CA 应通过（系统根证书）: %v", err)
	}
	if err := cfg.Validate(ModeClient); err != nil {
		t.Fatalf("[client] data_tls=false 应通过: %v", err)
	}
	if !hasWarningContaining(cfg, "[server]", "data_tls = true", "系统根证书") {
		t.Errorf("data_tls=true 无 CA 应提示使用系统根证书: %v", cfg.Warnings)
	}
	if !hasWarningContaining(cfg, "[client]", "data_tls = false", "明确失败") {
		t.Errorf("data_tls=false 应提示不静默降级: %v", cfg.Warnings)
	}

	bad, err := Load(writeTempINI(t, "[server]\nrelay_addr = relay.example.com:7000\ntoken = t\nlocal = 127.0.0.1:8080\ndata_tls = yes\n"))
	if err == nil {
		err = bad.Validate(ModeAgent)
	}
	if err == nil || !strings.Contains(err.Error(), "data_tls") {
		t.Fatalf("data_tls 非法值应校验失败，实际 %v", err)
	}
}

// TestNegotiateDataTLS 覆盖拨号侧 data_tls × relay 下发值 的判定表。
func TestNegotiateDataTLS(t *testing.T) {
	boolPtr := func(v bool) *bool { return &v }
	cases := []struct {
		name        string
		mode        TLSMode
		advertised  *bool
		wantTLS     bool
		wantWarning bool
		wantErr     string
	}{
		{name: "auto_nil", mode: TLSAuto, advertised: nil, wantWarning: true},
		{name: "auto_true", mode: TLSAuto, advertised: boolPtr(true), wantTLS: true},
		{name: "auto_false", mode: TLSAuto, advertised: boolPtr(false)},
		{name: "zero值视为auto_nil", mode: TLSMode(""), advertised: nil, wantWarning: true},
		{name: "true_nil", mode: TLSTrue, advertised: nil, wantErr: "旧 relay"},
		{name: "true_false", mode: TLSTrue, advertised: boolPtr(false), wantErr: "未启用数据面 TLS"},
		{name: "true_true", mode: TLSTrue, advertised: boolPtr(true), wantTLS: true},
		{name: "false_nil", mode: TLSFalse, advertised: nil},
		{name: "false_true", mode: TLSFalse, advertised: boolPtr(true), wantErr: "data_tls = false"},
		{name: "false_false", mode: TLSFalse, advertised: boolPtr(false)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NegotiateDataTLS(tc.mode, tc.advertised)
			if tc.wantErr != "" {
				if got.Err == nil {
					t.Fatalf("期望错误（%s），实际 %+v", tc.wantErr, got)
				}
				if !strings.Contains(got.Err.Error(), tc.wantErr) {
					t.Fatalf("错误 = %q，期望包含 %q", got.Err.Error(), tc.wantErr)
				}
				return
			}
			if got.Err != nil {
				t.Fatalf("不应报错: %v", got.Err)
			}
			if got.UseTLS != tc.wantTLS {
				t.Errorf("UseTLS = %v，期望 %v", got.UseTLS, tc.wantTLS)
			}
			if (got.Warning != "") != tc.wantWarning {
				t.Errorf("Warning = %q，期望非空=%v", got.Warning, tc.wantWarning)
			}
		})
	}
}

// TestClientTLSConfigShared 覆盖隧道与数据面共用的 TLS 配置构造器。
func TestClientTLSConfigShared(t *testing.T) {
	ca := writeTempCA(t)
	cfg, err := ClientTLSConfig(ca, false, "relay.example.com")
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	if cfg.RootCAs == nil {
		t.Error("tls_ca 非空时 RootCAs 不应为 nil")
	}
	if cfg.ServerName != "relay.example.com" {
		t.Errorf("ServerName = %q", cfg.ServerName)
	}
	if cfg.InsecureSkipVerify {
		t.Error("insecure=false 时不应跳过校验")
	}
	if cfg.MinVersion != 0x0303 {
		t.Errorf("MinVersion = %#x，期望 TLS 1.2", cfg.MinVersion)
	}

	sys, err := ClientTLSConfig("", false, "relay.example.com")
	if err != nil {
		t.Fatalf("ClientTLSConfig(空 CA): %v", err)
	}
	if sys.RootCAs != nil {
		t.Error("空 tls_ca 时 RootCAs 应为 nil（系统根证书）")
	}

	insecure, err := ClientTLSConfig("", true, "relay.example.com")
	if err != nil {
		t.Fatalf("ClientTLSConfig(insecure): %v", err)
	}
	if !insecure.InsecureSkipVerify {
		t.Error("insecure=true 应跳过校验")
	}

	bad, err := ClientTLSConfig(t.TempDir()+"/missing.pem", false, "x")
	if err == nil || !strings.Contains(err.Error(), "tls_ca") {
		t.Fatalf("缺失 tls_ca 应报含 tls_ca 的错误，实际 %v (%v)", bad, err)
	}
}
