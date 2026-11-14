package relay

import (
	"fmt"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// setupTCPPair 启动 relay 并完成一次信令，返回数据面配对信息。
func setupTCPPair(t *testing.T, cfgMutate func(*config.Config)) (*server, *ctrlConn, *ctrlConn, *protocol.Incoming, *protocol.PairResult) {
	t.Helper()
	cfg := testConfig(t, func(c *config.Config) {
		if cfgMutate != nil {
			cfgMutate(c)
		}
	})
	s := startTestServer(t, cfg)
	agent := dialControl(t, s.tunnelAddr)
	agent.hello(protocol.RoleAgent, testToken)
	agent.register("echo", "tcp")
	client := dialControl(t, s.tunnelAddr)
	client.hello(protocol.RoleClient, testToken)
	incoming, result := issuePair(t, agent, client, "echo", "conn-1")
	return s, agent, client, incoming, result
}

// bindDataRaw 是 goroutine 安全的数据面报到（不调用 t.Fatalf）。
func bindDataRaw(host string, port int, bind *protocol.Bind) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 2*time.Second)
	if err != nil {
		return nil, err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if err := protocol.WriteFrame(conn, protocol.MsgBind, bind); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// TestTCPPairAgentFirst 覆盖 agent 先报到、client 后到报到的配对与双向转发。
func TestTCPPairAgentFirst(t *testing.T) {
	s, _, _, incoming, result := setupTCPPair(t, nil)

	agentData := bindData(t, "127.0.0.1", s.tcpPort, &protocol.Bind{
		PairingKey:   incoming.PairingKey,
		PairingToken: incoming.PairingToken,
		Role:         protocol.RoleAgent,
	})
	// 先到的一侧必须保持静默（不写 BindOK、不写错误帧）。
	expectSilent(t, agentData, 250*time.Millisecond)

	clientData := bindData(t, "127.0.0.1", s.tcpPort, &protocol.Bind{
		PairingKey:   result.PairingKey,
		PairingToken: result.PairingToken,
		Role:         protocol.RoleClient,
	})
	echoRoundTrip(t, clientData, agentData, "agent-first")

	_ = clientData.Close()
	_ = agentData.Close()
	waitFor(t, 3*time.Second, "配对表清理", func() bool { return s.pairs.Len() == 0 })
}

// TestTCPPairClientFirst 覆盖 client 先报到、agent 后到报到的配对与双向转发。
func TestTCPPairClientFirst(t *testing.T) {
	s, _, _, incoming, result := setupTCPPair(t, nil)

	clientData := bindData(t, "127.0.0.1", s.tcpPort, &protocol.Bind{
		PairingKey:   result.PairingKey,
		PairingToken: result.PairingToken,
		Role:         protocol.RoleClient,
	})
	expectSilent(t, clientData, 250*time.Millisecond)

	agentData := bindData(t, "127.0.0.1", s.tcpPort, &protocol.Bind{
		PairingKey:   incoming.PairingKey,
		PairingToken: incoming.PairingToken,
		Role:         protocol.RoleAgent,
	})
	echoRoundTrip(t, clientData, agentData, "client-first")

	_ = clientData.Close()
	_ = agentData.Close()
	waitFor(t, 3*time.Second, "配对表清理", func() bool { return s.pairs.Len() == 0 })
}

// TestTCPBindRejections 覆盖令牌错误、非法首帧、非法配对键、重复侧与令牌复用。
func TestTCPBindRejections(t *testing.T) {
	s, _, _, incoming, result := setupTCPPair(t, nil)
	_ = incoming
	key := result.PairingKey
	token := result.PairingToken

	// 令牌错误。
	badToken := bindData(t, "127.0.0.1", s.tcpPort, &protocol.Bind{
		PairingKey:   key,
		PairingToken: "not-the-token",
		Role:         protocol.RoleAgent,
	})
	expectClosed(t, badToken, 2*time.Second)

	// 首帧不是 MsgBind。
	nonBind := dialRaw(t, "127.0.0.1", s.tcpPort)
	if err := protocol.WriteFrame(nonBind, protocol.MsgHeartbeat, &protocol.Heartbeat{}); err != nil {
		t.Fatalf("写非 Bind 首帧: %v", err)
	}
	expectClosed(t, nonBind, 2*time.Second)

	// 配对键非法。
	badKey := bindData(t, "127.0.0.1", s.tcpPort, &protocol.Bind{
		PairingKey:   "no-separator",
		PairingToken: token,
		Role:         protocol.RoleAgent,
	})
	expectClosed(t, badKey, 2*time.Second)

	// agent 先到并静默等待。
	agentData := bindData(t, "127.0.0.1", s.tcpPort, &protocol.Bind{
		PairingKey:   key,
		PairingToken: token,
		Role:         protocol.RoleAgent,
	})
	expectSilent(t, agentData, 150*time.Millisecond)

	// 同一侧重复报到被拒绝。
	duplicate := bindData(t, "127.0.0.1", s.tcpPort, &protocol.Bind{
		PairingKey:   key,
		PairingToken: token,
		Role:         protocol.RoleAgent,
	})
	expectClosed(t, duplicate, 2*time.Second)

	// 另一侧到达完成配对。
	clientData := bindData(t, "127.0.0.1", s.tcpPort, &protocol.Bind{
		PairingKey:   key,
		PairingToken: token,
		Role:         protocol.RoleClient,
	})
	echoRoundTrip(t, clientData, agentData, "before-replay")

	// 配对完成后（令牌已消费）用同一令牌再次报到必须被拒绝。
	replay := bindData(t, "127.0.0.1", s.tcpPort, &protocol.Bind{
		PairingKey:   key,
		PairingToken: token,
		Role:         protocol.RoleClient,
	})
	expectClosed(t, replay, 2*time.Second)

	_ = clientData.Close()
	_ = agentData.Close()
	waitFor(t, 3*time.Second, "配对表清理", func() bool { return s.pairs.Len() == 0 })

	// 转发结束后令牌仍然是已消费状态，不能复用。
	reuse := bindData(t, "127.0.0.1", s.tcpPort, &protocol.Bind{
		PairingKey:   key,
		PairingToken: token,
		Role:         protocol.RoleAgent,
	})
	expectClosed(t, reuse, 2*time.Second)
}

// TestTCPPairTimeout 覆盖只到一侧时 pair_timeout 的静默等待与清理。
func TestTCPPairTimeout(t *testing.T) {
	s, _, _, incoming, _ := setupTCPPair(t, func(c *config.Config) {
		c.Relay.PairTimeout = 300 * time.Millisecond
	})

	agentData := bindData(t, "127.0.0.1", s.tcpPort, &protocol.Bind{
		PairingKey:   incoming.PairingKey,
		PairingToken: incoming.PairingToken,
		Role:         protocol.RoleAgent,
	})
	start := time.Now()
	expectSilent(t, agentData, 100*time.Millisecond)
	expectClosed(t, agentData, 3*time.Second)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("配对超时耗时 %v，过长", elapsed)
	}

	waitFor(t, 3*time.Second, "配对表清理", func() bool { return s.pairs.Len() == 0 })
	waitFor(t, 3*time.Second, "等待信号清理", func() bool {
		s.waitMu.Lock()
		defer s.waitMu.Unlock()
		return len(s.waits) == 0
	})
}

