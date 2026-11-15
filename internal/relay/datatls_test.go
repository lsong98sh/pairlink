package relay

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// dialControlTLS 用自签证书完成 TLS 握手后返回控制面连接。
func dialControlTLS(t *testing.T, addr, certPath string) (*ctrlConn, *tls.Conn) {
	t.Helper()
	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("读取测试证书: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		t.Fatal("测试证书不是合法 PEM")
	}
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    pool,
		ServerName: "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("TLS 连接 %s 失败: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &ctrlConn{t: t, conn: conn}, conn
}

// dialDataTLS 用自签证书连 TCP 数据面（只验证监听端是否 TLS）。
func dialDataTLS(t *testing.T, addr, certPath string) (*tls.Conn, error) {
	t.Helper()
	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("读取测试证书: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		t.Fatal("测试证书不是合法 PEM")
	}
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	return tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    pool,
		ServerName: "127.0.0.1",
	})
}

// TestRelayDataTLSAdvertised 覆盖 tunnel TLS + data_tls=true：
// HelloOK 显式下发 true、数据面只接受 TLS、明文报到被拒并记录 WARN。
func TestRelayDataTLSAdvertised(t *testing.T) {
	logs := &lockedBuffer{}
	certPath, keyPath := writeTestCert(t)
	s := startRelayWithLogs(t, testConfig(t, func(c *config.Config) {
		c.Relay.TLS = config.TLSTrue
		c.Relay.TLSCert = certPath
		c.Relay.TLSKey = keyPath
		c.Relay.DataTLS = config.TLSTrue
	}), logs)

	agent, _ := dialControlTLS(t, s.tunnelAddr, certPath)
	ok := agent.hello(protocol.RoleAgent, testToken)
	if ok.DataTLS == nil || !*ok.DataTLS {
		t.Fatalf("HelloOK.data_tls = %v，期望显式 true", ok.DataTLS)
	}

	// TLS 客户端能连上数据面（握手成功即可，不做配对）。
	tlsConn, err := dialDataTLS(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(s.tcpPort)), certPath)
	if err != nil {
		t.Fatalf("数据面 TLS 握手失败（说明未按 data_tls=true 监听）: %v", err)
	}
	_ = tlsConn.Close()

	// 明文客户端连 TLS 数据面：被拒绝（连接关闭），且不产生任何业务数据。
	plain, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(s.tcpPort)), 2*time.Second)
	if err != nil {
		t.Fatalf("明文拨号数据面: %v", err)
	}
	defer plain.Close()
	if err := protocol.WriteFrame(plain, protocol.MsgBind, &protocol.Bind{
		PairingKey: "svc/conn", Role: protocol.RoleClient,
	}); err != nil {
		t.Fatalf("写明文报到帧: %v", err)
	}
	expectClosed(t, plain, 3*time.Second)
	waitFor(t, 3*time.Second, "数据面 TLS 握手失败日志", func() bool {
		return strings.Contains(logs.String(), "data_plane_tls_handshake_fail")
	})

	out := logs.String()
	if !strings.Contains(out, "data_plane_tls_handshake_fail") {
		t.Errorf("明文连 TLS 数据面必须记录 data_plane_tls_handshake_fail:\n%s", out)
	}
	if !strings.Contains(out, "数据面: TLS") {
		t.Errorf("启动日志应显式说明数据面 TLS:\n%s", out)
	}
}

