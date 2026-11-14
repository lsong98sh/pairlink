//go:build soak

// Package e2e 的 soak 测试：长时间、多轮次的 TCP echo + UDP churn，
// 每轮采样 goroutine 数、RSS 与 fd 数，结束时输出斜率与峰值。
//
// 运行方式（见 README 建议文案）：
//
//	go test -tags soak ./test -run TestSoak -timeout 10m                # 短版（默认 ~2.5 分钟）
//	SOAK_DURATION=30m SOAK_ROUNDS=300 go test -tags soak ./test -run TestSoak -timeout 60m
//
// 环境变量：SOAK_ROUNDS（默认 30）、SOAK_DURATION（默认 150s）、
// SOAK_SESSIONS（每轮并发 TCP/UDP 会话数，默认 4）、SOAK_ROUND_SLEEP（覆盖每轮间隔）。
package e2e

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// soakParams 是一次 soak 的参数。
type soakParams struct {
	// Rounds 是轮数。
	Rounds int
	// Duration 是目标总时长（每轮之间补齐间隔以达到该时长）。
	Duration time.Duration
	// Sessions 是每轮并发的 TCP 会话数与 UDP 源 socket 数。
	Sessions int
	// RoundSleep 非 0 时直接使用该间隔（不做时长补齐）。
	RoundSleep time.Duration
}

// soakSample 是单轮采样。
type soakSample struct {
	Round      int
	Elapsed    time.Duration
	Goroutines int
	RSSBytes   int64
	FDs        int
	TCPOK      int
	UDPOK      int
}

// TestSoak 是可长期运行的 soak（默认短版参数，可用环境变量放大）。
func TestSoak(t *testing.T) {
	runSoak(t, soakParamsFromEnv(t))
}

// TestSoakShort 是固化的小参数版本（CI 冒烟用，几秒钟）。
func TestSoakShort(t *testing.T) {
	runSoak(t, soakParams{
		Rounds:     3,
		Duration:   6 * time.Second,
		Sessions:   2,
		RoundSleep: 500 * time.Millisecond,
	})
}

// soakParamsFromEnv 从环境变量读参数。
func soakParamsFromEnv(t *testing.T) soakParams {
	t.Helper()
	p := soakParams{Rounds: 30, Duration: 150 * time.Second, Sessions: 4}
	if v := os.Getenv("SOAK_ROUNDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("SOAK_ROUNDS = %q 非法", v)
		}
		p.Rounds = n
	}
	if v := os.Getenv("SOAK_DURATION"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			t.Fatalf("SOAK_DURATION = %q 非法", v)
		}
		p.Duration = d
	}
	if v := os.Getenv("SOAK_SESSIONS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("SOAK_SESSIONS = %q 非法", v)
		}
		p.Sessions = n
	}
	if v := os.Getenv("SOAK_ROUND_SLEEP"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			t.Fatalf("SOAK_ROUND_SLEEP = %q 非法", v)
		}
		p.RoundSleep = d
	}
	return p
}

// runSoak 启动一套真实 relay/agent/client，按参数循环并采样。
func runSoak(t *testing.T, p soakParams) {
	guardGoroutines(t)

	echoAddr := startTCPEchoServer(t)
	udpEchoAddr := startUDPEchoServer(t, "soak:")

	// 不配 udp_listen → udp_mode=tcp_tunnel，同时压到本阶段新增的承载发送队列。
	relayManage := hostPort(freeTCPPort(t))
	relayCfg, _ := writeRelayINI(t, relaySpec{
		Tunnel:            hostPort(freeTCPPort(t)),
		TCP:               hostPort(freeTCPPort(t)),
		Manage:            relayManage,
		PairTimeout:       5 * time.Second,
		UDPSessionTimeout: 2 * time.Second,
	})
	relayH := startRelay(t, relayCfg)

	agentManage := hostPort(freeTCPPort(t))
	agentCfg, _ := writeAgentINI(t, agentSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    agentManage,
		Protocol:  "tcp",
		Local:     echoAddr,
		Services: []serverService{
			{ID: "udp-echo", Spec: "udp:" + udpEchoAddr},
		},
		// 会话快速回收，制造 UDP churn（建立/清理循环）。
		UDPSessionTimeout:   2 * time.Second,
		ReconnectMaxBackoff: 5 * time.Second,
	})
	agentH := startAgent(t, agentCfg)
	waitServicesRegistered(t, relayH, relayManage, "default", "udp-echo")

	tcpLocal := hostPort(freeTCPPort(t))
	udpLocal := hostPort(freeUDPPort(t))
	clientCfg, _ := writeClientINI(t, clientSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    hostPort(freeTCPPort(t)),
		Services: []clientService{
			{ID: "default", Listen: tcpLocal},
			{ID: "udp-echo", Listen: udpLocal},
		},
		UDPSessionTimeout: 2 * time.Second,
	})
	clientH := startClient(t, clientCfg)
	waitClientMappings(t, clientH, clientCfg.Common.ManageListen, map[string]string{
		"default":  tcpLocal,
		"udp-echo": udpLocal,
	})

	udpTarget, err := net.ResolveUDPAddr("udp", udpLocal)
	if err != nil {
		t.Fatalf("解析本地 UDP 地址: %v", err)
	}

	// 预热一轮，避免把启动期内存/goroutine 计入斜率。
	runSoakRound(t, tcpLocal, udpTarget, p.Sessions)

	roundSleep := p.RoundSleep
	if roundSleep == 0 && p.Rounds > 0 && p.Duration > 0 {
		roundSleep = p.Duration / time.Duration(p.Rounds)
	}

	start := time.Now()
	samples := make([]soakSample, 0, p.Rounds)
	baseGoroutines := runtime.NumGoroutine()
	for round := 0; round < p.Rounds; round++ {
		roundStart := time.Now()
		tcpOK, udpOK := runSoakRound(t, tcpLocal, udpTarget, p.Sessions)

		runtime.GC()
		s := soakSample{
			Round:      round,
			Elapsed:    time.Since(start),
			Goroutines: runtime.NumGoroutine(),
			RSSBytes:   processRSS(),
			FDs:        processFDCount(),
			TCPOK:      tcpOK,
			UDPOK:      udpOK,
		}
		samples = append(samples, s)
		t.Logf("soak round=%d elapsed=%s tcp_ok=%d udp_ok=%d goroutines=%d rss_kb=%d fds=%d",
			s.Round, s.Elapsed.Round(time.Millisecond), s.TCPOK, s.UDPOK,
			s.Goroutines, s.RSSBytes/1024, s.FDs)

		// 服务进程必须始终存活（soak 期间不允许退出）。
		if agentH.exited() {
			t.Fatalf("第 %d 轮 agent 已退出: %v", round, agentH.result())
		}
		if clientH.exited() {
			t.Fatalf("第 %d 轮 client 已退出: %v", round, clientH.result())
		}

		if roundSleep > 0 {
			if gap := roundSleep - time.Since(roundStart); gap > 0 {
				time.Sleep(gap)
			}
		}
	}

	reportSoak(t, samples, baseGoroutines, time.Since(start))
}

