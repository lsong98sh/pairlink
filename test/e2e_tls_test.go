package e2e

import (
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
)

// testPKI 是 e2e TLS 用例的自签证书材料（CA + relay 服务端证书）。
type testPKI struct {
	// CAPath 是 CA 证书（给 agent/client 的 tls_ca）。
	CAPath string
	// CertPath / KeyPath 是 relay 的 tls_cert / tls_key。
	CertPath, KeyPath string
}

// newTestPKI 生成一套自签 CA 与由它签发的 relay 服务端证书（SAN=127.0.0.1/localhost）。
//
// 全部写入 t.TempDir()，不依赖任何外部证书文件；私钥只落盘到临时目录。
func newTestPKI(t *testing.T) testPKI {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成 CA 私钥: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "PairLink e2e test CA", Organization: []string{"PairLink"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("生成 CA 证书: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("解析 CA 证书: %v", err)
	}

	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成服务端私钥: %v", err)
	}
	srvTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "pairlink-relay-e2e", Organization: []string{"PairLink"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("生成服务端证书: %v", err)
	}
	srvKeyDER, err := x509.MarshalECPrivateKey(srvKey)
	if err != nil {
		t.Fatalf("编码服务端私钥: %v", err)
	}

	dir := t.TempDir()
	pki := testPKI{
		CAPath:   filepath.Join(dir, "ca.pem"),
		CertPath: filepath.Join(dir, "server.pem"),
		KeyPath:  filepath.Join(dir, "server.key"),
	}
	writePEM(t, pki.CAPath, "CERTIFICATE", caDER)
	writePEM(t, pki.CertPath, "CERTIFICATE", srvDER)
	writePEM(t, pki.KeyPath, "EC PRIVATE KEY", srvKeyDER)
	return pki
}

// writePEM 把 DER 编码写入 PEM 文件。
func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("创建 %s: %v", path, err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: blockType, Bytes: der}); err != nil {
		t.Fatalf("写 PEM %s: %v", path, err)
	}
}

// TestE2ETLSTCPEcho 覆盖 TLS 隧道的完整端到端路径：
// 自签 CA → relay tls.Listen（tls_cert/tls_key）→ agent/client 用 tls_ca 校验 →
// 注册 → 配对 → TCP echo → 断开清理。数据面按设计仍是明文。
func TestE2ETLSTCPEcho(t *testing.T) {
	pki := newTestPKI(t)
	echoAddr := startTCPEchoServer(t)

	relayManage := hostPort(freeTCPPort(t))
	relayCfg, _ := writeRelayINI(t, relaySpec{
		Tunnel:      hostPort(freeTCPPort(t)),
		TCP:         hostPort(freeTCPPort(t)),
		Manage:      relayManage,
		TLSCert:     pki.CertPath,
		TLSKey:      pki.KeyPath,
		PairTimeout: 5 * time.Second,
	})
	relayH := startRelay(t, relayCfg)

	agentCfg, _ := writeAgentINI(t, agentSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    hostPort(freeTCPPort(t)),
		Protocol:  "tcp",
		Local:     echoAddr,
		TLSCA:     pki.CAPath,
	})
	agentH := startAgent(t, agentCfg)
	waitServicesRegistered(t, relayH, relayManage, "default")

	clientPort := freeTCPPort(t)
	clientCfg, _ := writeClientINI(t, clientSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    hostPort(freeTCPPort(t)),
		Services:  []clientService{{ID: "default", Listen: hostPort(clientPort)}},
		TLSCA:     pki.CAPath,
	})
	clientH := startClient(t, clientCfg)
	waitClientMappings(t, clientH, clientCfg.Common.ManageListen, map[string]string{
		"default": hostPort(clientPort),
	})

	conn := dialWithRetry(t, hostPort(clientPort))
	echoText(t, conn, "tls-echo-1")
	_ = conn.Close()

	// 配对表在转发结束后回落（复用既有管理页断言，证明清理路径正常）。
	waitRelayPairCount(t, relayH, relayManage, 0)

	// 关停顺序：client → agent → relay，三者 Run 都必须返回 nil。
	stopAll(t, clientH, agentH, relayH)
}