// TestRelayDataTLSDisabled 覆盖 tunnel TLS + data_tls=false：
// 数据面保持明文可用（完整配对 + 回显），TLS 客户端握手失败。
func TestRelayDataTLSDisabled(t *testing.T) {
	logs := &lockedBuffer{}
	certPath, keyPath := writeTestCert(t)
	s := startRelayWithLogs(t, testConfig(t, func(c *config.Config) {
		c.Relay.TLS = config.TLSTrue
		c.Relay.TLSCert = certPath
		c.Relay.TLSKey = keyPath
		c.Relay.DataTLS = config.TLSFalse
	}), logs)

	agentCtrl, _ := dialControlTLS(t, s.tunnelAddr, certPath)
	ok := agentCtrl.hello(protocol.RoleAgent, testToken)
	if ok.DataTLS == nil || *ok.DataTLS {
		t.Fatalf("HelloOK.data_tls = %v，期望显式 false", ok.DataTLS)
	}
	agentCtrl.register("svc-plain", "tcp")

	clientCtrl, _ := dialControlTLS(t, s.tunnelAddr, certPath)
	clientCtrl.hello(protocol.RoleClient, testToken)
	incoming, result := issuePair(t, agentCtrl, clientCtrl, "svc-plain", "conn-plain")

	agentData := bindData(t, "127.0.0.1", s.tcpPort, &protocol.Bind{
		PairingKey:   incoming.PairingKey,
		PairingToken: incoming.PairingToken,
		Role:         protocol.RoleAgent,
	})
	clientData := bindData(t, "127.0.0.1", s.tcpPort, &protocol.Bind{
		PairingKey:   result.PairingKey,
		PairingToken: result.PairingToken,
		Role:         protocol.RoleClient,
	})
	echoRoundTrip(t, clientData, agentData, "plaintext-data-plane")
	_ = clientData.Close()
	_ = agentData.Close()

	// 数据面是明文：TLS 握手必须失败（不能悄悄接受）。
	if conn, err := dialDataTLS(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(s.tcpPort)), certPath); err == nil {
		_ = conn.Close()
		t.Fatal("data_tls=false 时 TLS 客户端不应能完成数据面握手")
	}

	out := logs.String()
	if !strings.Contains(out, "data_tls = false") {
		t.Errorf("启动日志应说明 data_tls=false 导致数据面明文:\n%s", out)
	}
	if !strings.Contains(out, "数据面: 明文") {
		t.Errorf("启动日志应显式说明数据面明文:\n%s", out)
	}
}

// TestRelayDataTLSAutoFollowsTunnel 覆盖 data_tls=auto 跟随隧道判定。
func TestRelayDataTLSAutoFollowsTunnel(t *testing.T) {
	certPath, keyPath := writeTestCert(t)

	t.Run("隧道TLS时数据面TLS", func(t *testing.T) {
		s := startTestServer(t, testConfig(t, func(c *config.Config) {
			c.Relay.TLS = config.TLSTrue
			c.Relay.TLSCert = certPath
			c.Relay.TLSKey = keyPath
		}))
		agent, _ := dialControlTLS(t, s.tunnelAddr, certPath)
		ok := agent.hello(protocol.RoleAgent, testToken)
		if ok.DataTLS == nil || !*ok.DataTLS {
			t.Fatalf("auto 应跟随隧道 TLS，实际 data_tls=%v", ok.DataTLS)
		}
	})

	t.Run("隧道明文时数据面明文", func(t *testing.T) {
		s := startTestServer(t, testConfig(t, func(c *config.Config) {
			c.Relay.TLS = config.TLSFalse
		}))
		agent := dialControl(t, s.tunnelAddr)
		ok := agent.hello(protocol.RoleAgent, testToken)
		if ok.DataTLS == nil || *ok.DataTLS {
			t.Fatalf("auto 应跟随隧道明文，实际 data_tls=%v", ok.DataTLS)
		}
		plain, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(s.tcpPort)), 2*time.Second)
		if err != nil {
			t.Fatalf("明文数据面应可连接: %v", err)
		}
		_ = plain.Close()
	})
}

// TestRelayDataTLSRequiresCert 覆盖 data_tls=true 但缺证书时启动失败（不静默降级）。
func TestRelayDataTLSRequiresCert(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Relay.TLS = config.TLSFalse
		c.Relay.DataTLS = config.TLSTrue
	})
	s := newServer(cfg)
	err := s.prepareTLS()
	if err == nil || !strings.Contains(err.Error(), "data_tls = true") {
		t.Fatalf("缺证书时 prepareTLS 应报错，实际 %v", err)
	}
}
