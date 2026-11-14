package relay

import (
	"bytes"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/protocol"
)

// TestCarrierSlowSessionDoesNotBlockOthers 覆盖 A3 的核心收益：
// 一条承载连接的对端（agent）读得很慢/根本不读时，relay 的**读循环不得被阻塞**，
// 同一承载连接上的其它会话仍能正常转发。
//
// 构造：
//   - client 侧一条承载连接复用两个会话（slow / fast）；
//   - agent 侧是两个会话各自的承载连接：slow 侧把接收缓冲压到 1KB 且不读，
//     fast 侧正常读；
//   - 先用大量数据灌满 slow 侧（旧实现会在写 slow 的 Write 上阻塞读循环），
//     再从同一条 client 承载连接给 fast 发一帧，必须在 2s 内到达。
func TestCarrierSlowSessionDoesNotBlockOthers(t *testing.T) {
	cfg := testConfig(t, nil) // 不配 udp_listen → tcp_tunnel
	s := startTestServer(t, cfg)

	agent := dialControl(t, s.tunnelAddr)
	agent.hello(protocol.RoleAgent, testToken)
	agent.register("udp-svc", "udp")
	client := dialControl(t, s.tunnelAddr)
	client.hello(protocol.RoleClient, testToken)

	incSlow, resSlow := issuePair(t, agent, client, "udp-svc", "conn-slow")
	incFast, resFast := issuePair(t, agent, client, "udp-svc", "conn-fast")
	keySlow := includingKey(t, resSlow.PairingKey)
	keyFast := includingKey(t, resFast.PairingKey)

	slowAgent := openCarrier(t, s, &protocol.Bind{
		PairingKey:   incSlow.PairingKey,
		PairingToken: incSlow.PairingToken,
		Role:         protocol.RoleAgent,
		Carrier:      protocol.CarrierUDPOverTCP,
	})
	fastAgent := openCarrier(t, s, &protocol.Bind{
		PairingKey:   incFast.PairingKey,
		PairingToken: incFast.PairingToken,
		Role:         protocol.RoleAgent,
		Carrier:      protocol.CarrierUDPOverTCP,
	})

	// client 侧一条承载连接复用两个会话。
	clientCarrier := openCarrier(t, s, &protocol.Bind{
		PairingKey:   resSlow.PairingKey,
		PairingToken: resSlow.PairingToken,
		Role:         protocol.RoleClient,
		Carrier:      protocol.CarrierUDPOverTCP,
	})
	writeTunnel(t, clientCarrier, keyFast.String(), encodeBindFrame(t, &protocol.Bind{
		PairingKey:   resFast.PairingKey,
		PairingToken: resFast.PairingToken,
		Role:         protocol.RoleClient,
		Carrier:      protocol.CarrierUDPOverTCP,
	}))
	waitFor(t, 3*time.Second, "两个会话进入 Full", func() bool {
		_, _, full1 := carrierSessionState(s, keySlow)
		_, _, full2 := carrierSessionState(s, keyFast)
		return full1 && full2
	})

	// slow 侧：接收缓冲压到最小且不读，让 relay 的写很快阻塞。
	if tcpConn, ok := slowAgent.(interface{ SetReadBuffer(int) error }); ok {
		if err := tcpConn.SetReadBuffer(1024); err != nil {
			t.Logf("SetReadBuffer: %v（继续，按默认缓冲测试）", err)
		}
	}

	// 灌入约 2MB（远超内核缓冲 + 队列的瞬时排空能力），旧实现在这里就会阻塞读循环。
	payload := bytes.Repeat([]byte("x"), 32<<10)
	deadline := time.Now().Add(10 * time.Second)
	for i := 0; i < 64; i++ {
		if time.Now().After(deadline) {
			t.Fatal("写入 slow 会话数据超时（读循环疑似被阻塞）")
		}
		writeTunnel(t, clientCarrier, keySlow.String(), payload)
	}

	// 同一 client 承载连接上的 fast 会话必须仍然可用。
	writeTunnel(t, clientCarrier, keyFast.String(), []byte("fast-path"))
	session, got := readTunnel(t, fastAgent, 2*time.Second)
	if session != keyFast.String() || string(got) != "fast-path" {
		t.Fatalf("fast 会话被 slow 对端阻塞：session=%q payload=%q", session, got)
	}

	// slow 会话仍在（没有被误杀）；队列可能已丢弃部分帧，但不影响会话表。
	if _, _, full := carrierSessionState(s, keySlow); !full {
		t.Fatal("slow 会话不应因为对端慢而被关闭")
	}
}
