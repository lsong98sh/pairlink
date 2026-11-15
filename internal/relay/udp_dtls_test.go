package relay

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/pion/dtls/v3"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/pairing"
	"github.com/lsong98sh/pairlink/internal/protocol"
	"github.com/lsong98sh/pairlink/internal/udpdtls"
)

// 本文件覆盖 v0.5.0 原生 UDP 数据面的 DTLS-PSK 与 relay 侧 token 签发限流。

// newUDPDTLSCfg 返回一个启用原生 UDP 数据面 + DTLS 的 relay 测试配置。
func newUDPDTLSCfg(t *testing.T, mutate func(*config.Config)) *config.Config {
	t.Helper()
	return testConfig(t, func(c *config.Config) {
		c.Relay.UDPListen = reserveUDPAddr(t)
		c.Relay.UDPDTLS = true
		if mutate != nil {
			mutate(c)
		}
	})
}

// dtlsDialRelay 以 DTLS-PSK 客户端身份连接 relay 的 UDP 数据面并完成握手。
func dtlsDialRelay(t *testing.T, addr string, key pairing.PairingKey, authToken string) *dtls.Conn {
	t.Helper()
	conn := dtlsDialRelayErr(t, addr, key, authToken)
	if conn == nil {
		t.Fatalf("与 relay 的 DTLS 握手失败")
	}
	return conn
}

// dtlsDialRelayErr 同上，但握手失败返回 nil（便于断言失败场景）。
func dtlsDialRelayErr(t *testing.T, addr string, key pairing.PairingKey, authToken string) *dtls.Conn {
	t.Helper()
	raddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatalf("解析 relay UDP 地址 %s: %v", addr, err)
	}
	psk, err := udpdtls.Derive(authToken, key.String())
	if err != nil {
		t.Fatalf("派生 PSK: %v", err)
	}
	conn, err := dtls.Dial("udp", raddr, &dtls.Config{
		CipherSuites:    []dtls.CipherSuiteID{dtls.TLS_PSK_WITH_AES_128_GCM_SHA256},
		PSKIdentityHint: []byte(key.String()),
		PSK:             func([]byte) ([]byte, error) { return psk, nil },
	})
	if err != nil {
		t.Fatalf("建立 DTLS 客户端: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		t.Logf("DTLS 握手按预期失败: %v", err)
		return nil
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// dtlsWrite 在 DTLS 连接上写一个数据报。
func dtlsWrite(t *testing.T, conn net.Conn, payload []byte) {
	t.Helper()
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("写 DTLS 数据: %v", err)
	}
}

// dtlsRead 在 DTLS 连接上读一个数据报。
func dtlsRead(t *testing.T, conn net.Conn, timeout time.Duration) []byte {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, protocol.MaxUDPDatagram)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("读 DTLS 数据: %v", err)
	}
	return append([]byte(nil), buf[:n]...)
}

// dtlsExpectTimeout 断言在 timeout 内没有收到任何应用数据。
func dtlsExpectTimeout(t *testing.T, conn net.Conn, timeout time.Duration) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 1024)
	if n, err := conn.Read(buf); err == nil {
		t.Fatalf("本应没有数据，却收到 %d 字节: %q", n, string(buf[:n]))
	}
}

// dtlsSendBind 在 DTLS 连接上发送报到帧。
func dtlsSendBind(t *testing.T, conn net.Conn, key, token, role string) {
	t.Helper()
	frame, err := protocol.EncodeFrame(protocol.MsgBind, &protocol.Bind{
		PairingKey:   key,
		PairingToken: token,
		Role:         role,
	})
	if err != nil {
		t.Fatalf("编码 Bind: %v", err)
	}
	dtlsWrite(t, conn, frame)
}

// dtlsReadBindOK 读取并校验报到回执。
func dtlsReadBindOK(t *testing.T, conn net.Conn, key string) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 2048)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("读 BindOK: %v", err)
	}
	typ, payload, err := protocol.ParseFrame(buf[:n])
	if err != nil {
		t.Fatalf("解析 BindOK 帧: %v", err)
	}
	if typ != protocol.MsgBindOK {
		t.Fatalf("期望 MsgBindOK，实际 %s", typ)
	}
	msg, err := protocol.Decode(typ, payload)
	if err != nil {
		t.Fatalf("解码 BindOK: %v", err)
	}
	bindOK, ok := msg.(*protocol.BindOK)
	if !ok || bindOK.PairingKey != key {
		t.Fatalf("BindOK 内容异常: %+v", msg)
	}
}

