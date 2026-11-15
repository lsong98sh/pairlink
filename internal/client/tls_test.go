package client

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/logx"
)

// writeClientTestCA 生成一个自签 CA 文件（仅用于确认 RootCAs 被填充）。
func writeClientTestCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成 CA 私钥: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "PairLink client test CA"},
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

// newTLSClient 构造一个只用于 TLS 决策测试的 client（不启动 Run）。
func newTLSClient(t *testing.T, relayAddr string, mutate func(*config.ClientConfig)) (*client, *lockedBuffer) {
	t.Helper()
	cfg := &config.Config{
		Client: config.ClientConfig{
			RelayAddr:   relayAddr,
			Token:       "test-token",
			DialTimeout: 2 * time.Second,
		},
	}
	if mutate != nil {
		mutate(&cfg.Client)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	logs := &lockedBuffer{}
	log := logx.New(logx.Config{Level: "debug", Format: "text", Mode: "client", Output: logs})
	return newClientWithLogger(ctx, cfg, log), logs
}

// plainListener 启动一个立即接受并关闭连接的明文 TCP 监听，返回其地址。
func plainListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	return ln.Addr().String()
}

// TestClientTLSConfigAutoPlainWithoutCA 覆盖 auto + 无材料：明文，且必须明确
// WARN 明文风险与出口（SEC-T2），但不得出现「tls = false 材料被忽略」类误导信息。
func TestClientTLSConfigAutoPlainWithoutCA(t *testing.T) {
	addr := plainListener(t)
	c, logs := newTLSClient(t, addr, func(cc *config.ClientConfig) {
		cc.TLS = config.TLSAuto
	})
	conn, isTLS, err := c.dialControl()
	if err != nil {
		t.Fatalf("dialControl: %v", err)
	}
	defer conn.Close()
	if isTLS {
		t.Fatal("auto + 无 tls_ca 应为明文连接")
	}
	out := logs.String()
	if !strings.Contains(out, "明文连接 relay") || !strings.Contains(out, "tls 设为 true") {
		t.Errorf("auto + 无材料应 WARN 明文风险并给出出口，实际:\n%s", out)
	}
	if strings.Contains(out, "跳过") || strings.Contains(out, "tls = false") {
		t.Errorf("auto + 无材料不应产生误导性 TLS WARN，实际:\n%s", out)
	}
}

// TestClientTLSConfigAutoWithCA 覆盖 auto + ca：TLS，RootCAs 只含该 CA。
func TestClientTLSConfigAutoWithCA(t *testing.T) {
	ca := writeClientTestCA(t)
	c, _ := newTLSClient(t, "relay.example.com:7000", func(cc *config.ClientConfig) {
		cc.TLS = config.TLSAuto
		cc.TLSCA = ca
	})
	tc, err := c.tlsConfig()
	if err != nil {
		t.Fatalf("tlsConfig: %v", err)
	}
	if tc == nil || tc.RootCAs == nil {
		t.Fatalf("tls_ca 非空时 RootCAs 不应为 nil: %+v", tc)
	}
	if tc.InsecureSkipVerify {
		t.Error("未配置 insecure 时不应跳过校验")
	}
	if tc.ServerName != "relay.example.com" {
		t.Errorf("ServerName = %q, want relay.example.com", tc.ServerName)
	}
}

// TestClientTLSConfigTrueWithoutCAUsesSystemRoots 覆盖 tls=true + 无 ca。
func TestClientTLSConfigTrueWithoutCAUsesSystemRoots(t *testing.T) {
	c, _ := newTLSClient(t, "relay.example.com:7000", func(cc *config.ClientConfig) {
		cc.TLS = config.TLSTrue
	})
	tc, err := c.tlsConfig()
	if err != nil {
		t.Fatalf("tlsConfig: %v", err)
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
}

// TestClientTLSConfigTrueInsecureWarns 覆盖 tls=true + insecure：跳过校验并 WARN。
func TestClientTLSConfigTrueInsecureWarns(t *testing.T) {
	addr := plainListener(t)
	c, logs := newTLSClient(t, addr, func(cc *config.ClientConfig) {
		cc.TLS = config.TLSTrue
		cc.TLSInsecureSkipVerify = true
	})
	// 对端是明文监听：握手必然失败，但 WARN 在拨号前已写出。
	if _, _, err := c.dialControl(); err == nil {
		t.Fatal("对明文监听做 TLS 握手应失败")
	}
	tc, err := c.tlsConfig()
	if err != nil {
		t.Fatalf("tlsConfig: %v", err)
	}
	if !tc.InsecureSkipVerify {
		t.Error("tls=true + insecure 应设置 InsecureSkipVerify")
	}
	if !strings.Contains(logs.String(), "跳过") {
		t.Errorf("应输出跳过校验的 WARN，实际:\n%s", logs.String())
	}
}

// TestClientTLSConfigFalseWithCAIgnored 覆盖 tls=false + ca：明文 + WARN。
func TestClientTLSConfigFalseWithCAIgnored(t *testing.T) {
	ca := writeClientTestCA(t)
	addr := plainListener(t)
	c, logs := newTLSClient(t, addr, func(cc *config.ClientConfig) {
		cc.TLS = config.TLSFalse
		cc.TLSCA = ca
	})
	conn, isTLS, err := c.dialControl()
	if err != nil {
		t.Fatalf("dialControl: %v", err)
	}
	defer conn.Close()
	if isTLS {
		t.Fatal("tls=false 必须明文，tls_ca 应被忽略")
	}
	if !strings.Contains(logs.String(), "tls = false") || !strings.Contains(logs.String(), "被忽略") {
		t.Errorf("应输出 tls=false 忽略 tls_ca 的 WARN，实际:\n%s", logs.String())
	}
}
