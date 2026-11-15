package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/protocol"
)

// TestE2EFailureWrongToken 验收项：令牌错误时直接关闭数据连接，且不向客户端
// 写任何字节（TCP 数据面不能回错误帧，否则会污染透传）。
func TestE2EFailureWrongToken(t *testing.T) {
	relayManage := hostPort(freeTCPPort(t))
	relayCfg, _ := writeRelayINI(t, relaySpec{
		Tunnel:      hostPort(freeTCPPort(t)),
		TCP:         hostPort(freeTCPPort(t)),
		Manage:      relayManage,
		PairTimeout: 2 * time.Second,
	})
	relayH := startRelay(t, relayCfg)

	conn := bindRawData(t, relayCfg.Relay.TCPListen, "echo/wrong-token", "definitely-not-a-valid-token", protocol.RoleClient)
	expectClosedNoBytes(t, conn, 3*time.Second, "假令牌报到")
	_ = conn.Close()

	waitRelayPairCount(t, relayH, relayManage, 0)
	if got := relayPage(t, relayManage); !strings.Contains(got, "服务注册表（0）") {
		t.Fatalf("令牌错误不应创建服务或配对状态")
	}
	t.Log("令牌错误：连接被关闭，未收到任何字节，配对表保持 0")
}

// TestE2EFailureTokenReuse 验收项：成功配对后同一配对键/令牌再次报到被拒绝。
func TestE2EFailureTokenReuse(t *testing.T) {
	relayManage := hostPort(freeTCPPort(t))
	relayCfg, _ := writeRelayINI(t, relaySpec{
		Tunnel:      hostPort(freeTCPPort(t)),
		TCP:         hostPort(freeTCPPort(t)),
		Manage:      relayManage,
		PairTimeout: 3 * time.Second,
	})
	relayH := startRelay(t, relayCfg)
	agentCtl := startFakeAgent(t, relayCfg.Relay.TunnelListen, protocol.RegisterService{
		ServiceID: "echo", Proto: "tcp",
	})
	clientCtl := dialRawControl(t, relayCfg.Relay.TunnelListen, protocol.RoleClient, testToken)

	pair := clientCtl.requestPair("echo", "reuse-token-1")
	incoming := agentCtl.expectIncoming()
	if pair.PairingKey != incoming.PairingKey || pair.PairingToken != incoming.PairingToken {
		t.Fatalf("PairResult 与 Incoming 不一致")
	}

	clientConn := bindRawData(t, relayCfg.Relay.TCPListen, pair.PairingKey, pair.PairingToken, protocol.RoleClient)
	agentConn := bindRawData(t, relayCfg.Relay.TCPListen, pair.PairingKey, pair.PairingToken, protocol.RoleAgent)
	go rawEchoLoop(agentConn)
	rawEchoRoundTrip(t, clientConn, "reuse-first-pair-ok")
	_ = clientConn.Close()
	_ = agentConn.Close()
	waitRelayPairCount(t, relayH, relayManage, 0)

	// 首次配对已 Consume 令牌；再用同一令牌报到应被直接关闭且不写任何字节。
	replay := bindRawData(t, relayCfg.Relay.TCPListen, pair.PairingKey, pair.PairingToken, protocol.RoleClient)
	expectClosedNoBytes(t, replay, 3*time.Second, "令牌复用报到")
	_ = replay.Close()

	waitRelayPairCount(t, relayH, relayManage, 0)
	t.Logf("令牌复用被拒绝: key=%s", pair.PairingKey)
}

// TestE2EFailureTokenExpiry 验收项：令牌 TTL 到期后报到被拒绝。
func TestE2EFailureTokenExpiry(t *testing.T) {
	relayManage := hostPort(freeTCPPort(t))
	relayCfg, _ := writeRelayINI(t, relaySpec{
		Tunnel:      hostPort(freeTCPPort(t)),
		TCP:         hostPort(freeTCPPort(t)),
		Manage:      relayManage,
		PairTimeout: 5 * time.Second,
		TokenTTL:    300 * time.Millisecond,
	})
	relayH := startRelay(t, relayCfg)
	agentCtl := startFakeAgent(t, relayCfg.Relay.TunnelListen, protocol.RegisterService{
		ServiceID: "echo", Proto: "tcp",
	})
	clientCtl := dialRawControl(t, relayCfg.Relay.TunnelListen, protocol.RoleClient, testToken)

	pair := clientCtl.requestPair("echo", "expired-token-1")
	_ = agentCtl.expectIncoming()
	issuedAt := time.Now()
	time.Sleep(700 * time.Millisecond) // > token_ttl(300ms)

	agentConn := bindRawData(t, relayCfg.Relay.TCPListen, pair.PairingKey, pair.PairingToken, protocol.RoleAgent)
	expectClosedNoBytes(t, agentConn, 3*time.Second, "过期令牌（agent 侧）")
	_ = agentConn.Close()

	clientConn := bindRawData(t, relayCfg.Relay.TCPListen, pair.PairingKey, pair.PairingToken, protocol.RoleClient)
	expectClosedNoBytes(t, clientConn, 3*time.Second, "过期令牌（client 侧）")
	_ = clientConn.Close()

	waitRelayPairCount(t, relayH, relayManage, 0)
	t.Logf("令牌过期被拒绝: ttl=300ms 实际等待=%s key=%s", time.Since(issuedAt).Round(time.Millisecond), pair.PairingKey)
}