// dtlsPair 是一条已经两端报到的 DTLS 配对。
type dtlsPair struct {
	agent, client net.Conn
	key           pairing.PairingKey
	incoming      *protocol.Incoming
	result        *protocol.PairResult
}

// setupDTLSPair 走完整信令并用 DTLS 报到 agent/client 两端。
func setupDTLSPair(t *testing.T, s *server, serviceID, connID string) *dtlsPair {
	t.Helper()
	agent := dialControl(t, s.tunnelAddr)
	agent.hello(protocol.RoleAgent, testToken)
	agent.register(serviceID, "udp")
	client := dialControl(t, s.tunnelAddr)
	client.hello(protocol.RoleClient, testToken)
	incoming, result := issuePair(t, agent, client, serviceID, connID)

	key := pairing.PairingKey{ServiceID: serviceID, ClientConnID: connID}
	agentConn := dtlsDialRelay(t, s.udpAddr, key, testToken)
	clientConn := dtlsDialRelay(t, s.udpAddr, key, testToken)

	dtlsSendBind(t, agentConn, incoming.PairingKey, incoming.PairingToken, protocol.RoleAgent)
	dtlsReadBindOK(t, agentConn, incoming.PairingKey)
	dtlsSendBind(t, clientConn, result.PairingKey, result.PairingToken, protocol.RoleClient)
	dtlsReadBindOK(t, clientConn, result.PairingKey)

	return &dtlsPair{agent: agentConn, client: clientConn, key: key, incoming: incoming, result: result}
}

// TestUDPDTLSForwardAndReclaim 覆盖 DTLS 报到后的双向转发、会话超时清理与对端回收。
func TestUDPDTLSForwardAndReclaim(t *testing.T) {
	cfg := newUDPDTLSCfg(t, func(c *config.Config) {
		c.Relay.UDPSessionTimeout = 400 * time.Millisecond
	})
	s := startTestServer(t, cfg)
	pair := setupDTLSPair(t, s, "udp-svc", "conn-dtls")

	if entry, ok := s.udpTable.LookupByKey(includingKey(t, pair.result.PairingKey)); !ok || !entry.Complete {
		t.Fatalf("DTLS 报到后 UDP 会话未进入 Full: %+v ok=%v", entry, ok)
	}
	if got := s.udpData.peerCount(); got != 2 {
		t.Fatalf("DTLS 对端数 = %d，期望 2", got)
	}

	// 双向转发（含 1400 字节的中等报文）。
	dtlsWrite(t, pair.agent, []byte("ping"))
	if got := dtlsRead(t, pair.client, 2*time.Second); string(got) != "ping" {
		t.Fatalf("client 收到 %q，期望 ping", got)
	}
	big := bytes.Repeat([]byte("a"), 1400)
	dtlsWrite(t, pair.client, big)
	if got := dtlsRead(t, pair.agent, 2*time.Second); !bytes.Equal(got, big) {
		t.Fatalf("agent 收到 %d 字节，期望 1400 字节且内容一致", len(got))
	}

	// 会话空闲超时清理，但对端仍多活一个 pair_timeout，未登记端点的新数据报被丢弃。
	waitFor(t, 4*time.Second, "DTLS 会话空闲清理", func() bool {
		_, ok := s.udpTable.LookupByKey(pair.key)
		return !ok
	})
	if got := s.udpData.peerCount(); got != 2 {
		t.Fatalf("会话清理后 DTLS 对端数 = %d，期望仍为 2（观察窗口内不拆连接）", got)
	}
	dtlsWrite(t, pair.agent, []byte("late"))
	dtlsExpectTimeout(t, pair.client, 300*time.Millisecond)

	// 观察窗口结束后对端全部回收。
	waitFor(t, 6*time.Second, "DTLS 对端回收", func() bool { return s.udpData.peerCount() == 0 })
	if got := s.udpTable.Len(); got != 0 {
		t.Fatalf("对端回收后 UDP 会话数 = %d，期望 0", got)
	}
}

