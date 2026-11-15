package e2e

import (
	"fmt"
	"net"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/protocol"
)

// TestE2EUDPModeNative 验收项（task.md §9.3 第 3 条 + task2 原生模式）：
// relay 配置 udp_listen 时走原生 UDP；会话映射、转发、空闲超时清理都真实发生，
// 清理后同一本地会话的报文被丢弃，新的本地源端口会重新走信号配对。
func TestE2EUDPModeNative(t *testing.T) {
	echoAddr := startUDPEchoServer(t, "echo:")

	relayManage := hostPort(freeTCPPort(t))
	relayCfg, _ := writeRelayINI(t, relaySpec{
		Tunnel:            hostPort(freeTCPPort(t)),
		TCP:               hostPort(freeTCPPort(t)),
		UDP:               hostPort(freeUDPPort(t)),
		Manage:            relayManage,
		PairTimeout:       5 * time.Second,
		UDPSessionTimeout: 800 * time.Millisecond,
	})
	relayH := startRelay(t, relayCfg)

	agentCfg, _ := writeAgentINI(t, agentSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    hostPort(freeTCPPort(t)),
		Protocol:  "udp",
		Local:     echoAddr,
	})
	startAgent(t, agentCfg)
	waitServicesRegistered(t, relayH, relayManage, "default")

	clientPort := freeUDPPort(t)
	clientCfg, _ := writeClientINI(t, clientSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    hostPort(freeTCPPort(t)),
		Services:  []clientService{{ID: "default", Listen: hostPort(clientPort)}},
	})
	clientH := startClient(t, clientCfg)
	waitClientMappings(t, clientH, clientCfg.Common.ManageListen, map[string]string{
		"default": hostPort(clientPort),
	})

	// 三处 udp_mode 协商：relay 管理页、agent 管理页、client 管理页。
	body := relayPage(t, relayManage)
	if !strings.Contains(body, `UDP 承载模式</th><td>native</td>`) {
		t.Fatalf("relay 管理页未显示 native 承载模式:\n%s", body)
	}
	agentBody := mustAdminGet(t, agentCfg.Common.ManageListen, "/status?refresh=0")
	if !strings.Contains(agentBody, "udp_mode") || !strings.Contains(agentBody, ">native<") {
		t.Fatalf("agent 管理页未显示 udp_mode=native:\n%s", agentBody)
	}
	clientBody := mustAdminGet(t, clientCfg.Common.ManageListen, "/client?refresh=0")
	if !strings.Contains(clientBody, "udp_mode") || !strings.Contains(clientBody, ">native<") {
		t.Fatalf("client 管理页未显示 udp_mode=native:\n%s", clientBody)
	}

	target, err := net.ResolveUDPAddr("udp", hostPort(clientPort))
	if err != nil {
		t.Fatalf("解析 client 本地 UDP 地址: %v", err)
	}
	c1 := newUDPClient(t)

	udpEchoEventually(t, c1, target, "probe-native-1", "echo:probe-native-1", 5*time.Second)
	waitUDPSessionCount(t, relayH, relayManage, true, 1)
	keys1 := relayUDPPairingKeys(relayPage(t, relayManage))
	if len(keys1) != 1 {
		t.Fatalf("relay 原生 UDP 会话表配对键数量 = %d，期望 1 (%v)", len(keys1), keys1)
	}
	t.Logf("原生 UDP 会话建立: pairing_key=%s", keys1[0])

	// udp_session_timeout=800ms：relay 的清理 ticker 为 1s，最多约 2s 内清空。
	waitUDPSessionCount(t, relayH, relayManage, true, 0)

	// 原会话已被 relay 清理；同一本地 UDP socket 再发数据报会被当作未知端点丢弃。
	if err := c1.SetReadDeadline(time.Now().Add(700 * time.Millisecond)); err != nil {
		t.Fatalf("设置 UDP deadline: %v", err)
	}
	if _, err := c1.WriteToUDP([]byte("after-evict"), target); err != nil {
		t.Fatalf("发送清理后数据报: %v", err)
	}
	buf := make([]byte, 1024)
	if n, _, err := c1.ReadFromUDP(buf); err == nil {
		t.Fatalf("会话清理后仍收到回包 %q（应丢弃，等待重新配对）", buf[:n])
	} else if !isTimeoutErr(err) {
		t.Fatalf("会话清理后读取出现非超时错误: %v", err)
	}
	waitUDPSessionCount(t, relayH, relayManage, true, 0)

	// 新的第三方源端口 => client 建新会话、重新走 ConnectRequest 配对。
	c2 := newUDPClient(t)
	udpEchoEventually(t, c2, target, "probe-native-2", "echo:probe-native-2", 5*time.Second)
	waitUDPSessionCount(t, relayH, relayManage, true, 1)
	keys2 := relayUDPPairingKeys(relayPage(t, relayManage))
	if len(keys2) != 1 {
		t.Fatalf("重新配对后 relay 原生 UDP 会话表配对键数量 = %d，期望 1 (%v)", len(keys2), keys2)
	}
	if keys1[0] == keys2[0] {
		t.Fatalf("重新配对后的配对键未变化: %s", keys2[0])
	}
	t.Logf("空闲清理后新源端口重新配对成功: old=%s new=%s", keys1[0], keys2[0])
}

