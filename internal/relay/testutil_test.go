package relay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// testToken 是测试 relay 使用的隧道接入密钥。
const testToken = "test-secret"

// testConfig 构造一份可直接交给 relay 的配置（不经过 ini 解析）。
func testConfig(t *testing.T, mutate func(*config.Config)) *config.Config {
	t.Helper()
	cfg := &config.Config{
		Common: config.CommonConfig{
			LogLevel:  "error",
			LogFormat: "text",
		},
		Relay: config.RelayConfig{
			TunnelListen:      "127.0.0.1:0",
			TCPListen:         "127.0.0.1:0",
			AuthToken:         testToken,
			PairTimeout:       2 * time.Second,
			IdleTimeout:       30 * time.Second,
			HeartbeatTimeout:  30 * time.Second,
			UDPSessionTimeout: 2 * time.Second,
			TokenTTL:          15 * time.Second,
		},
	}
	if mutate != nil {
		mutate(cfg)
	}
	return cfg
}

// startTestServer 启动一个 relay 并在测试结束时优雅关闭。
func startTestServer(t *testing.T, cfg *config.Config) *server {
	t.Helper()
	s := newServer(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- s.serve(ctx) }()

	select {
	case <-s.started:
	case err := <-errCh:
		cancel()
		t.Fatalf("relay 启动失败: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("relay 启动超时")
	}

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errCh:
			if err != nil {
				t.Errorf("relay 退出错误: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("relay 退出超时")
		}
	})
	return s
}

// ctrlConn 是测试用的控制面连接。
type ctrlConn struct {
	t    *testing.T
	conn net.Conn
}

// dialControl 连接隧道端口。
func dialControl(t *testing.T, addr string) *ctrlConn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("连接隧道 %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &ctrlConn{t: t, conn: conn}
}

// send 写一帧。
func (c *ctrlConn) send(typ protocol.MessageType, payload any) {
	c.t.Helper()
	_ = c.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if err := protocol.WriteFrame(c.conn, typ, payload); err != nil {
		c.t.Fatalf("写 %s: %v", typ, err)
	}
}

// sendHello 只发送 Hello，不读取响应。
func (c *ctrlConn) sendHello(role, token string, version int) {
	c.t.Helper()
	c.send(protocol.MsgHello, &protocol.Hello{
		Role:       role,
		Token:      token,
		Version:    version,
		ClientName: "test",
	})
}

// read 读取一帧并解码；失败直接 Fatal。
func (c *ctrlConn) read(timeout time.Duration) (protocol.MessageType, any) {
	c.t.Helper()
	typ, msg, err := c.tryRead(timeout)
	if err != nil {
		c.t.Fatalf("读帧: %v", err)
	}
	return typ, msg
}

// tryRead 读取一帧并解码；返回错误而不 Fatal，便于断言"没有消息"。
func (c *ctrlConn) tryRead(timeout time.Duration) (protocol.MessageType, any, error) {
	c.t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(timeout))
	typ, payload, err := protocol.ReadFrame(c.conn)
	if err != nil {
		return 0, nil, err
	}
	msg, err := protocol.Decode(typ, payload)
	if err != nil {
		return typ, nil, err
	}
	return typ, msg, nil
}

// hello 完成 Hello 握手并返回 HelloOK。
func (c *ctrlConn) hello(role, token string) *protocol.HelloOK {
	c.t.Helper()
	c.sendHello(role, token, protocol.ProtocolVersion)
	typ, msg := c.read(2 * time.Second)
	if typ != protocol.MsgHelloOK {
		c.t.Fatalf("期望 HelloOK，实际 %s: %v", typ, msg)
	}
	ok, isOK := msg.(*protocol.HelloOK)
	if !isOK {
		c.t.Fatalf("HelloOK 类型错误: %T", msg)
	}
	return ok
}

// expectError 断言下一帧是 MsgError 且错误码匹配。
func (c *ctrlConn) expectError(code string) *protocol.Error {
	c.t.Helper()
	typ, msg := c.read(2 * time.Second)
	if typ != protocol.MsgError {
		c.t.Fatalf("期望 MsgError，实际 %s: %v", typ, msg)
	}
	protocolErr, isErr := msg.(*protocol.Error)
	if !isErr {
		c.t.Fatalf("MsgError 类型错误: %T", msg)
	}
	if protocolErr.Code != code {
		c.t.Fatalf("期望错误码 %q，实际 %q（%s）", code, protocolErr.Code, protocolErr.Message)
	}
	return protocolErr
}