// TestUDPDTLSLargeDatagramBoundary 记录 DTLS 数据面在大报文下的真实行为：
// 1400 / 8000 字节可正常往返；8192 字节超过 pion 的 8192 入站缓冲，
// 对端被回收且不会保留会话，relay 保持健康（随后重新配对仍可用）。
func TestUDPDTLSLargeDatagramBoundary(t *testing.T) {
	cfg := newUDPDTLSCfg(t, func(c *config.Config) {
		c.Relay.UDPSessionTimeout = 30 * time.Second
	})
	s := startTestServer(t, cfg)
	pair := setupDTLSPair(t, s, "udp-svc", "conn-big")

	for _, size := range []int{1400, 8000} {
		payload := bytes.Repeat([]byte("b"), size)
		dtlsWrite(t, pair.client, payload)
		got := dtlsRead(t, pair.agent, 2*time.Second)
		if !bytes.Equal(got, payload) {
			t.Fatalf("%d 字节报文转发不一致：收到 %d 字节", size, len(got))
		}
		t.Logf("DTLS 数据面 %d 字节报文往返成功（记录开销约 37 字节，远小于 8192 入站缓冲）", size)
	}

	// 8192 字节应用数据 + 记录开销 > 8192 入站缓冲：对端读失败并被回收。
	oversize := bytes.Repeat([]byte("c"), 8192)
	dtlsWrite(t, pair.client, oversize)
	waitFor(t, 4*time.Second, "超限报文导致对端回收", func() bool { return s.udpData.peerCount() == 1 })
	if got := s.udpTable.Len(); got != 0 {
		t.Fatalf("超限报文后 UDP 会话数 = %d，期望 0", got)
	}
	dtlsExpectTimeout(t, pair.client, 300*time.Millisecond)
	t.Logf("DTLS 数据面 8192 字节应用数据超出入站缓冲：对端被回收（relay 未崩溃）")

	// relay 仍然健康：重新配对（含双向收发）可用。
	again := setupDTLSPair(t, s, "udp-svc-big2", "conn-big-2")
	dtlsWrite(t, again.agent, []byte("after-oversize"))
	if got := dtlsRead(t, again.client, 2*time.Second); string(got) != "after-oversize" {
		t.Fatalf("超限报文后重新配对转发异常: %q", got)
	}
}

// TestUDPDTLSPlaintextRejected 覆盖 v0.4.0 明文端（裸 UDP + MsgBind）：
// 拿不到 BindOK、不建立会话、relay 只打限流 WARN，且随后仍可正常 DTLS 配对。
func TestUDPDTLSPlaintextRejected(t *testing.T) {
	cfg := newUDPDTLSCfg(t, nil)
	cfg.Common.LogLevel = "debug"
	logs := &lockedBuffer{}
	s := startRelayWithLogs(t, cfg, logs)

	agent := dialControl(t, s.tunnelAddr)
	agent.hello(protocol.RoleAgent, testToken)
	agent.register("udp-svc", "udp")
	client := dialControl(t, s.tunnelAddr)
	client.hello(protocol.RoleClient, testToken)
	incoming, _ := issuePair(t, agent, client, "udp-svc", "conn-plain")

	// 明文报到：合法的 key/token/role，但完全不走 DTLS。
	plain := dialUDPRelay(t, s.udpPort)
	sendUDPBind(t, plain, incoming.PairingKey, incoming.PairingToken, protocol.RoleAgent)
	expectUDPTimeout(t, plain, 500*time.Millisecond)
	if got := s.udpTable.Len(); got != 0 {
		t.Fatalf("明文报到不应建立会话，实际会话数 = %d", got)
	}
	if got := s.udpData.peerCount(); got != 0 {
		t.Fatalf("明文数据报不应建立 DTLS 对端，实际对端数 = %d", got)
	}

	waitFor(t, 2*time.Second, "非 DTLS 数据报 WARN", func() bool {
		return warnLines(logs, "event=udp_dtls_handshake_fail") >= 1
	})
	if !strings.Contains(logs.String(), "reason=non_dtls") {
		t.Fatalf("WARN 未标注 reason=non_dtls:\n%s", tail(logs.String(), 20))
	}

	// relay 健康：正常 DTLS 配对可用。
	pair := setupDTLSPair(t, s, "udp-svc-plain2", "conn-plain-2")
	dtlsWrite(t, pair.client, []byte("ok"))
	if got := dtlsRead(t, pair.agent, 2*time.Second); string(got) != "ok" {
		t.Fatalf("明文拒绝后 DTLS 转发异常: %q", got)
	}
}

