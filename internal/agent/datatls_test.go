package agent

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
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// writeDataTLSCert 生成 127.0.0.1 的自签证书（数据面 TLS 单测用）。
func writeDataTLSCert(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成私钥: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "PairLink data TLS test"},
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
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
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

// echoOnce 在 ln 上接受一个连接并回显一次（测试辅助）。
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

// newDataTLSConfig 返回校验 127.0.0.1 自签证书的客户端配置。
func newDataTLSConfig(t *testing.T, certPath string) *tls.Config {
	t.Helper()
	tc, err := config.ClientTLSConfig(certPath, false, "127.0.0.1")
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	return tc
}

// TestDialDataPlaneTLS 覆盖 data_tls=true 时数据面拨号完成 TLS 握手并可收发。
func TestDialDataPlaneTLS(t *testing.T) {
	certPath, keyPath := writeDataTLSCert(t)
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

	a := newAgent(context.Background(), &config.Config{Server: config.ServerConfig{
		DialTimeout: 3 * time.Second,
	}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer a.cancel()
	a.setDataTLS(true, newDataTLSConfig(t, certPath))

	conn, err := a.dialDataPlane(a.ctx, ln.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatalf("dialDataPlane: %v", err)
	}
	defer conn.Close()
	if _, ok := conn.(*tls.Conn); !ok {
		t.Fatalf("数据面连接类型 = %T，期望 *tls.Conn", conn)
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("回显 = %q，期望 ping", buf)
	}
}

// TestDialDataPlanePlaintextWhenDisabled 覆盖 data_tls=false 时数据面是裸 TCP。
func TestDialDataPlanePlaintextWhenDisabled(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer ln.Close()
	echoOnce(t, ln)

	a := newAgent(context.Background(), &config.Config{Server: config.ServerConfig{
		DialTimeout: 3 * time.Second,
	}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer a.cancel()
	a.setDataTLS(false, nil)

	conn, err := a.dialDataPlane(a.ctx, ln.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatalf("dialDataPlane: %v", err)
	}
	defer conn.Close()
	if _, ok := conn.(*tls.Conn); ok {
		t.Fatal("data_tls=false 时不应包 TLS")
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

// TestDialDataPlaneTLSFailureReadable 覆盖"本端要求 TLS、对端是明文"时可读且永久失败。
func TestDialDataPlaneTLSFailureReadable(t *testing.T) {
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

	a := newAgent(context.Background(), &config.Config{Server: config.ServerConfig{
		DialTimeout: 2 * time.Second,
	}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer a.cancel()
	a.setDataTLS(true, &tls.Config{ServerName: "127.0.0.1", InsecureSkipVerify: true})

	_, err = a.dialDataPlane(a.ctx, ln.Addr().String(), 2*time.Second)
	if err == nil {
		t.Fatal("对端明文时 TLS 握手应失败")
	}
	if !isPermanent(err) {
		t.Errorf("数据面 TLS 配置不匹配应为永久错误（不重连）: %v", err)
	}
	if !strings.Contains(err.Error(), "数据面 TLS 握手失败") {
		t.Errorf("错误应包含可读提示，实际 %q", err.Error())
	}
}

// TestServiceRegisterErrorMapping 覆盖 relay 拒绝原因 → agent 可读提示。
func TestServiceRegisterErrorMapping(t *testing.T) {
	cases := []struct {
		detail  string
		wantSub []string
	}{
		{detail: protocol.ErrDetailServiceDenied, wantSub: []string{"service_allow", "白名单"}},
		{detail: protocol.ErrDetailServiceConflict, wantSub: []string{"已被另一个在线 agent 占用", "重复部署"}},
		{detail: protocol.ErrDetailServiceLimit, wantSub: []string{"max_services_per_agent", "上限"}},
		{detail: "", wantSub: []string{"relay 拒绝注册服务"}},
	}
	for _, tc := range cases {
		t.Run(tc.detail, func(t *testing.T) {
			frame, err := protocol.EncodeFrame(protocol.MsgError, &protocol.Error{
				Code:    protocol.ErrCodeBadRequest,
				Message: "服务注册被拒绝",
				Detail:  tc.detail,
			})
			if err != nil {
				t.Fatalf("EncodeFrame: %v", err)
			}
			_, payload, err := protocol.ParseFrame(frame)
			if err != nil {
				t.Fatalf("ParseFrame: %v", err)
			}
			err = serviceRegisterError("svc-x", payload)
			if err == nil {
				t.Fatal("应返回错误")
			}
			for _, sub := range tc.wantSub {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("错误 %q 缺少 %q", err.Error(), sub)
				}
			}
		})
	}
}

// TestDataTLSConfigUsesSharedBuilder 覆盖数据面 TLS 配置与隧道共用 CA/SNI 规则。
func TestDataTLSConfigUsesSharedBuilder(t *testing.T) {
	ca := writeAgentTestCA(t)
	s := &config.ServerConfig{DataTLS: config.TLSTrue, TLSCA: ca}
	tc, err := dataTLSConfig(s, "relay.example.com")
	if err != nil {
		t.Fatalf("dataTLSConfig: %v", err)
	}
	if tc.RootCAs == nil {
		t.Error("tls_ca 非空时 RootCAs 不应为 nil")
	}
	if tc.ServerName != "relay.example.com" {
		t.Errorf("ServerName = %q", tc.ServerName)
	}
	if tc.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %#x，期望 TLS 1.2", tc.MinVersion)
	}

	bad, err := dataTLSConfig(&config.ServerConfig{TLSCA: filepath.Join(t.TempDir(), "missing.pem")}, "x")
	if err == nil || !strings.Contains(err.Error(), "tls_ca") {
		t.Fatalf("缺失 tls_ca 应报错，实际 %v (%v)", bad, err)
	}
}

// TestHandshakeLogDataPlaneTLS 覆盖数据面状态日志（TLS / 明文原因）。
func TestHandshakeLogDataPlaneTLS(t *testing.T) {
	newBuf := func() (*bytes.Buffer, *slog.Logger) {
		buf := &bytes.Buffer{}
		return buf, slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}

	t.Run("TLS", func(t *testing.T) {
		buf, log := newBuf()
		a := newAgent(context.Background(), &config.Config{Server: config.ServerConfig{}}, log)
		defer a.cancel()
		advertised := true
		a.logDataPlaneTLS(true, &advertised)
		if !strings.Contains(buf.String(), "数据面: TLS") {
			t.Errorf("应打印数据面 TLS，实际 %q", buf.String())
		}
	})

	t.Run("显式关闭", func(t *testing.T) {
		buf, log := newBuf()
		a := newAgent(context.Background(), &config.Config{Server: config.ServerConfig{DataTLS: config.TLSFalse}}, log)
		defer a.cancel()
		advertised := false
		a.logDataPlaneTLS(false, &advertised)
		out := buf.String()
		if !strings.Contains(out, "数据面: 明文") || !strings.Contains(out, "data_tls = false") {
			t.Errorf("应说明 data_tls=false 导致明文，实际 %q", out)
		}
	})

	t.Run("旧relay", func(t *testing.T) {
		buf, log := newBuf()
		a := newAgent(context.Background(), &config.Config{Server: config.ServerConfig{DataTLS: config.TLSAuto}}, log)
		defer a.cancel()
		a.logDataPlaneTLS(false, nil)
		if !strings.Contains(buf.String(), "旧 relay") {
			t.Errorf("旧 relay 应给出明确原因，实际 %q", buf.String())
		}
	})
}

// TestDataTLSOldRelayFallsBackToPlaintext 覆盖"新 agent + 旧 relay（未下发 data_tls）"：
// auto 必须回退明文并保持可用，而不是拒绝连接。
func TestDataTLSOldRelayFallsBackToPlaintext(t *testing.T) {
	echoAddr := startTCPEcho(t)
	f := newFakeRelay(t, relayOptions{
		token:         "test-token",
		udpMode:       protocol.UDPModeTCPTunnel,
		enableTCPData: true,
	})
	cfg := newTestConfig(f.tunnelAddr(), config.ServerService{ID: "default", Proto: "tcp", Addr: echoAddr})
	run := startAgent(t, cfg)

	waitCond(t, 3*time.Second, func() bool { return len(run.a.registeredServices()) == 1 }, "agent 完成注册")
	if useTLS, tcfg := run.a.dataTLSSnapshot(); useTLS || tcfg != nil {
		t.Fatalf("旧 relay 未下发 data_tls，数据面应明文，实际 useTLS=%v", useTLS)
	}
	if err := run.stop(t); err != nil {
		t.Fatalf("停止 agent: %v", err)
	}
}

// TestDataTLSRequiresTLSAgainstOldRelay 覆盖本地 data_tls=true + 旧 relay：
// 必须返回可读的永久错误（不重连、不静默降级）。
func TestDataTLSRequiresTLSAgainstOldRelay(t *testing.T) {
	echoAddr := startTCPEcho(t)
	f := newFakeRelay(t, relayOptions{
		token:         "test-token",
		udpMode:       protocol.UDPModeTCPTunnel,
		enableTCPData: true,
	})
	cfg := newTestConfig(f.tunnelAddr(), config.ServerService{ID: "default", Proto: "tcp", Addr: echoAddr})
	cfg.Server.DataTLS = config.TLSTrue
	run := startAgent(t, cfg)

	err := run.wait(t)
	if err == nil {
		t.Fatal("本地 data_tls=true、旧 relay 未下发时应失败")
	}
	if !strings.Contains(err.Error(), "旧 relay") {
		t.Errorf("错误应说明 relay 版本过旧，实际 %q", err.Error())
	}
	if !isPermanent(err) {
		t.Errorf("配置不兼容应为永久错误（不重连）: %v", err)
	}
}
