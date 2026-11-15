package e2e

import (
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// tunnelProxy 是插在 agent/client 与 relay 隧道端口之间的最小 TCP 代理。
//
// 用途：e2e 自愈用例需要「杀掉 relay 的某条控制连接」但**不触发 MsgServerShutdown**
// （relay 优雅停机时会下发 MsgServerShutdown，agent/client 按协议主动退出，测不到重连）。
// 通过代理掐断连接，relay 侧只看到 EOF，等价于真实网络中断/relay 进程被强杀，
// 而代理本身继续监听，agent/client 的重连会照常成功。
type tunnelProxy struct {
	ln     net.Listener
	target string

	mu    sync.Mutex
	conns map[net.Conn]struct{}
	done  chan struct{}
	wg    sync.WaitGroup
}

// newTunnelProxy 启动一个指向 target 的代理。
func newTunnelProxy(t *testing.T, target string) *tunnelProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动隧道代理: %v", err)
	}
	p := &tunnelProxy{ln: ln, target: target, conns: make(map[net.Conn]struct{}), done: make(chan struct{})}
	p.wg.Add(1)
	go p.acceptLoop()
	t.Cleanup(p.close)
	return p
}

// addr 返回代理监听地址（作为 agent/client 的 relay_addr）。
func (p *tunnelProxy) addr() string { return p.ln.Addr().String() }

// cut 掐断当前所有已建立的代理连接（监听保持，后续重连可成功）。
func (p *tunnelProxy) cut(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	conns := make([]net.Conn, 0, len(p.conns))
	for c := range p.conns {
		conns = append(conns, c)
	}
	p.conns = make(map[net.Conn]struct{})
	p.mu.Unlock()
	if len(conns) == 0 {
		t.Fatalf("代理上没有可掐断的连接")
	}
	for _, c := range conns {
		_ = c.Close()
	}
}

// close 关闭监听与全部连接。
func (p *tunnelProxy) close() {
	select {
	case <-p.done:
		return
	default:
	}
	close(p.done)
	_ = p.ln.Close()
	p.mu.Lock()
	conns := make([]net.Conn, 0, len(p.conns))
	for c := range p.conns {
		conns = append(conns, c)
	}
	p.conns = make(map[net.Conn]struct{})
	p.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
	p.wg.Wait()
}

// acceptLoop 接受新连接并为每条连接建立双向转发。
func (p *tunnelProxy) acceptLoop() {
	defer p.wg.Done()
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}
		upstream, derr := net.DialTimeout("tcp", p.target, 3*time.Second)
		if derr != nil {
			_ = client.Close()
			continue
		}
		p.track(client, upstream)
		p.wg.Add(2)
		go p.pipe(upstream, client)
		go p.pipe(client, upstream)
	}
}

// track 记录一对连接，供 cut/close 关闭。
func (p *tunnelProxy) track(conns ...net.Conn) {
	p.mu.Lock()
	for _, c := range conns {
		p.conns[c] = struct{}{}
	}
	p.mu.Unlock()
}

// untrack 移除一对连接。
func (p *tunnelProxy) untrack(conns ...net.Conn) {
	p.mu.Lock()
	for _, c := range conns {
		delete(p.conns, c)
	}
	p.mu.Unlock()
}

// pipe 单向拷贝并在结束后关闭两端。
func (p *tunnelProxy) pipe(dst, src net.Conn) {
	defer p.wg.Done()
	_, _ = io.Copy(dst, src)
	_ = dst.Close()
	_ = src.Close()
	p.untrack(dst, src)
}