// warnLines 统计日志中包含 sub 且级别为 WARN 的行数。
func warnLines(logs *lockedBuffer, sub string) int {
	n := 0
	for _, line := range logLinesWith(logs, sub) {
		if strings.Contains(line, "level=WARN") {
			n++
		}
	}
	return n
}

// TestUDPDTLSTamperedRecordsRejected 覆盖篡改的合法 DTLS 记录与随机字节注入：
// 不崩溃、不建立会话、WARN 限流（5s 内 1 条）、对端最终回收、relay 保持健康。
func TestUDPDTLSTamperedRecordsRejected(t *testing.T) {
	cfg := newUDPDTLSCfg(t, nil)
	cfg.Common.LogLevel = "debug"
	logs := &lockedBuffer{}
	s := startRelayWithLogs(t, cfg, logs)

	hello := captureClientHello(t)
	bad := append([]byte(nil), hello...)
	bad[len(bad)/2] ^= 0x01 // 翻转合法 ClientHello 的一位
	bad[len(bad)-1] ^= 0x80

	relayAddr, err := net.ResolveUDPAddr("udp", s.udpAddr)
	if err != nil {
		t.Fatalf("解析 relay UDP 地址: %v", err)
	}
	// 3 个不同源端口各发一份篡改记录（各自建一个握手中的对端）。
	for i := 0; i < 3; i++ {
		conn, derr := net.DialUDP("udp", nil, relayAddr)
		if derr != nil {
			t.Fatalf("建立注入 socket: %v", derr)
		}
		t.Cleanup(func() { _ = conn.Close() })
		if _, werr := conn.Write(bad); werr != nil {
			t.Fatalf("发送篡改记录: %v", werr)
		}
	}
	// 再注入随机字节（非 DTLS 记录头）。
	junk, derr := net.DialUDP("udp", nil, relayAddr)
	if derr != nil {
		t.Fatalf("建立垃圾注入 socket: %v", derr)
	}
	t.Cleanup(func() { _ = junk.Close() })
	if _, werr := junk.Write(bytes.Repeat([]byte{0x5A}, 17)); werr != nil {
		t.Fatalf("发送随机字节: %v", werr)
	}

	// 3 次握手失败 + 1 次非 DTLS：只应打出 1 条 WARN（5s 限流窗口），其余降为 DEBUG。
	waitFor(t, 2*time.Second, "首次握手失败 WARN", func() bool {
		return warnLines(logs, "event=udp_dtls_handshake_fail") >= 1
	})
	waitFor(t, 5*time.Second, "全部注入被丢弃", func() bool {
		return len(logLinesWith(logs, "event=udp_dtls_handshake_fail")) >= 4
	})
	if got := warnLines(logs, "event=udp_dtls_handshake_fail"); got != 1 {
		t.Fatalf("握手失败 WARN 数 = %d，期望 1（5s 限流）:\n%s", got, tail(logs.String(), 30))
	}
	if got := s.udpTable.Len(); got != 0 {
		t.Fatalf("注入数据不应建立会话，实际会话数 = %d", got)
	}

	// 握手中的对端在手握超时后回收（handshakeTimeout = pair_timeout = 2s）。
	waitFor(t, 6*time.Second, "注入产生的对端回收", func() bool { return s.udpData.peerCount() == 0 })

	// relay 健康：正常配对转发可用。
	pair := setupDTLSPair(t, s, "udp-svc", "conn-tamper-ok")
	dtlsWrite(t, pair.agent, []byte("healthy"))
	if got := dtlsRead(t, pair.client, 2*time.Second); string(got) != "healthy" {
		t.Fatalf("注入后 DTLS 转发异常: %q", got)
	}
}

// captureClientHello 抓取一个真实 DTLS 客户端发出的 ClientHello 数据报。
func captureClientHello(t *testing.T) []byte {
	t.Helper()
	fake, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("建立假 DTLS 服务端: %v", err)
	}
	t.Cleanup(func() { _ = fake.Close() })

	psk := bytes.Repeat([]byte{0x11}, udpdtls.KeyLen)
	conn, err := dtls.Dial("udp", fake.LocalAddr().(*net.UDPAddr), &dtls.Config{
		CipherSuites:    []dtls.CipherSuiteID{dtls.TLS_PSK_WITH_AES_128_GCM_SHA256},
		PSKIdentityHint: []byte("svc/conn"),
		PSK:             func([]byte) ([]byte, error) { return psk, nil },
	})
	if err != nil {
		t.Fatalf("建立 DTLS 客户端: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() { _ = conn.HandshakeContext(ctx) }()

	_ = fake.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 2048)
	n, _, err := fake.ReadFromUDP(buf)
	if err != nil {
		_ = conn.Close()
		t.Fatalf("抓取 ClientHello: %v", err)
	}
	_ = conn.Close()
	if n < dtlsRecordHeaderSize || !looksLikeDTLSRecord(buf[:n]) {
		t.Fatalf("抓到的数据报不像 DTLS 记录: n=%d head=% x", n, buf[:min(n, 8)])
	}
	return append([]byte(nil), buf[:n]...)
}