// runSoakRound 执行一轮：Sessions 个并发 TCP echo 会话 + Sessions 个 UDP 源 socket churn。
func runSoakRound(t *testing.T, tcpLocal string, udpTarget *net.UDPAddr, sessions int) (tcpOK, udpOK int) {
	t.Helper()
	type result struct {
		tcp bool
		udp bool
	}
	ch := make(chan result, sessions*2)

	for i := 0; i < sessions; i++ {
		go func(i int) {
			ok := false
			conn, err := net.DialTimeout("tcp", tcpLocal, 5*time.Second)
			if err == nil {
				payload := fmt.Sprintf("soak-tcp-%d", i)
				if err := soakEcho(conn, payload); err == nil {
					ok = true
				}
				_ = conn.Close()
			}
			ch <- result{tcp: ok}
		}(i)

		go func(i int) {
			ok := false
			// 每轮新建源 socket（换源端口 = churn），关闭后由 udp_session_timeout 回收会话。
			conn, err := net.DialUDP("udp", nil, udpTarget)
			if err == nil {
				want := fmt.Sprintf("soak:soak-udp-%d", i)
				deadline := time.Now().Add(5 * time.Second)
				for attempt := 0; attempt < 3 && time.Now().Before(deadline); attempt++ {
					if _, werr := conn.Write([]byte(fmt.Sprintf("soak-udp-%d", i))); werr != nil {
						break
					}
					_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
					buf := make([]byte, 128)
					n, rerr := conn.Read(buf)
					if rerr == nil && string(buf[:n]) == want {
						ok = true
						break
					}
				}
				_ = conn.Close()
			}
			ch <- result{udp: ok}
		}(i)
	}

	for i := 0; i < sessions*2; i++ {
		r := <-ch
		if r.tcp {
			tcpOK++
		}
		if r.udp {
			udpOK++
		}
	}
	if tcpOK != sessions {
		t.Fatalf("TCP echo 成功 %d/%d", tcpOK, sessions)
	}
	if udpOK != sessions {
		t.Fatalf("UDP churn 成功 %d/%d", udpOK, sessions)
	}
	return tcpOK, udpOK
}

// soakEcho 在连接上做一次 echo 往返。
func soakEcho(conn net.Conn, payload string) error {
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	if _, err := conn.Write([]byte(payload)); err != nil {
		return err
	}
	buf := make([]byte, len(payload))
	got := 0
	for got < len(payload) {
		n, err := conn.Read(buf[got:])
		if err != nil {
			return err
		}
		got += n
	}
	if string(buf) != payload {
		return fmt.Errorf("echo 内容 %q != %q", buf, payload)
	}
	return nil
}

// reportSoak 输出斜率与峰值。
func reportSoak(t *testing.T, samples []soakSample, baseGoroutines int, total time.Duration) {
	t.Helper()
	if len(samples) == 0 {
		t.Fatal("没有采样数据")
	}
	first, last := samples[0], samples[len(samples)-1]
	peakG := first.Goroutines
	peakRSS := first.RSSBytes
	peakFD := first.FDs
	for _, s := range samples {
		if s.Goroutines > peakG {
			peakG = s.Goroutines
		}
		if s.RSSBytes > peakRSS {
			peakRSS = s.RSSBytes
		}
		if s.FDs > peakFD {
			peakFD = s.FDs
		}
	}

	minutes := total.Minutes()
	if minutes <= 0 {
		minutes = 1.0 / 60
	}
	gSlope := float64(last.Goroutines-first.Goroutines) / minutes
	rssSlopeKB := float64(last.RSSBytes-first.RSSBytes) / 1024 / minutes
	fdSlope := float64(last.FDs-first.FDs) / minutes

	t.Logf("soak summary: rounds=%d total=%s", len(samples), total.Round(time.Second))
	t.Logf("soak goroutines: start=%d end=%d peak=%d baseline=%d slope=%.2f/min",
		first.Goroutines, last.Goroutines, peakG, baseGoroutines, gSlope)
	t.Logf("soak rss: start_kb=%d end_kb=%d peak_kb=%d slope_kb=%.1f/min",
		first.RSSBytes/1024, last.RSSBytes/1024, peakRSS/1024, rssSlopeKB)
	t.Logf("soak fds: start=%d end=%d peak=%d slope=%.2f/min",
		first.FDs, last.FDs, peakFD, fdSlope)
}