// TestTCPPairConcurrent 覆盖同一 relay 上 20 组配对并发互不干扰。
func TestTCPPairConcurrent(t *testing.T) {
	cfg := testConfig(t, nil)
	s := startTestServer(t, cfg)

	agent := dialControl(t, s.tunnelAddr)
	agent.hello(protocol.RoleAgent, testToken)
	agent.register("echo", "tcp")
	client := dialControl(t, s.tunnelAddr)
	client.hello(protocol.RoleClient, testToken)

	const pairs = 20
	incomings := make([]*protocol.Incoming, pairs)
	results := make([]*protocol.PairResult, pairs)
	for i := 0; i < pairs; i++ {
		incomings[i], results[i] = issuePair(t, agent, client, "echo", fmt.Sprintf("conn-%02d", i))
	}

	errCh := make(chan error, pairs)
	var wg sync.WaitGroup
	for i := 0; i < pairs; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			agentConn, err := bindDataRaw("127.0.0.1", s.tcpPort, &protocol.Bind{
				PairingKey:   incomings[idx].PairingKey,
				PairingToken: incomings[idx].PairingToken,
				Role:         protocol.RoleAgent,
			})
			if err != nil {
				errCh <- fmt.Errorf("第 %d 组 agent 报到: %w", idx, err)
				return
			}
			defer agentConn.Close()

			clientConn, err := bindDataRaw("127.0.0.1", s.tcpPort, &protocol.Bind{
				PairingKey:   results[idx].PairingKey,
				PairingToken: results[idx].PairingToken,
				Role:         protocol.RoleClient,
			})
			if err != nil {
				errCh <- fmt.Errorf("第 %d 组 client 报到: %w", idx, err)
				return
			}
			defer clientConn.Close()

			if err := echoRoundTripErr(clientConn, agentConn, fmt.Sprintf("payload-%02d", idx)); err != nil {
				errCh <- fmt.Errorf("第 %d 组回显: %w", idx, err)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	waitFor(t, 5*time.Second, "并发配对清理", func() bool {
		return s.pairs.Len() == 0 && len(s.activeSnapshot()) == 0
	})
}

// TestTCPPairUnknownCarrier 覆盖未知 carrier 直接关闭。
func TestTCPPairUnknownCarrier(t *testing.T) {
	s, _, _, incoming, _ := setupTCPPair(t, nil)
	conn := bindData(t, "127.0.0.1", s.tcpPort, &protocol.Bind{
		PairingKey:   incoming.PairingKey,
		PairingToken: incoming.PairingToken,
		Role:         protocol.RoleAgent,
		Carrier:      "bogus-carrier",
	})
	expectClosed(t, conn, 2*time.Second)
}

// TestAgentGoneCleansPendingPair 覆盖 agent 控制连接断开时关闭其名下未完成配对。
func TestAgentGoneCleansPendingPair(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Relay.PairTimeout = 5 * time.Second
	})
	s := startTestServer(t, cfg)

	agent := dialControl(t, s.tunnelAddr)
	agent.hello(protocol.RoleAgent, testToken)
	agent.register("svc-gone", "tcp")

	client := dialControl(t, s.tunnelAddr)
	client.hello(protocol.RoleClient, testToken)

	incoming, _ := issuePair(t, agent, client, "svc-gone", "conn-gone")
	agentData := bindData(t, "127.0.0.1", s.tcpPort, &protocol.Bind{
		PairingKey:   incoming.PairingKey,
		PairingToken: incoming.PairingToken,
		Role:         protocol.RoleAgent,
	})
	expectSilent(t, agentData, 100*time.Millisecond)

	// 断开 agent 控制连接：既有清理路径应取消等待并关闭数据连接。
	_ = agent.conn.Close()
	expectClosed(t, agentData, 3*time.Second)

	waitFor(t, 3*time.Second, "agent 下线后的注册表与配对清理", func() bool {
		s.waitMu.Lock()
		waiting := len(s.waits)
		s.waitMu.Unlock()
		return s.agents.Count() == 0 && s.services.Len() == 0 && s.pairs.Len() == 0 && waiting == 0
	})

	// client 控制连接不受影响，服务列表已为空。
	if list := client.listServices(); len(list.Services) != 0 {
		t.Fatalf("agent 下线后服务列表应为空: %+v", list.Services)
	}
}

// TestHeartbeatTimeoutClosesAgent 覆盖心跳判死（直接驱动一次检查）。
func TestHeartbeatTimeoutClosesAgent(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Relay.HeartbeatTimeout = 100 * time.Millisecond
	})
	s := startTestServer(t, cfg)

	agent := dialControl(t, s.tunnelAddr)
	agent.hello(protocol.RoleAgent, testToken)
	agent.register("svc-heartbeat", "tcp")
	if s.agents.Count() != 1 {
		t.Fatalf("在线 agent 数 = %d", s.agents.Count())
	}

	time.Sleep(150 * time.Millisecond)
	s.checkHeartbeats()

	expectClosed(t, agent.conn, 3*time.Second)
	waitFor(t, 3*time.Second, "心跳判死后的注册表清理", func() bool {
		return s.agents.Count() == 0 && s.services.Len() == 0
	})
}