// TestUDPDTLSWrongTokenFails 覆盖 auth_token 不一致：PSK 不同 → 握手失败、无会话。
func TestUDPDTLSWrongTokenFails(t *testing.T) {
	cfg := newUDPDTLSCfg(t, nil)
	cfg.Common.LogLevel = "debug"
	logs := &lockedBuffer{}
	s := startRelayWithLogs(t, cfg, logs)

	agent := dialControl(t, s.tunnelAddr)
	agent.hello(protocol.RoleAgent, testToken)
	agent.register("udp-svc", "udp")
	client := dialControl(t, s.tunnelAddr)
	client.hello(protocol.RoleClient, testToken)
	issuePair(t, agent, client, "udp-svc", "conn-badtoken")
	key := pairing.PairingKey{ServiceID: "udp-svc", ClientConnID: "conn-badtoken"}

	if conn := dtlsDialRelayErr(t, s.udpAddr, key, "wrong-token"); conn != nil {
		t.Fatalf("auth_token 不一致时 DTLS 握手不应成功")
	}
	if got := s.udpTable.Len(); got != 0 {
		t.Fatalf("握手失败不应建立会话，实际会话数 = %d", got)
	}
	waitFor(t, 2*time.Second, "握手失败 WARN", func() bool {
		return warnLines(logs, "event=udp_dtls_handshake_fail") >= 1
	})
	waitFor(t, 6*time.Second, "握手失败对端回收", func() bool { return s.udpData.peerCount() == 0 })

	// relay 健康：正确 token 的配对可用。
	pair := setupDTLSPair(t, s, "udp-svc-badtoken2", "conn-badtoken-2")
	dtlsWrite(t, pair.agent, []byte("good-token"))
	if got := dtlsRead(t, pair.client, 2*time.Second); string(got) != "good-token" {
		t.Fatalf("错误 token 拒绝后 DTLS 转发异常: %q", got)
	}
}

// TestUDPDTLSDisabledStaysPlaintext 覆盖 udp_dtls = false：明文路径可用、启动 WARN、
// 且 DTLS 客户端无法握手（该 relay 根本不解析 DTLS）。
func TestUDPDTLSDisabledStaysPlaintext(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Relay.UDPListen = reserveUDPAddr(t)
		c.Relay.UDPDTLS = false
		c.Common.LogLevel = "debug"
	})
	logs := &lockedBuffer{}
	s := startRelayWithLogs(t, cfg, logs)
	if s.udpData.dtlsOn.Load() {
		t.Fatal("udp_dtls = false 时监听器不应启用 DTLS")
	}
	if !strings.Contains(logs.String(), "event=udp_dtls_disabled") {
		t.Fatalf("缺少 udp_dtls_disabled 启动 WARN:\n%s", tail(logs.String(), 20))
	}

	agent := dialControl(t, s.tunnelAddr)
	agent.hello(protocol.RoleAgent, testToken)
	agent.register("udp-svc", "udp")
	client := dialControl(t, s.tunnelAddr)
	client.hello(protocol.RoleClient, testToken)
	incoming, result := issuePair(t, agent, client, "udp-svc", "conn-off")

	agentUDP := dialUDPRelay(t, s.udpPort)
	clientUDP := dialUDPRelay(t, s.udpPort)
	sendUDPBind(t, agentUDP, incoming.PairingKey, incoming.PairingToken, protocol.RoleAgent)
	if typ, msg := readUDPFrame(t, agentUDP, 2*time.Second); typ != protocol.MsgBindOK {
		t.Fatalf("udp_dtls=false 时明文报到应成功，实际 %s: %v", typ, msg)
	}
	sendUDPBind(t, clientUDP, result.PairingKey, result.PairingToken, protocol.RoleClient)
	if typ, msg := readUDPFrame(t, clientUDP, 2*time.Second); typ != protocol.MsgBindOK {
		t.Fatalf("udp_dtls=false 时明文报到应成功，实际 %s: %v", typ, msg)
	}
	sendUDPRaw(t, clientUDP, []byte("plain-ping"))
	if got := readUDPData(t, agentUDP, 2*time.Second); string(got) != "plain-ping" {
		t.Fatalf("明文转发异常: %q", got)
	}
	if got := warnLines(logs, "event=udp_dtls_handshake_fail"); got != 0 {
		t.Fatalf("udp_dtls=false 时不应出现握手失败 WARN，实际 %d 条", got)
	}

	// 明文 relay 不接受 DTLS：客户端握手必然失败。
	key := pairing.PairingKey{ServiceID: "udp-svc", ClientConnID: "conn-off"}
	if conn := dtlsDialRelayErr(t, s.udpAddr, key, testToken); conn != nil {
		t.Fatalf("udp_dtls=false 的 relay 不应接受 DTLS 握手")
	}
}