// TestE2EReconnectAfterControlCut 覆盖阶段 6C 的自愈 e2e：
// 掐断 agent/client 到 relay 的控制连接（模拟 relay 被杀/网络中断）后，
// 两端按指数退避自动重连并恢复注册；随后 TCP echo 与 UDP 仍可用。
func TestE2EReconnectAfterControlCut(t *testing.T) {
	echoAddr := startTCPEchoServer(t)
	udpEchoAddr := startUDPEchoServer(t, "udp:")

	relayManage := hostPort(freeTCPPort(t))
	relayCfg, _ := writeRelayINI(t, relaySpec{
		Tunnel:      hostPort(freeTCPPort(t)),
		TCP:         hostPort(freeTCPPort(t)),
		UDP:         hostPort(freeUDPPort(t)),
		Manage:      relayManage,
		PairTimeout: 5 * time.Second,
	})
	relayH := startRelay(t, relayCfg)

	// agent/client 都通过代理接入 relay 隧道，便于「掐断控制连接」。
	proxy := newTunnelProxy(t, relayCfg.Relay.TunnelListen)

	agentManage := hostPort(freeTCPPort(t))
	agentCfg, _ := writeAgentINI(t, agentSpec{
		RelayAddr: proxy.addr(),
		Manage:    agentManage,
		Protocol:  "tcp",
		Local:     echoAddr,
		Services:  []serverService{{ID: "udp-echo", Spec: "udp:" + udpEchoAddr}},
	})
	agentH := startAgent(t, agentCfg)
	waitServicesRegistered(t, relayH, relayManage, "default", "udp-echo")

	tcpLocal := hostPort(freeTCPPort(t))
	udpLocal := hostPort(freeUDPPort(t))
	clientCfg, _ := writeClientINI(t, clientSpec{
		RelayAddr: proxy.addr(),
		Manage:    hostPort(freeTCPPort(t)),
		Services: []clientService{
			{ID: "default", Listen: tcpLocal},
			{ID: "udp-echo", Listen: udpLocal},
		},
	})
	clientH := startClient(t, clientCfg)
	waitClientMappings(t, clientH, clientCfg.Common.ManageListen, map[string]string{
		"default":  tcpLocal,
		"udp-echo": udpLocal,
	})

	// 1) 掐断前：TCP echo 与 UDP echo 都可用。
	assertTCPEchoThroughClient(t, tcpLocal, "before-cut")
	udpTarget, err := net.ResolveUDPAddr("udp", udpLocal)
	if err != nil {
		t.Fatalf("解析本地 UDP 地址: %v", err)
	}
	udpClient := newUDPClient(t)
	// 首个数据报可能先于 agent 侧报到到达（半会话丢弃），UDP 允许丢包：用重试式校验。
	udpEchoEventually(t, udpClient, udpTarget, "ping-before", "udp:ping-before", 10*time.Second)

	// 2) 掐断控制连接（relay 侧只看到 EOF，不会发 MsgServerShutdown）。
	proxy.cut(t)

	// 3) relay 上服务应先消失（旧控制连接被清理），随后重连重新注册。
	waitRelayServiceCount(t, relayH, relayManage, 0)
	waitRelayAgentCount(t, relayH, relayManage, 0)
	waitRelayServiceCount(t, relayH, relayManage, 2)
	waitRelayAgentCount(t, relayH, relayManage, 1)

	// 4) 进程仍存活（重连而非退出）。
	if agentH.exited() {
		t.Fatalf("agent 不应退出（重连期间进程存活），Run 返回: %v", agentH.result())
	}
	if clientH.exited() {
		t.Fatalf("client 不应退出（重连期间进程存活），Run 返回: %v", clientH.result())
	}

	// 5) 重连后 TCP echo 与 UDP 仍可用（本地监听未重建、端口不变）。
	//
	// 注意：agent 与 client 是两套**独立**的退避循环，relay 上"服务已重新注册"
	// 只代表 agent 侧完成重连；client 侧可能还差几十~几百毫秒，此时新接入的本地
	// 连接会因"控制连接未就绪"被立即关闭（fail fast，不排队）。因此这里用有界重试
	// 等待真正可用，再用一次立即成功证明已稳定恢复。
	assertTCPEchoEventually(t, tcpLocal, "after-reconnect", 15*time.Second)
	assertTCPEchoThroughClient(t, tcpLocal, "after-reconnect-2")
	udpClient2 := newUDPClient(t)
	udpEchoEventually(t, udpClient2, udpTarget, "ping-after", "udp:ping-after", 15*time.Second)

	// 管理页可访问，且显示客户端仍在服务。
	if body := mustAdminGet(t, clientCfg.Common.ManageListen, "/client?refresh=0"); !strings.Contains(body, "default") {
		t.Fatalf("client 管理页在重连后应仍显示本地映射:\n%s", body)
	}

	stopAll(t, clientH, agentH, relayH)
}

// assertTCPEchoThroughClient 通过 client 本地端口做一次 TCP echo 往返。
func assertTCPEchoThroughClient(t *testing.T, localAddr, payload string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", localAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("连接 client 本地端口 %s: %v", localAddr, err)
	}
	defer conn.Close()
	echoText(t, conn, payload)
}

// assertTCPEchoEventually 在有界时间内重试 TCP echo，直到成功（用于等待 client
// 侧控制连接也完成重连）。超时仍未成功则 Fatal。
func assertTCPEchoEventually(t *testing.T, localAddr, payload string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		conn, err := net.DialTimeout("tcp", localAddr, time.Second)
		if err == nil {
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			_, werr := conn.Write([]byte(payload))
			if werr == nil {
				buf := make([]byte, len(payload))
				got := 0
				for got < len(payload) {
					n, rerr := conn.Read(buf[got:])
					if rerr != nil {
						lastErr = rerr
						break
					}
					got += n
				}
				if got == len(payload) && string(buf) == payload {
					_ = conn.Close()
					return
				}
			} else {
				lastErr = werr
			}
			_ = conn.Close()
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 client 本地端口 %s 恢复 TCP echo 超时: %v", localAddr, lastErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