// TestE2EUDPModeTCPTunnel 验收项（task2 的 3 条）：
// relay 不配置 udp_listen 时 udp_mode=tcp_tunnel，UDP 数据仍通；两个并发 UDP
// 会话复用同一条承载 TCP（管理页只有 2 个不同承载对端地址），互不干扰。
func TestE2EUDPModeTCPTunnel(t *testing.T) {
	echoAddr := startUDPEchoServer(t, "echo:")

	relayManage := hostPort(freeTCPPort(t))
	relayCfg, _ := writeRelayINI(t, relaySpec{
		Tunnel:      hostPort(freeTCPPort(t)),
		TCP:         hostPort(freeTCPPort(t)), // 不配置 udp_listen
		Manage:      relayManage,
		PairTimeout: 5 * time.Second,
	})
	relayH := startRelay(t, relayCfg)

	agentCfg, _ := writeAgentINI(t, agentSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    hostPort(freeTCPPort(t)),
		Protocol:  "udp",
		Local:     echoAddr,
	})
	startAgent(t, agentCfg)
	waitServicesRegistered(t, relayH, relayManage, "default")

	clientPort := freeUDPPort(t)
	clientCfg, _ := writeClientINI(t, clientSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    hostPort(freeTCPPort(t)),
		Services:  []clientService{{ID: "default", Listen: hostPort(clientPort)}},
	})
	clientH := startClient(t, clientCfg)
	waitClientMappings(t, clientH, clientCfg.Common.ManageListen, map[string]string{
		"default": hostPort(clientPort),
	})

	body := relayPage(t, relayManage)
	if !strings.Contains(body, `UDP 承载模式</th><td>tcp_tunnel</td>`) {
		t.Fatalf("relay 管理页未显示 tcp_tunnel 承载模式:\n%s", body)
	}
	if !strings.Contains(body, "未监听（复用 tcp_listen") {
		t.Fatalf("relay 管理页未说明 UDP 未监听:\n%s", body)
	}
	agentBody := mustAdminGet(t, agentCfg.Common.ManageListen, "/status?refresh=0")
	if !strings.Contains(agentBody, "udp-over-tcp") || !strings.Contains(agentBody, ">tcp_tunnel<") {
		t.Fatalf("agent 管理页未显示 tcp_tunnel / udp-over-tcp:\n%s", agentBody)
	}
	clientBody := mustAdminGet(t, clientCfg.Common.ManageListen, "/client?refresh=0")
	if !strings.Contains(clientBody, "tcp_tunnel") {
		t.Fatalf("client 管理页未显示 tcp_tunnel:\n%s", clientBody)
	}

	target, err := net.ResolveUDPAddr("udp", hostPort(clientPort))
	if err != nil {
		t.Fatalf("解析 client 本地 UDP 地址: %v", err)
	}
	c1 := newUDPClient(t)
	c2 := newUDPClient(t)

	// 两个会话并发建立并各自多次收发；任一首发丢失都通过有界重试补齐。
	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	for i, c := range []*net.UDPConn{c1, c2} {
		i, c := i, c
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := 0; round < 4; round++ {
				payload := fmt.Sprintf("tunnel-session-%d-round-%d", i+1, round)
				want := "echo:" + payload
				if err := udpEchoEventuallyErr(c, target, payload, want, 8*time.Second); err != nil {
					errCh <- fmt.Errorf("session %d: %w", i+1, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("%v", err)
	}

	waitUDPSessionCount(t, relayH, relayManage, false, 2)
	body = relayPage(t, relayManage)
	keys := relayUDPPairingKeys(body)
	if len(keys) != 2 {
		t.Fatalf("UDP over TCP 会话表配对键数量 = %d，期望 2 (%v)", len(keys), keys)
	}
	remotes := relayUDPSessionRemotes(body)
	if len(remotes) != 2 {
		t.Fatalf("两个 UDP 会话应复用各 1 条承载 TCP（agent/client 各一个对端地址），"+
			"实际不同承载对端地址 = %d: %v\n%s", len(remotes), remotes, body)
	}
	t.Logf("tcp_tunnel: 2 个并发 UDP 会话复用 1 条 agent 承载 + 1 条 client 承载，承载对端=%v", remotes)

	// agent 管理页的承载连接会话数应为 2（证明 agent 只建了一条承载连接）。
	agentBody = mustAdminGet(t, agentCfg.Common.ManageListen, "/status?refresh=0")
	carrierRow := regexp.MustCompile(`<td>udp-over-tcp</td><td class="mono">[^<]+</td><td>[^<]+</td><td>2</td>`)
	if !carrierRow.MatchString(agentBody) {
		t.Fatalf("agent 管理页未显示承载连接上的 2 个会话:\n%s", agentBody)
	}
}

// TestE2EUDPModeNegotiation 验证 udp_mode 在 HelloOK、ServiceRegistered、
// ServiceList 三处都下发，并且 native / tcp_tunnel 的数据面端口字段一致。
func TestE2EUDPModeNegotiation(t *testing.T) {
	cases := []struct {
		name   string
		want   string
		native bool
	}{
		{name: "native", want: protocol.UDPModeNative, native: true},
		{name: "tcp_tunnel", want: protocol.UDPModeTCPTunnel, native: false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			spec := relaySpec{
				Tunnel: hostPort(freeTCPPort(t)),
				TCP:    hostPort(freeTCPPort(t)),
				Manage: hostPort(freeTCPPort(t)),
			}
			if tc.native {
				spec.UDP = hostPort(freeUDPPort(t))
			}
			relayCfg, _ := writeRelayINI(t, spec)
			startRelay(t, relayCfg)

			agentCtl := dialRawControl(t, relayCfg.Relay.TunnelListen, protocol.RoleAgent, testToken)
			if agentCtl.hello.UDPMode != tc.want {
				t.Fatalf("HelloOK.udp_mode = %q，期望 %q", agentCtl.hello.UDPMode, tc.want)
			}
			if agentCtl.hello.TCPDataPort <= 0 {
				t.Fatalf("HelloOK.tcp_data_port = %d，期望 > 0", agentCtl.hello.TCPDataPort)
			}
			if tc.native && agentCtl.hello.UDPDataPort <= 0 {
				t.Fatalf("native 模式 HelloOK.udp_data_port = %d，期望 > 0", agentCtl.hello.UDPDataPort)
			}
			if !tc.native && agentCtl.hello.UDPDataPort != 0 {
				t.Fatalf("tcp_tunnel 模式 HelloOK.udp_data_port = %d，期望 0", agentCtl.hello.UDPDataPort)
			}

			agentCtl.send(protocol.MsgRegisterService, &protocol.RegisterService{
				ServiceID: "echo", Proto: "tcp",
			})
			typ, msg := agentCtl.recv(3 * time.Second)
			if typ != protocol.MsgServiceRegistered {
				t.Fatalf("注册响应 = %s，期望 service_registered", typ)
			}
			reg, ok := msg.(*protocol.ServiceRegistered)
			if !ok {
				t.Fatalf("注册响应类型 = %T", msg)
			}
			if reg.UDPMode != tc.want {
				t.Fatalf("ServiceRegistered.udp_mode = %q，期望 %q", reg.UDPMode, tc.want)
			}

			clientCtl := dialRawControl(t, relayCfg.Relay.TunnelListen, protocol.RoleClient, testToken)
			if clientCtl.hello.UDPMode != tc.want {
				t.Fatalf("client HelloOK.udp_mode = %q，期望 %q", clientCtl.hello.UDPMode, tc.want)
			}
			list := clientCtl.listServices()
			if list.UDPMode != tc.want {
				t.Fatalf("ServiceList.udp_mode = %q，期望 %q", list.UDPMode, tc.want)
			}
			t.Logf("%s: HelloOK / ServiceRegistered / ServiceList 均下发 udp_mode=%s", tc.name, tc.want)
		})
	}
}

// TestE2EUDPModeNativePlaintext 验收 v0.5.0 兼容开关：三端都写 udp_dtls = false
// 时原生 UDP 数据面回到 v0.4.0 明文路径，端到端仍然可用（同时验证三端配置一致）。
func TestE2EUDPModeNativePlaintext(t *testing.T) {
	echoAddr := startUDPEchoServer(t, "echo:")
	off := false

	relayManage := hostPort(freeTCPPort(t))
	relayCfg, _ := writeRelayINI(t, relaySpec{
		Tunnel:            hostPort(freeTCPPort(t)),
		TCP:               hostPort(freeTCPPort(t)),
		UDP:               hostPort(freeUDPPort(t)),
		Manage:            relayManage,
		PairTimeout:       5 * time.Second,
		UDPSessionTimeout: 30 * time.Second,
		UDPDTLS:           &off,
	})
	relayH := startRelay(t, relayCfg)

	agentCfg, _ := writeAgentINI(t, agentSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    hostPort(freeTCPPort(t)),
		Protocol:  "udp",
		Local:     echoAddr,
		UDPDTLS:   &off,
	})
	startAgent(t, agentCfg)
	waitServicesRegistered(t, relayH, relayManage, "default")

	clientPort := freeUDPPort(t)
	clientCfg, _ := writeClientINI(t, clientSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    hostPort(freeTCPPort(t)),
		Services:  []clientService{{ID: "default", Listen: hostPort(clientPort)}},
		UDPDTLS:   &off,
	})
	startClient(t, clientCfg)

	// 三端解析结果必须一致为 false（udp_dtls 只有完全一致才能互通）。
	if relayCfg.Relay.UDPDTLS || agentCfg.Server.UDPDTLS || clientCfg.Client.UDPDTLS {
		t.Fatalf("udp_dtls 三端解析不一致: relay=%t agent=%t client=%t",
			relayCfg.Relay.UDPDTLS, agentCfg.Server.UDPDTLS, clientCfg.Client.UDPDTLS)
	}

	target, err := net.ResolveUDPAddr("udp", hostPort(clientPort))
	if err != nil {
		t.Fatalf("解析 client 本地 UDP 地址: %v", err)
	}
	c := newUDPClient(t)
	udpEchoEventually(t, c, target, "plaintext-probe", "echo:plaintext-probe", 5*time.Second)
	waitUDPSessionCount(t, relayH, relayManage, true, 1)
	keys := relayUDPPairingKeys(relayPage(t, relayManage))
	if len(keys) != 1 {
		t.Fatalf("udp_dtls=false 时原生 UDP 会话表配对键数量 = %d，期望 1 (%v)", len(keys), keys)
	}
	t.Logf("udp_dtls=false 明文数据面端到端可用: pairing_key=%s", keys[0])
}