// register 注册服务并等待 MsgServiceRegistered。
func (c *ctrlConn) register(serviceID, proto string) *protocol.ServiceRegistered {
	c.t.Helper()
	c.send(protocol.MsgRegisterService, &protocol.RegisterService{
		ServiceID: serviceID,
		Proto:     proto,
		LocalHint: "127.0.0.1:1",
	})
	typ, msg := c.read(2 * time.Second)
	if typ != protocol.MsgServiceRegistered {
		c.t.Fatalf("期望 ServiceRegistered，实际 %s: %v", typ, msg)
	}
	reg, isReg := msg.(*protocol.ServiceRegistered)
	if !isReg {
		c.t.Fatalf("ServiceRegistered 类型错误: %T", msg)
	}
	return reg
}

// sync 发送一次心跳并等待 Ack，用于确保同一控制连接上先前的消息已被处理。
//
// 订阅后 relay 可能主动下发全量 ServiceList（订阅基线或 revision 断档重同步），
// 这里跳过它，保持"同步点"的语义不变。
func (c *ctrlConn) sync() {
	c.t.Helper()
	c.send(protocol.MsgHeartbeat, &protocol.Heartbeat{})
	for {
		typ, msg := c.read(2 * time.Second)
		switch typ {
		case protocol.MsgHeartbeatAck:
			return
		case protocol.MsgServiceList:
			continue
		default:
			c.t.Fatalf("期望 HeartbeatAck，实际 %s: %v", typ, msg)
		}
	}
}

// listServices 拉取服务列表。
func (c *ctrlConn) listServices() *protocol.ServiceList {
	c.t.Helper()
	c.send(protocol.MsgListServices, &protocol.ListServices{})
	typ, msg := c.read(2 * time.Second)
	if typ != protocol.MsgServiceList {
		c.t.Fatalf("期望 ServiceList，实际 %s: %v", typ, msg)
	}
	list, isList := msg.(*protocol.ServiceList)
	if !isList {
		c.t.Fatalf("ServiceList 类型错误: %T", msg)
	}
	return list
}

// issuePair 走完整信令：client 发 ConnectRequest，返回 PairResult 与 Incoming。
func issuePair(t *testing.T, agent, client *ctrlConn, serviceID, connID string) (*protocol.Incoming, *protocol.PairResult) {
	t.Helper()
	client.send(protocol.MsgConnectRequest, &protocol.ConnectRequest{
		ServiceID:    serviceID,
		ClientConnID: connID,
	})

	typ, msg := client.read(2 * time.Second)
	if typ != protocol.MsgPairResult {
		t.Fatalf("期望 PairResult，实际 %s: %v", typ, msg)
	}
	result, isResult := msg.(*protocol.PairResult)
	if !isResult {
		t.Fatalf("PairResult 类型错误: %T", msg)
	}

	typ, msg = agent.read(2 * time.Second)
	if typ != protocol.MsgIncoming {
		t.Fatalf("期望 Incoming，实际 %s: %v", typ, msg)
	}
	incoming, isIncoming := msg.(*protocol.Incoming)
	if !isIncoming {
		t.Fatalf("Incoming 类型错误: %T", msg)
	}
	return incoming, result
}

// bindData 连接 TCP 数据面并发送一帧 MsgBind；返回连接（调用方负责 Close）。
func bindData(t *testing.T, host string, port int, bind *protocol.Bind) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 2*time.Second)
	if err != nil {
		t.Fatalf("连接数据面: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if err := protocol.WriteFrame(conn, protocol.MsgBind, bind); err != nil {
		t.Fatalf("写 MsgBind: %v", err)
	}
	return conn
}

// dialRaw 建立一条裸 TCP 连接（用于测试非法首帧）。
func dialRaw(t *testing.T, host string, port int) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 2*time.Second)
	if err != nil {
		t.Fatalf("连接数据面: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// expectClosed 断言连接被对端关闭且没有收到任何业务字节。
func expectClosed(t *testing.T, conn net.Conn, timeout time.Duration) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 1)
	n, err := conn.Read(buf)
	if err == nil {
		t.Fatalf("连接应已关闭，却读到 %d 字节", n)
	}
	if n != 0 {
		t.Fatalf("连接关闭前收到 %d 字节", n)
	}
	if isTimeout(err) {
		t.Fatalf("连接未关闭（读超时）")
	}
}

