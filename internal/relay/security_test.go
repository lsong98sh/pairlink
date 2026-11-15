package relay

import (
	"encoding/binary"
	"errors"
	"net"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// TestAuthAllowed 覆盖 SEC-A1 的纯函数鉴权矩阵。
//
// 之前的实现里 auth_token 为空 = 完全开放；现在空 token 只允许回环来源。
// 用纯函数覆盖是因为假 relay 只能产生 127.0.0.1 连接，无法在集成测试里
// 构造"非回环来源 + 空 token"。
func TestAuthAllowed(t *testing.T) {
	loop4 := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 44321}
	loop6 := &net.TCPAddr{IP: net.ParseIP("::1"), Port: 44321}
	remote4 := &net.TCPAddr{IP: net.ParseIP("203.0.113.9"), Port: 44321}
	remote6 := &net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 44321}

	cases := []struct {
		name    string
		want    string
		got     string
		remote  net.Addr
		wantErr error
	}{
		{"空token_回环IPv4_放行", "", "任意", loop4, nil},
		{"空token_回环IPv6_放行", "", "", loop6, nil},
		{"空token_非回环IPv4_拒绝", "", "任意", remote4, errAuthNonLoopbackEmptyToken},
		{"空token_非回环IPv6_拒绝", "", "任意", remote6, errAuthNonLoopbackEmptyToken},
		{"空token_无来源_拒绝", "", "任意", nil, errAuthNonLoopbackEmptyToken},
		{"有token_匹配_放行", "s3cret", "s3cret", remote4, nil},
		{"有token_不匹配_拒绝", "s3cret", "S3CRET", remote4, errAuthMismatch},
		{"有token_空值_拒绝", "s3cret", "", loop4, errAuthMismatch},
		{"有token_回环也不放行空值", "s3cret", "other", loop4, errAuthMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := authAllowed(tc.want, tc.got, tc.remote)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("应放行，实际 %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("错误 = %v，期望 %v", err, tc.wantErr)
			}
		})
	}

	// 空白 token 等价于空：仍然只允许回环。
	if err := authAllowed("   ", "x", remote4); !errors.Is(err, errAuthNonLoopbackEmptyToken) {
		t.Fatalf("空白 token + 非回环应拒绝，实际 %v", err)
	}
}

// TestDataPlaneBindOversizedHeaderRejected 是 P0-1 的数据面回归：
// 只发 5 字节帧头声明 1 MiB，relay 必须立即拒绝并关闭连接，
// 且该窗口内的内存分配 < 256 KiB（不得按声明长度预分配）。
func TestDataPlaneBindOversizedHeaderRejected(t *testing.T) {
	s := startTestServer(t, testConfig(t, nil))
	conn := dialRaw(t, "127.0.0.1", s.tcpPort)

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	var hdr [protocol.HeaderSize]byte
	hdr[0] = byte(protocol.MsgBind)
	binary.BigEndian.PutUint32(hdr[1:], protocol.MaxPayload)
	if _, err := conn.Write(hdr[:]); err != nil {
		t.Fatalf("写帧头: %v", err)
	}
	expectClosed(t, conn, 2*time.Second)

	runtime.ReadMemStats(&after)
	delta := int64(after.TotalAlloc - before.TotalAlloc)
	if delta >= 256<<10 {
		t.Fatalf("拒绝 1 MiB 声明帧期间新增分配 %d 字节，应 < 256 KiB", delta)
	}
	t.Logf("数据面拒绝 1 MiB 声明帧：新增分配 %d 字节（上限 256 KiB）", delta)

	// 拒绝之后的正常连接仍可工作（限流/拒绝没有污染监听）。
	agent := dialControl(t, s.tunnelAddr)
	agent.hello(protocol.RoleAgent, testToken)
	agent.register("tcp-svc", "tcp")
	client := dialControl(t, s.tunnelAddr)
	client.hello(protocol.RoleClient, testToken)
	incoming, result := issuePair(t, agent, client, "tcp-svc", "conn-ok")
	aConn := bindData(t, "127.0.0.1", s.tcpPort, &protocol.Bind{
		PairingKey: incoming.PairingKey, PairingToken: incoming.PairingToken, Role: protocol.RoleAgent,
	})
	cConn := bindData(t, "127.0.0.1", s.tcpPort, &protocol.Bind{
		PairingKey: result.PairingKey, PairingToken: result.PairingToken, Role: protocol.RoleClient,
	})
	echoRoundTrip(t, cConn, aConn, "after-reject")
}

