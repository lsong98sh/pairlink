package e2e

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/protocol"
)

// dialDataPlaneTLS 用测试 CA 校验 relay 的 TCP 数据面证书。
func dialDataPlaneTLS(addr, caPath string) (*tls.Conn, error) {
	pemBytes, err := os.ReadFile(caPath)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("测试 CA %s 不是合法 PEM", caPath)
	}
	d := &net.Dialer{Timeout: 3 * time.Second}
	return tls.DialWithDialer(d, "tcp", addr, &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    pool,
		ServerName: "127.0.0.1",
	})
}

// assertPlaintextDataRejected 断言明文连接不能用于数据面：写一帧 MsgBind 后
// 连接必须被快速关闭（不等待 pair_timeout），且不产生任何业务响应。
func assertPlaintextDataRejected(t *testing.T, addr string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("明文拨号数据面 %s: %v", addr, err)
	}
	defer conn.Close()
	if err := protocol.WriteFrame(conn, protocol.MsgBind, &protocol.Bind{
		PairingKey:   "svc/conn",
		PairingToken: "bogus",
		Role:         protocol.RoleClient,
	}); err != nil {
		t.Fatalf("写明文报到帧: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	n, err := conn.Read(buf)
	if err == nil {
		t.Fatalf("明文连接在 TLS 数据面上收到了 %d 字节响应，说明未被拒绝", n)
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("明文连接未被关闭（仍在等 pair_timeout），TLS 数据面应快速拒绝")
	}
}

// TestE2EDataPlaneTLS 覆盖 tunnel TLS + 数据面 TLS 的完整链路：
// relay data_tls=true，agent/client data_tls=true + tls_ca → 注册/配对/TCP echo，
// 并直接从外部确认数据面要求 TLS、明文连接被拒。
func TestE2EDataPlaneTLS(t *testing.T) {
	guardGoroutines(t)
	pki := newTestPKI(t)
	echoAddr := startTCPEchoServer(t)

	relayManage := hostPort(freeTCPPort(t))
	relayCfg, _ := writeRelayINI(t, relaySpec{
		Tunnel:      hostPort(freeTCPPort(t)),
		TCP:         hostPort(freeTCPPort(t)),
		Manage:      relayManage,
		TLS:         "true",
		TLSCert:     pki.CertPath,
		TLSKey:      pki.KeyPath,
		DataTLS:     "true",
		PairTimeout: 5 * time.Second,
	})
	relayH := startRelay(t, relayCfg)

	// 数据面必须只接受 TLS：用同一套 CA 握手成功，明文连接被拒。
	dataAddr := relayCfg.Relay.TCPListen
	tlsConn, err := dialDataPlaneTLS(dataAddr, pki.CAPath)
	if err != nil {
		t.Fatalf("data_tls=true 时数据面 TLS 握手失败: %v", err)
	}
	_ = tlsConn.Close()
	assertPlaintextDataRejected(t, dataAddr)

	agentCfg, _ := writeAgentINI(t, agentSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    hostPort(freeTCPPort(t)),
		Protocol:  "tcp",
		Local:     echoAddr,
		TLSCA:     pki.CAPath,
		DataTLS:   "true",
	})
	agentH := startAgent(t, agentCfg)
	waitServicesRegistered(t, relayH, relayManage, "default")

	clientPort := freeTCPPort(t)
	clientCfg, _ := writeClientINI(t, clientSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    hostPort(freeTCPPort(t)),
		Services:  []clientService{{ID: "default", Listen: hostPort(clientPort)}},
		TLSCA:     pki.CAPath,
		DataTLS:   "true",
	})
	clientH := startClient(t, clientCfg)
	waitClientMappings(t, clientH, clientCfg.Common.ManageListen, map[string]string{
		"default": hostPort(clientPort),
	})

	// 业务数据经"TLS 隧道 → relay → TLS 数据面"端到端可用。
	conn := dialWithRetry(t, hostPort(clientPort))
	echoText(t, conn, "data-tls-e2e")
	_ = conn.Close()
	waitRelayPairCount(t, relayH, relayManage, 0)

	stopAll(t, clientH, agentH, relayH)
}