// TestTokenRateLimiterBurstAndExpiry 覆盖 token 签发限流器的 burst、按 IP 独立、
// rate=0 不限速与表项过期（短窗口，2s 内回收）。
func TestTokenRateLimiterBurstAndExpiry(t *testing.T) {
	now := time.Unix(1700000000, 0)
	advance := func(d time.Duration) { now = now.Add(d) }

	lim := newTokenRateLimiter(10, 3)
	if lim == nil {
		t.Fatal("rate > 0 时不应返回 nil")
	}
	lim.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if !lim.allow("10.0.0.1") {
			t.Fatalf("burst 内第 %d 个请求应放行", i+1)
		}
	}
	if lim.allow("10.0.0.1") {
		t.Fatal("超过 burst 的请求应被拒绝")
	}
	if !lim.allow("10.0.0.2") {
		t.Fatal("不同来源 IP 应各自独立计数")
	}
	advance(200 * time.Millisecond) // rate=10/s → 补 2 个令牌
	if !lim.allow("10.0.0.1") {
		t.Fatal("等待后令牌应补齐")
	}
	if got := lim.entryCount(); got != 2 {
		t.Fatalf("限流表项数 = %d，期望 2", got)
	}

	// rate = 0（不限速）→ nil 限流器，永远放行且不占内存。
	if off := newTokenRateLimiter(0, 5); off != nil {
		t.Fatal("rate = 0 时应返回 nil（不限速）")
	}
	var nilLim *tokenRateLimiter
	if !nilLim.allow("10.0.0.3") {
		t.Fatal("nil 限流器应放行")
	}
	if nilLim.entryCount() != 0 || nilLim.has("10.0.0.3") {
		t.Fatal("nil 限流器不应记录任何表项")
	}

	// 过期：rate=10 / burst=1 → ttl=1s、清理间隔 1s，2s 内回收空闲表项。
	short := newTokenRateLimiter(10, 1)
	short.now = func() time.Time { return now }
	if !short.allow("10.0.0.4") {
		t.Fatal("空桶首次请求应放行")
	}
	if !short.has("10.0.0.4") {
		t.Fatal("首次请求应记录表项")
	}
	advance(2 * time.Second)
	if !short.allow("10.0.0.5") {
		t.Fatal("新 IP 应放行")
	}
	if short.has("10.0.0.4") {
		t.Fatal("空闲超过 ttl 的表项应被回收（限流表不能无界增长）")
	}
	if !short.has("10.0.0.5") {
		t.Fatal("新表项应保留")
	}
	if got := short.entryCount(); got != 1 {
		t.Fatalf("过期回收后表项数 = %d，期望 1", got)
	}
}