// TestConnLimitMaxConns 覆盖 SEC-D2 的并发连接上限与释放。
func TestConnLimitMaxConns(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Relay.MaxConns = 2
		c.Relay.MaxConnsPerIP = 0
	})
	s := startTestServer(t, cfg)

	conns := make([]net.Conn, 0, 2)
	for i := 0; i < 2; i++ {
		conn, err := net.DialTimeout("tcp", s.tunnelAddr, 2*time.Second)
		if err != nil {
			t.Fatalf("连接 %d 失败: %v", i, err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		conns = append(conns, conn)
	}
	waitFor(t, 2*time.Second, "限流计数达到 2", func() bool { return s.conns.count() == 2 })

	// 第 3 条连接立即被关闭。
	third, err := net.DialTimeout("tcp", s.tunnelAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("第 3 条连接拨号失败: %v", err)
	}
	defer third.Close()
	expectClosed(t, third, 2*time.Second)
	if got := s.conns.count(); got != 2 {
		t.Fatalf("被拒连接不应计入计数: count=%d", got)
	}

	// 释放一条后新连接可以接入。
	_ = conns[0].Close()
	waitFor(t, 2*time.Second, "限流计数回落到 1", func() bool { return s.conns.count() == 1 })
	fourth, err := net.DialTimeout("tcp", s.tunnelAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("释放后新连接失败: %v", err)
	}
	defer fourth.Close()
	waitFor(t, 2*time.Second, "新连接被接受", func() bool { return s.conns.count() == 2 })
}

// TestConnLimitPerIP 覆盖 SEC-D2 的单 IP 并发上限（回环来源共用 127.0.0.1）。
func TestConnLimitPerIP(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Relay.MaxConns = 0
		c.Relay.MaxConnsPerIP = 1
	})
	s := startTestServer(t, cfg)

	first, err := net.DialTimeout("tcp", s.tunnelAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer first.Close()
	waitFor(t, 2*time.Second, "首个连接被计数", func() bool { return s.conns.count() == 1 })

	second, err := net.DialTimeout("tcp", s.tunnelAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("第 2 个连接拨号失败: %v", err)
	}
	defer second.Close()
	expectClosed(t, second, 2*time.Second)
}

// TestConnectRequestPendingQuota 覆盖 SEC-D2 的全局配额：
// max_pending_pairs 计入"已签发未消费的令牌"，超过即回 too_many_pending。
func TestConnectRequestPendingQuota(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Relay.MaxPendingPairs = 1
	})
	s := startTestServer(t, cfg)

	agent := dialControl(t, s.tunnelAddr)
	agent.hello(protocol.RoleAgent, testToken)
	agent.register("tcp-svc", "tcp")
	client := dialControl(t, s.tunnelAddr)
	client.hello(protocol.RoleClient, testToken)

	issuePair(t, agent, client, "tcp-svc", "conn-1")
	// 第 2 个请求：tokens.Len() = 1 >= 1 → 拒绝。
	client.send(protocol.MsgConnectRequest, &protocol.ConnectRequest{
		ServiceID: "tcp-svc", ClientConnID: "conn-2",
	})
	protocolErr := client.expectError(protocol.ErrCodeTooManyPending)
	if protocolErr.Message == "" {
		t.Error("too_many_pending 应带可读消息")
	}
	if got := s.tokens.Len(); got != 1 {
		t.Fatalf("被拒绝的请求不应签发令牌，tokens.Len=%d", got)
	}
}

