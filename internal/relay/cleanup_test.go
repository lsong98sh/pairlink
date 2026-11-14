package relay

import (
	"context"
	"net"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// TestRunShutdownCleanup 验证 Run 返回后 goroutine 不增长、监听端口已释放。
func TestRunShutdownCleanup(t *testing.T) {
	tunnelAddr := reserveTCPAddr(t)
	tcpAddr := reserveTCPAddr(t)
	udpAddr := reserveUDPAddr(t)
	adminAddr := reserveTCPAddr(t)

	cfg := testConfig(t, func(c *config.Config) {
		c.Relay.TunnelListen = tunnelAddr
		c.Relay.TCPListen = tcpAddr
		c.Relay.UDPListen = udpAddr
		c.Common.ManageListen = adminAddr
		c.Relay.PairTimeout = 500 * time.Millisecond
	})

	runtime.GC()
	base := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- Run(ctx, cfg) }()

	tunnelConn := dialTCPRetry(t, tunnelAddr, 5*time.Second)
	agent := &ctrlConn{t: t, conn: tunnelConn}
	agent.hello(protocol.RoleAgent, testToken)
	agent.register("cleanup-svc", "tcp")

	clientConn := dialTCPRetry(t, tunnelAddr, 5*time.Second)
	client := &ctrlConn{t: t, conn: clientConn}
	client.hello(protocol.RoleClient, testToken)

	incoming, result := issuePair(t, agent, client, "cleanup-svc", "conn-clean")

	host, portText, err := net.SplitHostPort(tcpAddr)
	if err != nil {
		t.Fatalf("解析 TCP 地址: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("解析 TCP 端口: %v", err)
	}

	agentData := bindData(t, host, port, &protocol.Bind{
		PairingKey:   incoming.PairingKey,
		PairingToken: incoming.PairingToken,
		Role:         protocol.RoleAgent,
	})
	clientData := bindData(t, host, port, &protocol.Bind{
		PairingKey:   result.PairingKey,
		PairingToken: result.PairingToken,
		Role:         protocol.RoleClient,
	})
	echoRoundTrip(t, clientData, agentData, "cleanup")

	_ = clientData.Close()
	_ = agentData.Close()
	_ = client.conn.Close()
	_ = agent.conn.Close()

	cancel()
	select {
	case runErr := <-errCh:
		if runErr != nil {
			t.Fatalf("Run 返回错误: %v", runErr)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("Run 关停超时")
	}

	// 3 次采样，允许 ±2 的容差。
	maxGoroutines := 0
	for i := 0; i < 3; i++ {
		runtime.GC()
		time.Sleep(40 * time.Millisecond)
		if n := runtime.NumGoroutine(); n > maxGoroutines {
			maxGoroutines = n
		}
	}
	if maxGoroutines > base+2 {
		t.Fatalf("goroutine 泄漏：启动前 %d，关停后 %d", base, maxGoroutines)
	}

	assertTCPPortFree(t, tunnelAddr)
	assertTCPPortFree(t, tcpAddr)
	assertTCPPortFree(t, adminAddr)
	assertUDPPortFree(t, udpAddr)
}

// TestRunPortInUse 覆盖数据面端口占用时 Run 返回错误。
func TestRunPortInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占用端口: %v", err)
	}
	defer ln.Close()

	cfg := testConfig(t, func(c *config.Config) {
		c.Relay.TCPListen = ln.Addr().String()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg) }()

	select {
	case runErr := <-done:
		if runErr == nil {
			t.Fatal("端口被占用时 Run 应返回错误")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("端口占用时 Run 未及时返回")
	}
}

// TestShutdownNotifiesControl 覆盖 ctx 取消时向控制连接发送 MsgServerShutdown。
func TestShutdownNotifiesControl(t *testing.T) {
	tunnelAddr := reserveTCPAddr(t)
	tcpAddr := reserveTCPAddr(t)
	cfg := testConfig(t, func(c *config.Config) {
		c.Relay.TunnelListen = tunnelAddr
		c.Relay.TCPListen = tcpAddr
	})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- Run(ctx, cfg) }()

	conn := dialTCPRetry(t, tunnelAddr, 5*time.Second)
	ctrl := &ctrlConn{t: t, conn: conn}
	ctrl.hello(protocol.RoleAgent, testToken)

	cancel()
	typ, msg := ctrl.read(3 * time.Second)
	if typ != protocol.MsgServerShutdown {
		t.Fatalf("期望 MsgServerShutdown，实际 %s: %v", typ, msg)
	}
	if shutdown, ok := msg.(*protocol.ServerShutdown); !ok || shutdown.Reason == "" {
		t.Fatalf("ServerShutdown 内容异常: %+v", msg)
	}
	expectClosed(t, ctrl.conn, 3*time.Second)

	select {
	case runErr := <-errCh:
		if runErr != nil {
			t.Fatalf("Run 返回错误: %v", runErr)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("Run 关停超时")
	}
}
