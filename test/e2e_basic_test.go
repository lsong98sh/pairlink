package e2e

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/protocol"
)

// TestE2EHTTPZeroChange 验收项：真实 net/http 服务零改造，经 agent 注册、
// client 本地端口访问成功（task.md §9.3 第 1 条）。
func TestE2EHTTPZeroChange(t *testing.T) {
	httpAddr := startHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-PairLink-Test", "zero-change")
		fmt.Fprintf(w, "pairlink-http-ok path=%s", r.URL.RequestURI())
	}))

	relayManage := hostPort(freeTCPPort(t))
	relayCfg, _ := writeRelayINI(t, relaySpec{
		Tunnel:      hostPort(freeTCPPort(t)),
		TCP:         hostPort(freeTCPPort(t)),
		Manage:      relayManage,
		Pprof:       false,
		PairTimeout: 5 * time.Second,
	})
	relayH := startRelay(t, relayCfg)

	agentCfg, _ := writeAgentINI(t, agentSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    hostPort(freeTCPPort(t)),
		Protocol:  "tcp",
		Local:     httpAddr,
		Services: []serverService{
			// 任务里的 client 配置使用 service.http-svc，因此 agent 也注册同名服务
			// （主服务 default 保留，指向同一个真实 HTTP 服务）。
			{ID: "http-svc", Spec: "tcp:" + httpAddr},
		},
	})
	agentH := startAgent(t, agentCfg)
	waitServicesRegistered(t, relayH, relayManage, "default", "http-svc")

	clientPort := freeTCPPort(t)
	clientCfg, _ := writeClientINI(t, clientSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    hostPort(freeTCPPort(t)),
		Services:  []clientService{{ID: "http-svc", Listen: hostPort(clientPort)}},
	})
	clientH := startClient(t, clientCfg)
	_ = agentH
	_ = clientH
	waitClientMappings(t, clientH, clientCfg.Common.ManageListen, map[string]string{
		"http-svc": hostPort(clientPort),
	})

	httpClient := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	t.Cleanup(httpClient.CloseIdleConnections)

	for _, path := range []string{"/", "/index.html?x=1"} {
		resp, err := httpClient.Get("http://" + hostPort(clientPort) + path)
		if err != nil {
			t.Fatalf("经 client 本地端口访问真实 HTTP 服务失败（path=%s）: %v", path, err)
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			t.Fatalf("读取 HTTP 响应体: %v", readErr)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("HTTP 状态码 = %d，期望 200（body=%q）", resp.StatusCode, body)
		}
		want := "pairlink-http-ok path=" + path
		if string(body) != want {
			t.Fatalf("HTTP 响应体 = %q，期望 %q", body, want)
		}
		if got := resp.Header.Get("X-PairLink-Test"); got != "zero-change" {
			t.Fatalf("HTTP 响应头 X-PairLink-Test = %q", got)
		}
		t.Logf("HTTP 200: path=%s body=%q", path, body)
	}

	// relay 侧应看到 1 个在线 agent、2 个服务；第三方的访问结束后配对表回落。
	if got := relayPage(t, relayManage); !strings.Contains(got, "服务注册表（2）") {
		t.Fatalf("relay 管理页服务注册表数量不是 2")
	}
	waitRelayPairCount(t, relayH, relayManage, 0)
}