// TestConnectRequestPerConnQuota 覆盖 SEC-D2 的单连接配额（默认 64）。
func TestConnectRequestPerConnQuota(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Relay.MaxPendingPairs = 0 // 不限制全局，单独验证单连接配额
	})
	s := startTestServer(t, cfg)

	agent := dialControl(t, s.tunnelAddr)
	agent.hello(protocol.RoleAgent, testToken)
	agent.register("tcp-svc", "tcp")
	client := dialControl(t, s.tunnelAddr)
	client.hello(protocol.RoleClient, testToken)

	for i := 0; i < maxPendingPerConn; i++ {
		client.send(protocol.MsgConnectRequest, &protocol.ConnectRequest{
			ServiceID:    "tcp-svc",
			ClientConnID: pairingClientConnID(i),
		})
		typ, msg := client.read(2 * time.Second)
		if typ != protocol.MsgPairResult {
			t.Fatalf("第 %d 个请求期望 PairResult，实际 %s: %v", i, typ, msg)
		}
		// 每条通知都要从 agent 侧读走，避免 agent 写队列堆积。
		if atyp, _ := agent.read(2 * time.Second); atyp != protocol.MsgIncoming {
			t.Fatalf("第 %d 个请求 agent 侧期望 Incoming，实际 %s", i, atyp)
		}
	}

	// 第 65 个请求：单连接未完成请求数达到上限。
	client.send(protocol.MsgConnectRequest, &protocol.ConnectRequest{
		ServiceID: "tcp-svc", ClientConnID: "conn-over",
	})
	client.expectError(protocol.ErrCodeTooManyPending)
	if got := s.tokens.Len(); got != maxPendingPerConn {
		t.Fatalf("令牌数 = %d，期望 %d（第 65 个不应签发）", got, maxPendingPerConn)
	}
}

// pairingClientConnID 生成合法的 client_conn_id（与 pairing 的校验规则一致）。
func pairingClientConnID(i int) string {
	return "conn-" + strconv.Itoa(i)
}

// TestCarrierReadTimeout 覆盖 SEC-M4：承载连接只发半帧时按 deadline 关闭并清理会话。
func TestCarrierReadTimeout(t *testing.T) {
	s := startTestServer(t, testConfig(t, nil))
	if s.udpMode != protocol.UDPModeTCPTunnel {
		t.Fatalf("udp_mode = %q，期望 tcp_tunnel", s.udpMode)
	}
	// 测试内缩短单帧读超时（生产值 max(30s, 2×udp_session_timeout)）。
	s.carrierTimeout.Store(int64(300 * time.Millisecond))

	agent := dialControl(t, s.tunnelAddr)
	agent.hello(protocol.RoleAgent, testToken)
	agent.register("udp-svc", "udp")
	client := dialControl(t, s.tunnelAddr)
	client.hello(protocol.RoleClient, testToken)
	inc, res := issuePair(t, agent, client, "udp-svc", "conn-timeout")
	key := includingKey(t, res.PairingKey)

	agentCarrier := openCarrier(t, s, &protocol.Bind{
		PairingKey: inc.PairingKey, PairingToken: inc.PairingToken,
		Role: protocol.RoleAgent, Carrier: protocol.CarrierUDPOverTCP,
	})
	clientCarrier := openCarrier(t, s, &protocol.Bind{
		PairingKey: res.PairingKey, PairingToken: res.PairingToken,
		Role: protocol.RoleClient, Carrier: protocol.CarrierUDPOverTCP,
	})
	writeTunnel(t, agentCarrier, key.String(), encodeBindFrame(t, &protocol.Bind{
		PairingKey: inc.PairingKey, PairingToken: inc.PairingToken,
		Role: protocol.RoleAgent, Carrier: protocol.CarrierUDPOverTCP,
	}))
	writeTunnel(t, clientCarrier, key.String(), encodeBindFrame(t, &protocol.Bind{
		PairingKey: res.PairingKey, PairingToken: res.PairingToken,
		Role: protocol.RoleClient, Carrier: protocol.CarrierUDPOverTCP,
	}))
	waitFor(t, 2*time.Second, "会话进入 Full", func() bool {
		agentReady, clientReady, full := carrierSessionState(s, key)
		return agentReady && clientReady && full
	})

	// 客户端承载连接持续发数据帧，刷新 relay 侧读 deadline：确保只有
	// 发半帧的 agent 承载连接超时，而不是"两边都静默"造成的假通过。
	stopKeepalive := make(chan struct{})
	keepaliveDone := make(chan struct{})
	go func() {
		defer close(keepaliveDone)
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stopKeepalive:
				return
			case <-tick.C:
				// 用未登记的会话标识：relay 会读取并丢弃该帧（刷新 deadline），
				// 但不会转发给 agent 连接，避免干扰"agent 连接应被关闭"的断言。
				_ = clientCarrier.SetWriteDeadline(time.Now().Add(time.Second))
				_ = protocol.WriteUDPTunnelFrame(clientCarrier, key.String()+"-keep", []byte("keepalive"))
			}
		}
	}()

	// 只写 4 字节长度前缀 + 1 字节会话长度，后续 payload 永远不发。
	if _, err := agentCarrier.Write([]byte{0x00, 0x00, 0x00, 0x20, 0x01}); err != nil {
		t.Fatalf("写半帧失败: %v", err)
	}
	expectClosed(t, agentCarrier, 3*time.Second)
	waitFor(t, 3*time.Second, "承载超时后会话清理", func() bool {
		agentReady, clientReady, full := carrierSessionState(s, key)
		return !agentReady && !clientReady && !full
	})
	close(stopKeepalive)
	<-keepaliveDone
	if n := s.carriers.len(); n != 0 {
		t.Fatalf("承载会话应全部清理，剩余会话数 = %d", n)
	}
	// 只有超时的那条承载连接被关闭：对端承载连接保持打开（静默而非 EOF），
	// 且仍可写入（会话已删除，数据帧被安全丢弃）。
	expectSilent(t, clientCarrier, 150*time.Millisecond)
	writeTunnel(t, clientCarrier, key.String(), []byte("still-open"))
}