// TestE2EDataPlaneTLSDisabled 覆盖 tunnel TLS + data_tls=false：
// 控制面加密、数据面显式明文，端到端仍可用；TLS 客户端不能连数据面。
func TestE2EDataPlaneTLSDisabled(t *testing.T) {
	guardGoroutines(t)
	pki := newTestPKI(t)
	echoAddr := startTCPEchoServer(t)

	relayManage := hostPort(freeTCPPort(t))
	relayCfg, _ := writeRelayINI(t, relaySpec{
		Tunnel:  hostPort(freeTCPPort(t)),
		TCP:     hostPort(freeTCPPort(t)),
		Manage:  relayManage,
		TLS:     "true",
		TLSCert: pki.CertPath,
		TLSKey:  pki.KeyPath,
		DataTLS: "false",
	})
	relayH := startRelay(t, relayCfg)

	agentCfg, _ := writeAgentINI(t, agentSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    hostPort(freeTCPPort(t)),
		Protocol:  "tcp",
		Local:     echoAddr,
		TLSCA:     pki.CAPath,
		DataTLS:   "false",
	})
	agentH := startAgent(t, agentCfg)
	waitServicesRegistered(t, relayH, relayManage, "default")

	clientPort := freeTCPPort(t)
	clientCfg, _ := writeClientINI(t, clientSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    hostPort(freeTCPPort(t)),
		Services:  []clientService{{ID: "default", Listen: hostPort(clientPort)}},
		TLSCA:     pki.CAPath,
		DataTLS:   "false",
	})
	clientH := startClient(t, clientCfg)
	waitClientMappings(t, clientH, clientCfg.Common.ManageListen, map[string]string{
		"default": hostPort(clientPort),
	})

	conn := dialWithRetry(t, hostPort(clientPort))
	echoText(t, conn, "data-plain-e2e")
	_ = conn.Close()

	// 数据面确实是明文：TLS 客户端握手失败。
	dataAddr := relayCfg.Relay.TCPListen
	if c, err := dialDataPlaneTLS(dataAddr, pki.CAPath); err == nil {
		_ = c.Close()
		t.Fatal("data_tls=false 时数据面不应能完成 TLS 握手")
	}

	stopAll(t, clientH, agentH, relayH)
}

// TestE2EDataPlaneTLSMismatch 覆盖两侧 data_tls 冲突时**明确失败、不挂起**。
func TestE2EDataPlaneTLSMismatch(t *testing.T) {
	guardGoroutines(t)
	pki := newTestPKI(t)
	echoAddr := startTCPEchoServer(t)

	relayCfg, _ := writeRelayINI(t, relaySpec{
		Tunnel:  hostPort(freeTCPPort(t)),
		TCP:     hostPort(freeTCPPort(t)),
		TLS:     "true",
		TLSCert: pki.CertPath,
		TLSKey:  pki.KeyPath,
		DataTLS: "true",
	})
	relayH := startRelay(t, relayCfg)

	agentCfg, _ := writeAgentINI(t, agentSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Protocol:  "tcp",
		Local:     echoAddr,
		TLSCA:     pki.CAPath,
		DataTLS:   "true",
	})
	agentH := startAgent(t, agentCfg)

	// client 强制明文，relay 要求 TLS：必须在握手阶段永久失败，不能挂起。
	clientCfg, _ := writeClientINI(t, clientSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Services:  []clientService{{ID: "default", Listen: hostPort(freeTCPPort(t))}},
		TLSCA:     pki.CAPath,
		DataTLS:   "false",
	})
	clientH := startClient(t, clientCfg)
	err := waitRunError(t, clientH, 8*time.Second)
	if err == nil {
		t.Fatal("client data_tls=false 与 relay data_tls=true 冲突时必须退出并报错")
	}
	if !strings.Contains(err.Error(), "data_tls = false") {
		t.Errorf("错误应说明本端 data_tls=false，实际 %q", err.Error())
	}
	if !strings.Contains(err.Error(), "relay 要求数据面 TLS") {
		t.Errorf("错误应说明 relay 要求数据面 TLS，实际 %q", err.Error())
	}

	stopAll(t, agentH, relayH)
}

// TestE2EDataPlaneTLSRequiresNewRelay 覆盖"客户端 data_tls=true + relay data_tls=false"：
// 同样必须明确失败（不静默降级）。
func TestE2EDataPlaneTLSRequiresNewRelay(t *testing.T) {
	guardGoroutines(t)
	pki := newTestPKI(t)
	echoAddr := startTCPEchoServer(t)

	relayCfg, _ := writeRelayINI(t, relaySpec{
		Tunnel:  hostPort(freeTCPPort(t)),
		TCP:     hostPort(freeTCPPort(t)),
		TLS:     "true",
		TLSCert: pki.CertPath,
		TLSKey:  pki.KeyPath,
		DataTLS: "false",
	})
	relayH := startRelay(t, relayCfg)

	agentCfg, _ := writeAgentINI(t, agentSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Protocol:  "tcp",
		Local:     echoAddr,
		TLSCA:     pki.CAPath,
		DataTLS:   "false",
	})
	agentH := startAgent(t, agentCfg)

	clientCfg, _ := writeClientINI(t, clientSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Services:  []clientService{{ID: "default", Listen: hostPort(freeTCPPort(t))}},
		TLSCA:     pki.CAPath,
		DataTLS:   "true",
	})
	clientH := startClient(t, clientCfg)
	err := waitRunError(t, clientH, 8*time.Second)
	if err == nil {
		t.Fatal("client data_tls=true 与 relay data_tls=false 冲突时必须退出并报错")
	}
	if !strings.Contains(err.Error(), "data_tls = true") {
		t.Errorf("错误应说明本端 data_tls=true，实际 %q", err.Error())
	}

	stopAll(t, agentH, relayH)
}
