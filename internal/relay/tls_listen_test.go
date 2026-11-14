package relay

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// TestTunnelListenTLSExplicitOn 覆盖 tls=true：必须 tls.Listen，且使用自签证书的
// tls.Dial 能完成握手并跑通一次 Hello/HelloOK。
func TestTunnelListenTLSExplicitOn(t *testing.T) {
	certPath, keyPath := writeTestCert(t)
	s := startTestServer(t, testConfig(t, func(c *config.Config) {
		c.Relay.TLS = config.TLSTrue
		c.Relay.TLSCert = certPath
		c.Relay.TLSKey = keyPath
	}))

	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("读取测试证书: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		t.Fatal("测试证书不是合法 PEM")
	}
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", s.tunnelAddr, &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    pool,
		ServerName: "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("tls.Dial 自签证书握手失败（说明 relay 未按 tls=true 监听 TLS）: %v", err)
	}
	defer conn.Close()

	c := &ctrlConn{t: t, conn: conn}
	ok := c.hello(protocol.RoleAgent, testToken)
	mustPrefix(t, ok.AgentID, "a-", "AgentID")
	if state := conn.ConnectionState(); state.Version < tls.VersionTLS12 {
		t.Fatalf("TLS 版本过低: %x", state.Version)
	}
}

// TestTunnelListenTLSFalseWithCert 覆盖 tls=false + 已配置证书：
// 必须明文监听（明文客户端能完成 Hello），TLS 客户端握手失败。
func TestTunnelListenTLSFalseWithCert(t *testing.T) {
	certPath, keyPath := writeTestCert(t)
	s := startTestServer(t, testConfig(t, func(c *config.Config) {
		c.Relay.TLS = config.TLSFalse
		c.Relay.TLSCert = certPath
		c.Relay.TLSKey = keyPath
	}))

	// 明文客户端：能连上并完成一次 Hello。
	plain := dialControl(t, s.tunnelAddr)
	plain.hello(protocol.RoleAgent, testToken)

	// TLS 客户端：对端不说 TLS，握手必须失败。
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", s.tunnelAddr, &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true,
	})
	if err == nil {
		_ = conn.Close()
		t.Fatal("tls=false 时 TLS 客户端握手应失败（relay 必须按明文监听）")
	}
}