// expectSilent 断言在 timeout 内连接既没有关闭也没有数据。
func expectSilent(t *testing.T, conn net.Conn, timeout time.Duration) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 1)
	n, err := conn.Read(buf)
	if err == nil {
		t.Fatalf("连接本应静默，却读到 %d 字节", n)
	}
	if n != 0 {
		t.Fatalf("连接静默期收到 %d 字节", n)
	}
	if !isTimeout(err) {
		t.Fatalf("连接本应保持打开，却返回错误: %v", err)
	}
	_ = conn.SetReadDeadline(time.Time{})
}

// isTimeout 判断错误是否为超时。
func isTimeout(err error) bool {
	type timeout interface{ Timeout() bool }
	if te, ok := err.(timeout); ok {
		return te.Timeout()
	}
	return false
}

// echoRoundTrip 通过已配对的两条数据连接做一次往返校验。
func echoRoundTrip(t *testing.T, client, agent net.Conn, payload string) {
	t.Helper()
	if err := echoRoundTripErr(client, agent, payload); err != nil {
		t.Fatalf("回显失败: %v", err)
	}
}

// echoRoundTripErr 是可在 goroutine 中使用的回显校验。
func echoRoundTripErr(client, agent net.Conn, payload string) error {
	deadline := time.Now().Add(5 * time.Second)
	_ = client.SetDeadline(deadline)
	_ = agent.SetDeadline(deadline)

	if _, err := client.Write([]byte(payload)); err != nil {
		return fmt.Errorf("client 写: %w", err)
	}
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(agent, buf); err != nil {
		return fmt.Errorf("agent 读: %w", err)
	}
	if string(buf) != payload {
		return fmt.Errorf("agent 收到 %q，期望 %q", string(buf), payload)
	}
	if _, err := agent.Write(buf); err != nil {
		return fmt.Errorf("agent 写: %w", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(client, got); err != nil {
		return fmt.Errorf("client 读: %w", err)
	}
	if string(got) != payload {
		return fmt.Errorf("client 收到 %q，期望 %q", string(got), payload)
	}
	_ = client.SetDeadline(time.Time{})
	_ = agent.SetDeadline(time.Time{})
	return nil
}

// waitFor 轮询等待条件成立。
func waitFor(t *testing.T, timeout time.Duration, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待 %s 超时", what)
}

// writeTestCert 生成一个仅用于测试的自签名证书，返回 cert/key 文件路径。
func writeTestCert(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("生成证书: %v", err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")

	certFile, err := os.Create(certPath)
	if err != nil {
		t.Fatalf("创建证书文件: %v", err)
	}
	if err := pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("写证书: %v", err)
	}
	_ = certFile.Close()

	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("编码私钥: %v", err)
	}
	keyFile, err := os.Create(keyPath)
	if err != nil {
		t.Fatalf("创建私钥文件: %v", err)
	}
	if err := pem.Encode(keyFile, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		t.Fatalf("写私钥: %v", err)
	}
	_ = keyFile.Close()
	return certPath, keyPath
}

// reserveTCPAddr 预留并立即释放一个 TCP 地址，供需要固定端口的测试使用。
func reserveTCPAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("预留 TCP 端口: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// reserveUDPAddr 预留并立即释放一个 UDP 地址。
func reserveUDPAddr(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("预留 UDP 端口: %v", err)
	}
	addr := conn.LocalAddr().String()
	_ = conn.Close()
	return addr
}

// dialTCPRetry 在超时时间内重试拨号。
func dialTCPRetry(t *testing.T, addr string, timeout time.Duration) net.Conn {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			return conn
		}
		if time.Now().After(deadline) {
			t.Fatalf("拨号 %s 超时: %v", addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// assertTCPPortFree 断言 TCP 地址可以重新监听（端口已释放）。
func assertTCPPortFree(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var lastErr error
	for {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			_ = ln.Close()
			return
		}
		lastErr = err
		if time.Now().After(deadline) {
			t.Fatalf("TCP 端口 %s 未释放: %v", addr, lastErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// assertUDPPortFree 断言 UDP 地址可以重新监听（端口已释放）。
func assertUDPPortFree(t *testing.T, addr string) {
	t.Helper()
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatalf("解析 UDP 地址 %s: %v", addr, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		conn, err := net.ListenUDP("udp", udpAddr)
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("UDP 端口 %s 未释放: %v", addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// mustPrefix 断言字符串前缀。
func mustPrefix(t *testing.T, s, prefix, what string) {
	t.Helper()
	if !strings.HasPrefix(s, prefix) {
		t.Fatalf("%s = %q，期望以 %q 开头", what, s, prefix)
	}
}