// TestE2ETCPPairingBothOrders 验收项：注册 → ConnectRequest → PairResult/Incoming
// → 两侧 MsgBind → 双向转发，并且“agent 先到”和“client 先到”两种报到顺序都成功。
//
// 本用例直接操纵数据面连接顺序（task.md §9.3 允许），用 fake agent / fake client
// 控制面 + 原始数据面连接精确构造两种顺序。
func TestE2ETCPPairingBothOrders(t *testing.T) {
	relayManage := hostPort(freeTCPPort(t))
	relayCfg, _ := writeRelayINI(t, relaySpec{
		Tunnel:      hostPort(freeTCPPort(t)),
		TCP:         hostPort(freeTCPPort(t)),
		Manage:      relayManage,
		PairTimeout: 3 * time.Second,
	})
	relayH := startRelay(t, relayCfg)

	fakeAgent := startFakeAgent(t, relayCfg.Relay.TunnelListen, protocol.RegisterService{
		ServiceID: "echo",
		Proto:     "tcp",
		LocalHint: "127.0.0.1:9",
	})
	fakeClient := dialRawControl(t, relayCfg.Relay.TunnelListen, protocol.RoleClient, testToken)

	cases := []struct {
		name      string
		connID    string
		agentNext bool // true: agent 先报到；false: client 先报到
	}{
		{name: "client-first", connID: "order-client-first", agentNext: false},
		{name: "agent-first", connID: "order-agent-first", agentNext: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pair := fakeClient.requestPair("echo", tc.connID)
			incoming := fakeAgent.expectIncoming()
			if pair.PairingKey != incoming.PairingKey {
				t.Fatalf("PairResult.key=%q 与 Incoming.key=%q 不一致", pair.PairingKey, incoming.PairingKey)
			}
			if pair.PairingToken != incoming.PairingToken {
				t.Fatalf("两侧配对令牌不一致")
			}
			if pair.Proto != "tcp" || incoming.Proto != "tcp" {
				t.Fatalf("proto = %q/%q，期望 tcp", pair.Proto, incoming.Proto)
			}
			key := pair.PairingKey

			var clientConn, agentConn net.Conn
			if tc.agentNext {
				agentConn = bindRawData(t, relayCfg.Relay.TCPListen, key, pair.PairingToken, protocol.RoleAgent)
				go rawEchoLoop(agentConn)
				// 确认 agent 侧已占位，再让 client 报到。
				waitUntil(t, 5*time.Second, "relay 待配对表出现 agent 占位", []*runHandle{relayH}, func() bool {
					code, body, err := adminGet(relayManage, "/relay?refresh=0")
					return err == nil && code == 200 && strings.Contains(body, key) &&
						strings.Contains(body, "当前配对表（1）")
				})
				clientConn = bindRawData(t, relayCfg.Relay.TCPListen, key, pair.PairingToken, protocol.RoleClient)
			} else {
				clientConn = bindRawData(t, relayCfg.Relay.TCPListen, key, pair.PairingToken, protocol.RoleClient)
				agentConn = bindRawData(t, relayCfg.Relay.TCPListen, key, pair.PairingToken, protocol.RoleAgent)
				go rawEchoLoop(agentConn)
			}

			for i := 0; i < 3; i++ {
				payload := fmt.Sprintf("%s-payload-%d", tc.name, i)
				rawEchoRoundTrip(t, clientConn, payload)
			}
			_ = clientConn.Close()
			_ = agentConn.Close()

			waitRelayPairCount(t, relayH, relayManage, 0)
		})
	}

	// 数据面清理后，服务注册仍在、agent 仍在线。
	body := relayPage(t, relayManage)
	if !strings.Contains(body, "服务注册表（1）") || !strings.Contains(body, "在线 agent（1）") {
		t.Fatalf("配对清理后注册状态异常:\n%s", body)
	}
}

// TestE2ETCPEchoConcurrent20 验收项：20 组并发 TCP echo 互不干扰，全部关闭后
// relay 配对表回落为 0，agent/client 活跃配对表为空（task.md §9.3 第 2、4 条）。
func TestE2ETCPEchoConcurrent20(t *testing.T) {
	echoAddr := startTCPEchoServer(t)

	relayManage := hostPort(freeTCPPort(t))
	relayCfg, _ := writeRelayINI(t, relaySpec{
		Tunnel:      hostPort(freeTCPPort(t)),
		TCP:         hostPort(freeTCPPort(t)),
		Manage:      relayManage,
		PairTimeout: 5 * time.Second,
		IdleTimeout: 20 * time.Second,
	})
	relayH := startRelay(t, relayCfg)

	agentCfg, _ := writeAgentINI(t, agentSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    hostPort(freeTCPPort(t)),
		Protocol:  "tcp",
		Local:     echoAddr,
	})
	startAgent(t, agentCfg)
	waitServicesRegistered(t, relayH, relayManage, "default")

	clientPort := freeTCPPort(t)
	clientCfg, _ := writeClientINI(t, clientSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    hostPort(freeTCPPort(t)),
		Services:  []clientService{{ID: "default", Listen: hostPort(clientPort)}},
	})
	clientH := startClient(t, clientCfg)
	waitClientMappings(t, clientH, clientCfg.Common.ManageListen, map[string]string{
		"default": hostPort(clientPort),
	})

	const workers = 20
	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for i := 0; i < workers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := net.DialTimeout("tcp", hostPort(clientPort), 5*time.Second)
			if err != nil {
				errCh <- fmt.Errorf("worker %d 连接本地端口: %w", i, err)
				return
			}
			defer conn.Close()
			for round := 0; round < 2; round++ {
				payload := fmt.Sprintf("worker-%02d-round-%d-%s", i, round, strings.Repeat("x", 64))
				if err := conn.SetDeadline(time.Now().Add(8 * time.Second)); err != nil {
					errCh <- err
					return
				}
				if _, err := conn.Write([]byte(payload)); err != nil {
					errCh <- fmt.Errorf("worker %d 写: %w", i, err)
					return
				}
				buf := make([]byte, len(payload))
				if _, err := io.ReadFull(conn, buf); err != nil {
					errCh <- fmt.Errorf("worker %d 读: %w", i, err)
					return
				}
				if string(buf) != payload {
					errCh <- fmt.Errorf("worker %d 回显不一致: got=%q want=%q", i, buf, payload)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("%v", err)
	}
	if t.Failed() {
		t.FailNow()
	}

	waitRelayPairCount(t, relayH, relayManage, 0)
	waitUntil(t, 8*time.Second, "agent 活跃数据连接回落为空", nil, func() bool {
		code, body, err := adminGet(agentCfg.Common.ManageListen, "/status?refresh=0")
		return err == nil && code == 200 && strings.Contains(body, "当前没有活跃配对")
	})
	waitUntil(t, 8*time.Second, "client 活跃配对回落为空", nil, func() bool {
		code, body, err := adminGet(clientCfg.Common.ManageListen, "/client?refresh=0")
		return err == nil && code == 200 && strings.Contains(body, "当前没有活跃配对")
	})
	t.Logf("20 组并发 echo（每组 2 轮）全部回显正确，relay 配对表 = 0")
}