// TestE2ETLSInsecureSkipVerify 覆盖"client 只配 tls_insecure_skip_verify=true
// （没有 tls_ca）也能连 TLS relay"这条明确支持的组合。
func TestE2ETLSInsecureSkipVerify(t *testing.T) {
	pki := newTestPKI(t)
	echoAddr := startTCPEchoServer(t)

	relayManage := hostPort(freeTCPPort(t))
	relayCfg, _ := writeRelayINI(t, relaySpec{
		Tunnel:  hostPort(freeTCPPort(t)),
		TCP:     hostPort(freeTCPPort(t)),
		Manage:  relayManage,
		TLSCert: pki.CertPath,
		TLSKey:  pki.KeyPath,
	})
	relayH := startRelay(t, relayCfg)

	// agent 用 CA 正常校验证书；client 跳过校验（仅测试场景）。
	agentCfg, _ := writeAgentINI(t, agentSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    hostPort(freeTCPPort(t)),
		Protocol:  "tcp",
		Local:     echoAddr,
		TLSCA:     pki.CAPath,
	})
	agentH := startAgent(t, agentCfg)
	waitServicesRegistered(t, relayH, relayManage, "default")

	clientPort := freeTCPPort(t)
	clientCfg, _ := writeClientINI(t, clientSpec{
		RelayAddr:   relayCfg.Relay.TunnelListen,
		Manage:      hostPort(freeTCPPort(t)),
		Services:    []clientService{{ID: "default", Listen: hostPort(clientPort)}},
		TLSInsecure: true,
	})
	clientH := startClient(t, clientCfg)
	waitClientMappings(t, clientH, clientCfg.Common.ManageListen, map[string]string{
		"default": hostPort(clientPort),
	})

	conn := dialWithRetry(t, hostPort(clientPort))
	echoText(t, conn, "tls-insecure-ok")
	_ = conn.Close()

	stopAll(t, clientH, agentH, relayH)
}

// TestE2ETLSPlaintextAgentAgainstTLSRelay 覆盖最容易踩的配置错误：
// relay 开了 TLS，agent 仍是明文（既没有 tls_ca 也没有 tls_insecure_skip_verify）。
//
// 必须**明确失败**而不是卡住或静默：Run 返回可读错误（含 "agent:" 前缀与
// 握手/读取失败的底层原因），进程退出由外部守护重启。
func TestE2ETLSPlaintextAgentAgainstTLSRelay(t *testing.T) {
	pki := newTestPKI(t)
	echoAddr := startTCPEchoServer(t)

	relayCfg, _ := writeRelayINI(t, relaySpec{
		Tunnel:  hostPort(freeTCPPort(t)),
		TCP:     hostPort(freeTCPPort(t)),
		TLSCert: pki.CertPath,
		TLSKey:  pki.KeyPath,
	})
	relayH := startRelay(t, relayCfg)

	// 明文 agent：不配 tls_ca / tls_insecure_skip_verify。
	//
	// 阶段 6C 起 agent 默认自动重连，而"对端要求 TLS 但本端是明文"只能表现为
	// 读 HelloOK 时 EOF（无法与瞬时断链区分），因此本用例固定 reconnect=false，
	// 继续断言"配置不匹配必须明确失败、进程退出"。
	agentCfg, _ := writeAgentINI(t, agentSpec{
		RelayAddr:   relayCfg.Relay.TunnelListen,
		Protocol:    "tcp",
		Local:       echoAddr,
		NoReconnect: true,
	})
	agentH := startAgent(t, agentCfg)

	err := waitRunError(t, agentH, 8*time.Second)
	if err == nil {
		t.Fatal("明文 agent 连接 TLS relay 必须返回错误")
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "agent:") {
		t.Fatalf("错误信息应带 agent: 前缀，实际 %q", msg)
	}
	// 明文客户端把 MsgHello 写进 TLS 记录层，relay 握手失败后关闭连接，
	// 因此底层原因通常是 EOF / connection reset / broken pipe 之一。
	if !containsAny(msg, "EOF", "reset", "broken pipe", "读取") {
		t.Fatalf("错误信息应可读（含握手/读取失败原因），实际 %q", msg)
	}
	if !agentH.exited() {
		t.Fatal("agent Run 应已退出")
	}
	stopAll(t, relayH)
}

// TestE2ETLSPlaintextClientAgainstTLSRelay 覆盖 client 侧的同一类配置错误。
func TestE2ETLSPlaintextClientAgainstTLSRelay(t *testing.T) {
	pki := newTestPKI(t)
	echoAddr := startTCPEchoServer(t)

	relayCfg, _ := writeRelayINI(t, relaySpec{
		Tunnel:  hostPort(freeTCPPort(t)),
		TCP:     hostPort(freeTCPPort(t)),
		TLSCert: pki.CertPath,
		TLSKey:  pki.KeyPath,
	})
	relayH := startRelay(t, relayCfg)

	agentCfg, _ := writeAgentINI(t, agentSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Protocol:  "tcp",
		Local:     echoAddr,
		TLSCA:     pki.CAPath,
	})
	agentH := startAgent(t, agentCfg)

	// 明文 client（同 agent 侧理由：这里固定 reconnect=false 以断言明确失败）。
	clientCfg, _ := writeClientINI(t, clientSpec{
		RelayAddr:   relayCfg.Relay.TunnelListen,
		Services:    []clientService{{ID: "default", Listen: hostPort(freeTCPPort(t))}},
		NoReconnect: true,
	})
	clientH := startClient(t, clientCfg)

	err := waitRunError(t, clientH, 8*time.Second)
	if err == nil {
		t.Fatal("明文 client 连接 TLS relay 必须返回错误")
	}
	if !strings.HasPrefix(err.Error(), "client:") {
		t.Fatalf("错误信息应带 client: 前缀，实际 %q", err.Error())
	}
	stopAll(t, agentH, relayH)
}