// TestTokenRateLimitConnectRequest 覆盖 relay 入口按来源 IP 限流：
// burst 内成功、超限返回协议错误、WARN 限流，且限流表只记 1 个来源 IP。
func TestTokenRateLimitConnectRequest(t *testing.T) {
	cfg := newUDPDTLSCfg(t, func(c *config.Config) {
		c.Relay.TokenRatePerIP = 2
		c.Relay.TokenBurstPerIP = 2
		c.Common.LogLevel = "debug"
	})
	logs := &lockedBuffer{}
	s := startRelayWithLogs(t, cfg, logs)

	agent := dialControl(t, s.tunnelAddr)
	agent.hello(protocol.RoleAgent, testToken)
	agent.register("udp-svc", "udp")
	client := dialControl(t, s.tunnelAddr)
	client.hello(protocol.RoleClient, testToken)

	for i := 0; i < 2; i++ {
		client.send(protocol.MsgConnectRequest, &protocol.ConnectRequest{
			ServiceID: "udp-svc", ClientConnID: fmt.Sprintf("conn-rate-%d", i),
		})
		if typ, msg := client.read(2 * time.Second); typ != protocol.MsgPairResult {
			t.Fatalf("burst 内第 %d 个请求期望 PairResult，实际 %s: %v", i+1, typ, msg)
		}
		if typ, msg := agent.read(2 * time.Second); typ != protocol.MsgIncoming {
			t.Fatalf("burst 内第 %d 个请求期望 Incoming，实际 %s: %v", i+1, typ, msg)
		}
	}

	// 第 3 个请求起超限：不发令牌，返回既有协议错误码。
	for i := 3; i <= 5; i++ {
		client.send(protocol.MsgConnectRequest, &protocol.ConnectRequest{
			ServiceID: "udp-svc", ClientConnID: fmt.Sprintf("conn-rate-%d", i),
		})
		typ, msg := client.read(2 * time.Second)
		if typ != protocol.MsgError {
			t.Fatalf("超限请求 %d 期望 MsgError，实际 %s: %v", i, typ, msg)
		}
		protocolErr, ok := msg.(*protocol.Error)
		if !ok {
			t.Fatalf("MsgError 类型错误: %T", msg)
		}
		if protocolErr.Code != protocol.ErrCodeTooManyPending {
			t.Fatalf("超限错误码 = %q，期望 %q", protocolErr.Code, protocol.ErrCodeTooManyPending)
		}
		if !strings.Contains(protocolErr.Message, "频繁") {
			t.Fatalf("超限错误文案异常: %q", protocolErr.Message)
		}
	}

	limiter := s.tunnel.tokenLimiter()
	if limiter == nil {
		t.Fatal("token_rate_per_ip > 0 时应启用限流器")
	}
	if got := limiter.entryCount(); got != 1 {
		t.Fatalf("限流表项数 = %d，期望 1（仅 127.0.0.1）", got)
	}
	waitFor(t, 2*time.Second, "token 限流 WARN", func() bool {
		return warnLines(logs, "event=token_rate_limited") >= 1
	})
	time.Sleep(100 * time.Millisecond)
	// 3 次超限只应打 1 条 WARN（5s 限流），其余降为 DEBUG。
	if got := warnLines(logs, "event=token_rate_limited"); got != 1 {
		t.Fatalf("token 限流 WARN 数 = %d，期望 1:\n%s", got, tail(logs.String(), 20))
	}
}

// TestTokenRateLimitDisabled 覆盖 token_rate_per_ip = 0：不限速、不建表。
func TestTokenRateLimitDisabled(t *testing.T) {
	cfg := newUDPDTLSCfg(t, func(c *config.Config) {
		c.Relay.TokenRatePerIP = 0
		c.Relay.TokenBurstPerIP = 0
	})
	s := startTestServer(t, cfg)

	agent := dialControl(t, s.tunnelAddr)
	agent.hello(protocol.RoleAgent, testToken)
	agent.register("udp-svc", "udp")
	client := dialControl(t, s.tunnelAddr)
	client.hello(protocol.RoleClient, testToken)

	for i := 0; i < 5; i++ {
		client.send(protocol.MsgConnectRequest, &protocol.ConnectRequest{
			ServiceID: "udp-svc", ClientConnID: fmt.Sprintf("conn-unlimited-%d", i),
		})
		if typ, msg := client.read(2 * time.Second); typ != protocol.MsgPairResult {
			t.Fatalf("不限速时第 %d 个请求期望 PairResult，实际 %s: %v", i+1, typ, msg)
		}
		if typ, msg := agent.read(2 * time.Second); typ != protocol.MsgIncoming {
			t.Fatalf("不限速时第 %d 个请求期望 Incoming，实际 %s: %v", i+1, typ, msg)
		}
	}
	if limiter := s.tunnel.tokenLimiter(); limiter != nil {
		t.Fatalf("token_rate_per_ip = 0 时不应构造限流器，实际表项 %d", limiter.entryCount())
	}
}