// TestE2EFailurePairTimeout 验收项：只一侧报到时，pair_timeout 后连接关闭、表项清理。
func TestE2EFailurePairTimeout(t *testing.T) {
	relayManage := hostPort(freeTCPPort(t))
	relayCfg, _ := writeRelayINI(t, relaySpec{
		Tunnel:      hostPort(freeTCPPort(t)),
		TCP:         hostPort(freeTCPPort(t)),
		Manage:      relayManage,
		PairTimeout: 400 * time.Millisecond,
	})
	relayH := startRelay(t, relayCfg)
	agentCtl := startFakeAgent(t, relayCfg.Relay.TunnelListen, protocol.RegisterService{
		ServiceID: "echo", Proto: "tcp",
	})
	clientCtl := dialRawControl(t, relayCfg.Relay.TunnelListen, protocol.RoleClient, testToken)

	pair := clientCtl.requestPair("echo", "half-pair-timeout")
	_ = agentCtl.expectIncoming()
	start := time.Now()
	clientConn := bindRawData(t, relayCfg.Relay.TCPListen, pair.PairingKey, pair.PairingToken, protocol.RoleClient)
	waitRelayPairCount(t, relayH, relayManage, 1)
	expectClosedNoBytes(t, clientConn, 4*time.Second, "只一侧报到的配对")
	elapsed := time.Since(start)
	if elapsed < 200*time.Millisecond {
		t.Fatalf("连接关闭过早（%s），pair_timeout 似乎未生效", elapsed)
	}
	_ = clientConn.Close()
	waitRelayPairCount(t, relayH, relayManage, 0)
	t.Logf("配对超时清理: pair_timeout=400ms 实际关闭耗时=%s", elapsed.Round(time.Millisecond))
}

// TestE2EFailureControlDrop 验收项：agent 控制连接断开后 relay 注销服务、
// 清理未完成配对；client 重新 ListServices 看不到该服务，新的接入请求被拒绝。
func TestE2EFailureControlDrop(t *testing.T) {
	relayManage := hostPort(freeTCPPort(t))
	relayCfg, _ := writeRelayINI(t, relaySpec{
		Tunnel:      hostPort(freeTCPPort(t)),
		TCP:         hostPort(freeTCPPort(t)),
		Manage:      relayManage,
		PairTimeout: 3 * time.Second,
	})
	relayH := startRelay(t, relayCfg)
	agentCtl := startFakeAgent(t, relayCfg.Relay.TunnelListen, protocol.RegisterService{
		ServiceID: "echo", Proto: "tcp",
	})
	waitRelayServiceCount(t, relayH, relayManage, 1)
	clientCtl := dialRawControl(t, relayCfg.Relay.TunnelListen, protocol.RoleClient, testToken)

	// 构造一个“只完成 client 侧报到”的未完成配对。
	pair := clientCtl.requestPair("echo", "control-drop-half")
	_ = agentCtl.expectIncoming()
	clientConn := bindRawData(t, relayCfg.Relay.TCPListen, pair.PairingKey, pair.PairingToken, protocol.RoleClient)
	waitRelayPairCount(t, relayH, relayManage, 1)

	// 关闭 agent 控制连接。
	agentCtl.close()

	expectClosedNoBytes(t, clientConn, 4*time.Second, "agent 控制连接断开后的未完成配对")
	_ = clientConn.Close()

	waitRelayServiceCount(t, relayH, relayManage, 0)
	waitRelayAgentCount(t, relayH, relayManage, 0)
	waitRelayPairCount(t, relayH, relayManage, 0)

	// 重新 ListServices：服务已消失。
	list := clientCtl.listServices()
	if len(list.Services) != 0 {
		t.Fatalf("agent 控制连接断开后服务列表仍有 %d 项: %+v", len(list.Services), list.Services)
	}

	// 新的接入请求应收到 service_not_found。
	clientCtl.send(protocol.MsgConnectRequest, &protocol.ConnectRequest{
		ServiceID:    "echo",
		ClientConnID: "control-drop-next",
	})
	relayErr := clientCtl.expectError()
	if relayErr.Code != protocol.ErrCodeServiceNotFound {
		t.Fatalf("新的接入请求错误码 = %q，期望 %q", relayErr.Code, protocol.ErrCodeServiceNotFound)
	}
	t.Logf("agent 控制连接断开：服务注销、未完成配对被清理，新请求错误码=%s", relayErr.Code)
}