// TestE2ETLSAgentTLSAgainstPlaintextRelay 覆盖反方向的配置不匹配：
// relay 明文、agent 配了 tls_ca → TLS 握手失败，错误必须点明 TLS。
func TestE2ETLSAgentTLSAgainstPlaintextRelay(t *testing.T) {
	pki := newTestPKI(t)
	echoAddr := startTCPEchoServer(t)

	relayCfg, _ := writeRelayINI(t, relaySpec{
		Tunnel: hostPort(freeTCPPort(t)),
		TCP:    hostPort(freeTCPPort(t)),
	})
	relayH := startRelay(t, relayCfg)

	agentCfg, _ := writeAgentINI(t, agentSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Protocol:  "tcp",
		Local:     echoAddr,
		TLSCA:     pki.CAPath,
	})
	agentH := startAgent(t, agentCfg)

	err := waitRunError(t, agentH, 8*time.Second)
	if err == nil {
		t.Fatal("TLS agent 连接明文 relay 必须返回错误")
	}
	if !strings.Contains(err.Error(), "TLS") {
		t.Fatalf("错误信息应提到 TLS 握手失败，实际 %q", err.Error())
	}
	stopAll(t, relayH)
}

// TestE2ETLSExplicitOn 覆盖三端都显式 tls=true 的正向路径：
// relay tls=true + 自签证书，agent/client tls=true + tls_ca 正常校验，TCP echo 通。
func TestE2ETLSExplicitOn(t *testing.T) {
	pki := newTestPKI(t)
	echoAddr := startTCPEchoServer(t)

	relayManage := hostPort(freeTCPPort(t))
	relayCfg, _ := writeRelayINI(t, relaySpec{
		Tunnel:  hostPort(freeTCPPort(t)),
		TCP:     hostPort(freeTCPPort(t)),
		Manage:  relayManage,
		TLSCert: pki.CertPath,
		TLSKey:  pki.KeyPath,
		TLS:     "true",
	})
	relayH := startRelay(t, relayCfg)

	agentCfg, _ := writeAgentINI(t, agentSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    hostPort(freeTCPPort(t)),
		Protocol:  "tcp",
		Local:     echoAddr,
		TLSCA:     pki.CAPath,
		TLS:       "true",
	})
	agentH := startAgent(t, agentCfg)
	waitServicesRegistered(t, relayH, relayManage, "default")

	clientPort := freeTCPPort(t)
	clientCfg, _ := writeClientINI(t, clientSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    hostPort(freeTCPPort(t)),
		Services:  []clientService{{ID: "default", Listen: hostPort(clientPort)}},
		TLSCA:     pki.CAPath,
		TLS:       "true",
	})
	clientH := startClient(t, clientCfg)
	waitClientMappings(t, clientH, clientCfg.Common.ManageListen, map[string]string{
		"default": hostPort(clientPort),
	})

	conn := dialWithRetry(t, hostPort(clientPort))
	echoText(t, conn, "tls-explicit-on")
	_ = conn.Close()

	waitRelayPairCount(t, relayH, relayManage, 0)
	stopAll(t, clientH, agentH, relayH)
}

// TestE2ETLSSystemRootsRejectsSelfSigned 覆盖 tls=true + 不配 tls_ca：
// 必须使用系统根证书做标准校验，因此自签 relay 证书**必然被拒绝**——
// 这证明 tls=true 不会像 tls_insecure_skip_verify 那样静默跳过校验。
func TestE2ETLSSystemRootsRejectsSelfSigned(t *testing.T) {
	pki := newTestPKI(t)
	echoAddr := startTCPEchoServer(t)

	relayCfg, _ := writeRelayINI(t, relaySpec{
		Tunnel:  hostPort(freeTCPPort(t)),
		TCP:     hostPort(freeTCPPort(t)),
		TLSCert: pki.CertPath,
		TLSKey:  pki.KeyPath,
		TLS:     "true",
	})
	relayH := startRelay(t, relayCfg)

	// 不配 tls_ca（也不配 insecure）：tls=true 必须走系统根证书。
	agentCfg, _ := writeAgentINI(t, agentSpec{
		RelayAddr:   relayCfg.Relay.TunnelListen,
		Protocol:    "tcp",
		Local:       echoAddr,
		TLS:         "true",
		NoReconnect: true,
	})
	agentH := startAgent(t, agentCfg)

	err := waitRunError(t, agentH, 10*time.Second)
	if err == nil {
		t.Fatal("tls=true 且不配 tls_ca 时，自签 relay 证书必须校验失败")
	}
	msg := err.Error()
	if !strings.Contains(strings.ToLower(msg), "tls") {
		t.Fatalf("错误信息应提到 TLS，实际 %q", msg)
	}
	if !containsAny(msg, "x509", "unknown authority", "certificate", "证书") {
		t.Fatalf("错误信息应给出证书校验失败的可读原因，实际 %q", msg)
	}
	if !agentH.exited() {
		t.Fatal("agent Run 应已退出")
	}
	stopAll(t, relayH)
}