// TestCarrierDuplicateBindNotForwarded 覆盖 P2-7：已登记会话重复发送 MsgBind 帧时
// 必须丢弃，不能当作数据转发给对端。
func TestCarrierDuplicateBindNotForwarded(t *testing.T) {
	s := startTestServer(t, testConfig(t, nil))
	agent := dialControl(t, s.tunnelAddr)
	agent.hello(protocol.RoleAgent, testToken)
	agent.register("udp-svc", "udp")
	client := dialControl(t, s.tunnelAddr)
	client.hello(protocol.RoleClient, testToken)
	inc, res := issuePair(t, agent, client, "udp-svc", "conn-dup")
	key := includingKey(t, res.PairingKey)

	agentCarrier := openCarrier(t, s, &protocol.Bind{
		PairingKey: inc.PairingKey, PairingToken: inc.PairingToken,
		Role: protocol.RoleAgent, Carrier: protocol.CarrierUDPOverTCP,
	})
	clientCarrier := openCarrier(t, s, &protocol.Bind{
		PairingKey: res.PairingKey, PairingToken: res.PairingToken,
		Role: protocol.RoleClient, Carrier: protocol.CarrierUDPOverTCP,
	})
	bindFrame := encodeBindFrame(t, &protocol.Bind{
		PairingKey: inc.PairingKey, PairingToken: inc.PairingToken,
		Role: protocol.RoleAgent, Carrier: protocol.CarrierUDPOverTCP,
	})
	writeTunnel(t, agentCarrier, key.String(), bindFrame)
	writeTunnel(t, clientCarrier, key.String(), encodeBindFrame(t, &protocol.Bind{
		PairingKey: res.PairingKey, PairingToken: res.PairingToken,
		Role: protocol.RoleClient, Carrier: protocol.CarrierUDPOverTCP,
	}))
	waitFor(t, 2*time.Second, "会话进入 Full", func() bool {
		agentReady, clientReady, full := carrierSessionState(s, key)
		return agentReady && clientReady && full
	})

	// 已登记会话再收到同一 MsgBind：丢弃，client 侧不应收到任何字节。
	writeTunnel(t, agentCarrier, key.String(), bindFrame)
	expectSilent(t, clientCarrier, 500*time.Millisecond)

	// 正常数据帧仍然转发（证明丢弃逻辑没有影响数据面）。
	writeTunnel(t, agentCarrier, key.String(), []byte("after-dup"))
	if session, payload := readTunnel(t, clientCarrier, 2*time.Second); session != key.String() || string(payload) != "after-dup" {
		t.Fatalf("正常数据帧转发失败: session=%q payload=%q", session, string(payload))
	}
}
