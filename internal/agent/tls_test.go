package agent

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
)

// writeAgentTestCA 生成一个自签 CA 文件（仅用于确认 RootCAs 被填充）。
func writeAgentTestCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成 CA 私钥: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "PairLink agent test CA"},
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
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("创建 CA 文件: %v", err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("写 CA PEM: %v", err)
	}
	return path
}

// newBufferLogger 返回写内存缓冲的 logger，用于断言 WARN。
func newBufferLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// TestServerTLSConfigAutoPlainWithoutCA 覆盖默认行为：auto + 无材料 → 明文。
func TestServerTLSConfigAutoPlainWithoutCA(t *testing.T) {
	buf := &bytes.Buffer{}
	tc, err := serverTLSConfig(&config.ServerConfig{TLS: config.TLSAuto}, "relay.example.com", newBufferLogger(buf))
	if err != nil {
		t.Fatalf("serverTLSConfig: %v", err)
	}
	if tc != nil {
		t.Fatalf("auto + 无 tls_ca 应返回 nil（明文），实际 %+v", tc)
	}
	if buf.Len() != 0 {
		t.Errorf("auto + 无材料不应产生 WARN，实际 %q", buf.String())
	}
}

// TestServerTLSConfigAutoWithCA 覆盖 auto + ca → TLS + 自定义 RootCAs。
func TestServerTLSConfigAutoWithCA(t *testing.T) {
	ca := writeAgentTestCA(t)
	tc, err := serverTLSConfig(&config.ServerConfig{TLS: config.TLSAuto, TLSCA: ca}, "relay.example.com", nil)
	if err != nil {
		t.Fatalf("serverTLSConfig: %v", err)
	}
	if tc == nil {
		t.Fatal("auto + tls_ca 应启用 TLS")
	}
	if tc.RootCAs == nil {
		t.Error("tls_ca 非空时 RootCAs 不应为 nil（只信任该 CA）")
	}
	if tc.InsecureSkipVerify {
		t.Error("未配置 insecure 时不应跳过校验")
	}
	if tc.ServerName != "relay.example.com" {
		t.Errorf("ServerName = %q, want relay.example.com", tc.ServerName)
	}
}

// TestServerTLSConfigTrueWithoutCAUsesSystemRoots 覆盖 tls=true + 无 ca：
// RootCAs 必须为 nil（系统根证书），且不得自动关闭校验。
func TestServerTLSConfigTrueWithoutCAUsesSystemRoots(t *testing.T) {
	tc, err := serverTLSConfig(&config.ServerConfig{TLS: config.TLSTrue}, "relay.example.com", nil)
	if err != nil {
		t.Fatalf("serverTLSConfig: %v", err)
	}
	if tc == nil {
		t.Fatal("tls=true 必须启用 TLS")
	}
	if tc.RootCAs != nil {
		t.Error("tls=true 且无 tls_ca 时 RootCAs 应为 nil（使用系统根证书）")
	}
	if tc.InsecureSkipVerify {
		t.Error("tls=true 无 ca 时不应跳过证书校验")
	}
	if tc.MinVersion != 0 && tc.MinVersion < 0x0303 {
		t.Errorf("MinVersion = %#x，应至少 TLS 1.2", tc.MinVersion)
	}
}

// TestServerTLSConfigTrueInsecureWarns 覆盖 tls=true + insecure：跳过校验并 WARN。
func TestServerTLSConfigTrueInsecureWarns(t *testing.T) {
	buf := &bytes.Buffer{}
	tc, err := serverTLSConfig(&config.ServerConfig{
		TLS:                   config.TLSTrue,
		TLSInsecureSkipVerify: true,
	}, "relay.example.com", newBufferLogger(buf))
	if err != nil {
		t.Fatalf("serverTLSConfig: %v", err)
	}
	if tc == nil || !tc.InsecureSkipVerify {
		t.Fatalf("tls=true + insecure 应启用 TLS 且跳过校验，实际 %+v", tc)
	}
	if !strings.Contains(buf.String(), "跳过") {
		t.Errorf("应输出跳过校验的 WARN，实际 %q", buf.String())
	}
	if !strings.Contains(buf.String(), "多余") {
		t.Errorf("tls=true 时 WARN 应说明该开关多余，实际 %q", buf.String())
	}
}

// TestServerTLSConfigFalseWithCAIgnored 覆盖 tls=false + ca：明文 + WARN。
func TestServerTLSConfigFalseWithCAIgnored(t *testing.T) {
	ca := writeAgentTestCA(t)
	buf := &bytes.Buffer{}
	tc, err := serverTLSConfig(&config.ServerConfig{
		TLS:   config.TLSFalse,
		TLSCA: ca,
	}, "relay.example.com", newBufferLogger(buf))
	if err != nil {
		t.Fatalf("serverTLSConfig: %v", err)
	}
	if tc != nil {
		t.Fatalf("tls=false 必须明文，实际 %+v", tc)
	}
	if !strings.Contains(buf.String(), "tls = false") || !strings.Contains(buf.String(), "被忽略") {
		t.Errorf("应输出 tls=false 忽略 tls_ca 的 WARN，实际 %q", buf.String())
	}
}

// TestServerTLSConfigAutoInsecure 覆盖旧行为：auto + 仅 insecure → TLS 跳过校验。
func TestServerTLSConfigAutoInsecure(t *testing.T) {
	buf := &bytes.Buffer{}
	tc, err := serverTLSConfig(&config.ServerConfig{
		TLS:                   config.TLSAuto,
		TLSInsecureSkipVerify: true,
	}, "relay.example.com", newBufferLogger(buf))
	if err != nil {
		t.Fatalf("serverTLSConfig: %v", err)
	}
	if tc == nil || !tc.InsecureSkipVerify {
		t.Fatalf("auto + insecure 应启用 TLS 且跳过校验，实际 %+v", tc)
	}
	if !strings.Contains(buf.String(), "跳过") {
		t.Errorf("应输出跳过校验的 WARN，实际 %q", buf.String())
	}
}

// TestServerTLSConfigBadCAMissing 覆盖 ca 文件缺失时的可读错误。
func TestServerTLSConfigBadCAMissing(t *testing.T) {
	_, err := serverTLSConfig(&config.ServerConfig{
		TLS:   config.TLSTrue,
		TLSCA: filepath.Join(t.TempDir(), "missing.pem"),
	}, "relay.example.com", nil)
	if err == nil {
		t.Fatal("读取不存在的 tls_ca 应报错")
	}
	if !strings.Contains(err.Error(), "tls_ca") {
		t.Errorf("错误信息应提到 tls_ca，实际 %q", err.Error())
	}
}

// 确保测试用的自签 CA 能被 x509 解析（避免 helper 本身写出坏文件）。
func TestAgentTestCAIsParseable(t *testing.T) {
	ca := writeAgentTestCA(t)
	pemBytes, err := os.ReadFile(ca)
	if err != nil {
		t.Fatalf("读取 CA: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		t.Fatal("测试 CA 不是合法 PEM")
	}
}