// TestE2ETLSPlainWhenFalse 覆盖 tls=false 强制明文：
// 即使 relay 配了证书，三端都显式 false 时也必须按明文连通。
func TestE2ETLSPlainWhenFalse(t *testing.T) {
	pki := newTestPKI(t)
	echoAddr := startTCPEchoServer(t)

	relayManage := hostPort(freeTCPPort(t))
	relayCfg, _ := writeRelayINI(t, relaySpec{
		Tunnel:  hostPort(freeTCPPort(t)),
		TCP:     hostPort(freeTCPPort(t)),
		Manage:  relayManage,
		TLSCert: pki.CertPath,
		TLSKey:  pki.KeyPath,
		TLS:     "false",
	})
	relayH := startRelay(t, relayCfg)

	agentCfg, _ := writeAgentINI(t, agentSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    hostPort(freeTCPPort(t)),
		Protocol:  "tcp",
		Local:     echoAddr,
		TLS:       "false",
	})
	agentH := startAgent(t, agentCfg)
	waitServicesRegistered(t, relayH, relayManage, "default")

	clientPort := freeTCPPort(t)
	clientCfg, _ := writeClientINI(t, clientSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    hostPort(freeTCPPort(t)),
		Services:  []clientService{{ID: "default", Listen: hostPort(clientPort)}},
		TLS:       "false",
	})
	clientH := startClient(t, clientCfg)
	waitClientMappings(t, clientH, clientCfg.Common.ManageListen, map[string]string{
		"default": hostPort(clientPort),
	})

	conn := dialWithRetry(t, hostPort(clientPort))
	echoText(t, conn, "tls-forced-plain")
	_ = conn.Close()

	waitRelayPairCount(t, relayH, relayManage, 0)
	stopAll(t, clientH, agentH, relayH)
}

// TestE2ETLSMismatchExplicit 覆盖显式不匹配：relay tls=false（明文）而 agent tls=true。
// TLS 握手必须失败，且错误要指向 tls 配置（而不是静默卡住或重试）。
func TestE2ETLSMismatchExplicit(t *testing.T) {
	echoAddr := startTCPEchoServer(t)

	relayCfg, _ := writeRelayINI(t, relaySpec{
		Tunnel: hostPort(freeTCPPort(t)),
		TCP:    hostPort(freeTCPPort(t)),
		TLS:    "false",
	})
	relayH := startRelay(t, relayCfg)

	agentCfg, _ := writeAgentINI(t, agentSpec{
		RelayAddr:   relayCfg.Relay.TunnelListen,
		Protocol:    "tcp",
		Local:       echoAddr,
		TLS:         "true",
		NoReconnect: true,
	})
	agentH := startAgent(t, agentCfg)

	err := waitRunError(t, agentH, 10*time.Second)
	if err == nil {
		t.Fatal("relay 明文而 agent tls=true 必须失败")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "tls") {
		t.Fatalf("错误信息应指向 tls 配置，实际 %q", err.Error())
	}
	if !agentH.exited() {
		t.Fatal("agent Run 应已退出")
	}
	stopAll(t, relayH)
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// waitRunError 等待 Run 退出并返回其错误（超时则 Fatal）。
func waitRunError(t *testing.T, h *runHandle, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if h.exited() {
			return h.result()
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: Run 未在 %s 内退出", h.name, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// stopAll 依次停止多个 Run，并断言它们都返回 nil（ctx 取消视为正常退出）。
func stopAll(t *testing.T, hs ...*runHandle) {
	t.Helper()
	for _, h := range hs {
		h.stop(t, 15*time.Second)
		if err := h.result(); err != nil {
			t.Errorf("%s: Run 返回错误: %v", h.name, err)
		}
	}
}

// dialWithRetry 在超时内重试拨号本地监听地址。
func dialWithRetry(t *testing.T, addr string) net.Conn {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err == nil {
			return conn
		}
		if time.Now().After(deadline) {
			t.Fatalf("拨号 %s 超时: %v", addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// containsAny 判断字符串是否包含任一子串。
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