// TestE2EAgentMultiServiceConcurrent 验收项：同一 agent 注册至少 3 个 TCP 服务，
// client 分别监听，三个端口并发访问互不串流（task.md §9.3 第 4 条）。
func TestE2EAgentMultiServiceConcurrent(t *testing.T) {
	defaultAddr := startHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "service-default")
	}))
	webAddr := startHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "service-web")
	}))
	echoAddr := startTCPEchoServer(t)

	relayManage := hostPort(freeTCPPort(t))
	relayCfg, _ := writeRelayINI(t, relaySpec{
		Tunnel:      hostPort(freeTCPPort(t)),
		TCP:         hostPort(freeTCPPort(t)),
		Manage:      relayManage,
		PairTimeout: 5 * time.Second,
	})
	relayH := startRelay(t, relayCfg)

	agentCfg, _ := writeAgentINI(t, agentSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    hostPort(freeTCPPort(t)),
		Protocol:  "tcp",
		Local:     defaultAddr,
		Services: []serverService{
			{ID: "web", Spec: "tcp:" + webAddr},
			{ID: "echo", Spec: "tcp:" + echoAddr},
		},
	})
	startAgent(t, agentCfg)
	waitServicesRegistered(t, relayH, relayManage, "default", "web", "echo")

	ports := map[string]int{
		"default": freeTCPPort(t),
		"web":     freeTCPPort(t),
		"echo":    freeTCPPort(t),
	}
	clientCfg, _ := writeClientINI(t, clientSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    hostPort(freeTCPPort(t)),
		Services: []clientService{
			{ID: "default", Listen: hostPort(ports["default"])},
			{ID: "web", Listen: hostPort(ports["web"])},
			{ID: "echo", Listen: hostPort(ports["echo"])},
		},
	})
	clientH := startClient(t, clientCfg)
	mappings := map[string]string{}
	for id, port := range ports {
		mappings[id] = hostPort(port)
	}
	waitClientMappings(t, clientH, clientCfg.Common.ManageListen, mappings)

	httpClient := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	t.Cleanup(httpClient.CloseIdleConnections)

	const rounds = 5
	var wg sync.WaitGroup
	errCh := make(chan error, 3*rounds)
	httpCheck := func(name, url, want string) {
		defer wg.Done()
		resp, err := httpClient.Get(url)
		if err != nil {
			errCh <- fmt.Errorf("%s: %w", name, err)
			return
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			errCh <- fmt.Errorf("%s 读取: %w", name, err)
			return
		}
		if resp.StatusCode != 200 || string(body) != want {
			errCh <- fmt.Errorf("%s: status=%d body=%q want=%q", name, resp.StatusCode, body, want)
		}
	}
	for round := 0; round < rounds; round++ {
		wg.Add(2)
		go httpCheck(fmt.Sprintf("default-%d", round), "http://"+hostPort(ports["default"])+"/", "service-default")
		go httpCheck(fmt.Sprintf("web-%d", round), "http://"+hostPort(ports["web"])+"/", "service-web")

		wg.Add(1)
		go func(round int) {
			defer wg.Done()
			conn, err := net.DialTimeout("tcp", hostPort(ports["echo"]), 5*time.Second)
			if err != nil {
				errCh <- fmt.Errorf("echo-%d 连接: %w", round, err)
				return
			}
			defer conn.Close()
			payload := fmt.Sprintf("echo-service-round-%d", round)
			if err := conn.SetDeadline(time.Now().Add(8 * time.Second)); err != nil {
				errCh <- err
				return
			}
			if _, err := conn.Write([]byte(payload)); err != nil {
				errCh <- fmt.Errorf("echo-%d 写: %w", round, err)
				return
			}
			buf := make([]byte, len(payload))
			if _, err := io.ReadFull(conn, buf); err != nil {
				errCh <- fmt.Errorf("echo-%d 读: %w", round, err)
				return
			}
			if string(buf) != payload {
				errCh <- fmt.Errorf("echo-%d 串流: got=%q want=%q", round, buf, payload)
			}
		}(round)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("%v", err)
	}
	if t.Failed() {
		t.FailNow()
	}

	waitRelayPairCount(t, relayH, relayManage, 0)
	t.Logf("3 个服务 x %d 轮并发访问，内容互不串流", rounds)
}
