package client

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
)

// writeClientDataTLSCert 生成 127.0.0.1 的自签证书（数据面 TLS 单测用）。
func writeClientDataTLSCert(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成私钥: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "PairLink client data TLS test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("生成证书: %v", err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("写证书: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("编码私钥: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("写私钥: %v", err)
	}
	return certPath, keyPath
}

// newDataTLSClient 构造只用于数据面拨号的 client（不启动 Run）。
func newDataTLSClient(t *testing.T, cfg *config.Config) *client {
	t.Helper()
	c := newClientWithLogger(nil, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(c.cancel)
	return c
}

// echoOnce 在 ln 上接受一个连接并回显一次。
func echoOnce(t *testing.T, ln net.Listener) {
	t.Helper()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 64)
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		_, _ = conn.Write(buf[:n])
	}()
}

// TestDialTCPDataTLS 覆盖 dataTLS=true 时数据面拨号完成 TLS 握手并可收发。
func TestDialTCPDataTLS(t *testing.T) {
	certPath, keyPath := writeClientDataTLSCert(t)
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	})
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	defer ln.Close()
	echoOnce(t, ln)

	cfg := &config.Config{Client: config.ClientConfig{DialTimeout: 3 * time.Second}}
	c := newDataTLSClient(t, cfg)
	tc, err := config.ClientTLSConfig(certPath, false, "127.0.0.1")
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	c.setNegotiated(negotiated{tcpDataAddr: ln.Addr().String(), dataTLS: true, dataTLSCfg: tc})

	conn, err := c.dialTCPData()
	if err != nil {
		t.Fatalf("dialTCPData: %v", err)
	}
	defer conn.Close()
	if _, ok := conn.(*tls.Conn); !ok {
		t.Fatalf("连接类型 = %T，期望 *tls.Conn", conn)
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("回显 = %q", buf)
	}
}

// TestDialTCPDataPlaintext 覆盖 dataTLS=false 时数据面是裸 TCP。
func TestDialTCPDataPlaintext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer ln.Close()
	echoOnce(t, ln)

	cfg := &config.Config{Client: config.ClientConfig{DialTimeout: 3 * time.Second}}
	c := newDataTLSClient(t, cfg)
	c.setNegotiated(negotiated{tcpDataAddr: ln.Addr().String()})

	conn, err := c.dialTCPData()
	if err != nil {
		t.Fatalf("dialTCPData: %v", err)
	}
	defer conn.Close()
	if _, ok := conn.(*tls.Conn); ok {
		t.Fatal("dataTLS=false 时不应包 TLS")
	}
	if _, err := conn.Write([]byte("plain")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(buf) != "plain" {
		t.Fatalf("回显 = %q", buf)
	}
}

// TestDialTCPDataTLSFailureReadable 覆盖"本端要求 TLS、对端明文"时可读且永久失败。
func TestDialTCPDataTLSFailureReadable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			defer conn.Close()
			_, _ = conn.Read(make([]byte, 16))
		}
	}()

	cfg := &config.Config{Client: config.ClientConfig{DialTimeout: 2 * time.Second}}
	c := newDataTLSClient(t, cfg)
	c.setNegotiated(negotiated{
		tcpDataAddr: ln.Addr().String(),
		dataTLS:     true,
		dataTLSCfg:  &tls.Config{ServerName: "127.0.0.1", InsecureSkipVerify: true},
	})

	_, err = c.dialTCPData()
	if err == nil {
		t.Fatal("对端明文时 TLS 握手应失败")
	}
	if !isPermanent(err) {
		t.Errorf("数据面 TLS 配置不匹配应为永久错误: %v", err)
	}
	if !strings.Contains(err.Error(), "数据面 TLS 握手失败") {
		t.Errorf("错误应包含可读提示，实际 %q", err.Error())
	}
}

// TestDialTCPDataTLSMissingConfig 覆盖内部状态异常（要求 TLS 但无配置）。
func TestDialTCPDataTLSMissingConfig(t *testing.T) {
	cfg := &config.Config{Client: config.ClientConfig{DialTimeout: time.Second}}
	c := newDataTLSClient(t, cfg)
	c.setNegotiated(negotiated{tcpDataAddr: "127.0.0.1:1", dataTLS: true})

	if _, err := c.dialTCPData(); err == nil || !strings.Contains(err.Error(), "TLS 配置缺失") {
		t.Fatalf("应返回 TLS 配置缺失错误，实际 %v", err)
	}
}

// TestConnectOnceDataTLSOldRelay 覆盖"新 client + 旧 relay（未下发 data_tls）"：
// auto 回退明文并 WARN，不中断连接。
func TestConnectOnceDataTLSOldRelay(t *testing.T) {
	r := newFakeRelay(t, fakeRelayOptions{revision: 1})
	buf := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := &config.Config{
		Common: config.CommonConfig{LogLevel: "debug", LogFormat: "text"},
		Client: config.ClientConfig{
			RelayAddr:   r.addr(),
			Token:       "t",
			DataTLS:     config.TLSAuto,
			DialTimeout: 2 * time.Second,
		},
	}
	c := newClientWithLogger(context.Background(), cfg, log)
	defer c.cancel()

	if err := c.connectOnce(); err != nil {
		t.Fatalf("旧 relay 下 auto 应成功回退明文: %v", err)
	}
	c.closeControl()
	if neg := c.negotiated(); neg.dataTLS {
		t.Fatal("旧 relay 未下发 data_tls，数据面应明文")
	}
	if !strings.Contains(buf.String(), "未下发 data_tls") {
		t.Errorf("旧 relay 应按明文回退并 WARN，实际日志:\n%s", buf.String())
	}
}

// TestConnectOnceDataTLSMismatch 覆盖本地 data_tls 与 relay 下发值冲突：
// 必须返回可读的永久错误（不重连、不静默降级）。
func TestConnectOnceDataTLSMismatch(t *testing.T) {
	t.Run("本地要求TLS_relay未启用", func(t *testing.T) {
		advertised := false
		r := newFakeRelay(t, fakeRelayOptions{revision: 1, dataTLS: &advertised})
		cfg := &config.Config{
			Client: config.ClientConfig{RelayAddr: r.addr(), Token: "t", DataTLS: config.TLSTrue, DialTimeout: 2 * time.Second},
		}
		c := newClientWithLogger(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
		defer c.cancel()
		err := c.connectOnce()
		if err == nil {
			t.Fatal("本地 data_tls=true、relay data_tls=false 应报错")
		}
		if !isPermanent(err) {
			t.Errorf("配置冲突应为永久错误（不重连）: %v", err)
		}
		if !strings.Contains(err.Error(), "data_tls = true") {
			t.Errorf("错误应说明本地 data_tls=true，实际 %q", err.Error())
		}
	})

	t.Run("relay要求TLS_本地强制明文", func(t *testing.T) {
		advertised := true
		r := newFakeRelay(t, fakeRelayOptions{revision: 1, dataTLS: &advertised})
		cfg := &config.Config{
			Client: config.ClientConfig{RelayAddr: r.addr(), Token: "t", DataTLS: config.TLSFalse, DialTimeout: 2 * time.Second},
		}
		c := newClientWithLogger(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
		defer c.cancel()
		err := c.connectOnce()
		if err == nil {
			t.Fatal("relay data_tls=true、本地 data_tls=false 应报错")
		}
		if !isPermanent(err) {
			t.Errorf("配置冲突应为永久错误: %v", err)
		}
		if !strings.Contains(err.Error(), "data_tls = false") {
			t.Errorf("错误应说明本地 data_tls=false，实际 %q", err.Error())
		}
	})
}
