package agent

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// syncBuffer 是并发安全的日志缓冲（agent 的 handler 在多个 goroutine 中写日志）。
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestIncomingConcurrentLimitDrops 覆盖 SEC-D2：agent 并发处理 MsgIncoming 的上限
// （maxConcurrentIncoming=256）。超过上限时丢弃通知并记 WARN，不 panic、不无限起
// goroutine；释放槽位后新通知照常处理。
func TestIncomingConcurrentLimitDrops(t *testing.T) {
	f := newFakeRelay(t, relayOptions{
		token:         "test-token",
		udpMode:       protocol.UDPModeNative,
		enableTCPData: true,
	})
	cfg := newTestConfig(f.tunnelAddr(),
		config.ServerService{ID: "default", Proto: "tcp", Addr: startTCPEcho(t)})
	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	run := startAgentWithLogger(t, cfg, log)
	recv(t, f.registrations, "注册请求")
	waitCond(t, 5*time.Second, func() bool { return run.a.ctrlReady.Load() }, "控制连接就绪")

	// 占满全部信号量槽位，模拟"已有 256 个 MsgIncoming 正在处理"。
	for i := 0; i < maxConcurrentIncoming; i++ {
		run.a.incomingSem <- struct{}{}
	}

	if err := f.sendIncoming("default/c-drop", "tok-drop", "tcp"); err != nil {
		t.Fatalf("下发 MsgIncoming: %v", err)
	}
	waitCond(t, 3*time.Second, func() bool {
		return strings.Contains(logs.String(), "并发接入通知已达上限")
	}, "超限 WARN")
	if strings.Contains(logs.String(), "收到接入通知") {
		t.Fatalf("超限时不应处理通知:\n%s", logs.String())
	}

	// 释放一个槽位：新的通知应被正常处理（不 panic、不被丢弃）。
	<-run.a.incomingSem
	if err := f.sendIncoming("default/c-ok", "tok-ok", "tcp"); err != nil {
		t.Fatalf("下发 MsgIncoming: %v", err)
	}
	waitCond(t, 5*time.Second, func() bool {
		return strings.Contains(logs.String(), "收到接入通知")
	}, "释放槽位后处理通知")

	// 排空手动占用的槽位，避免影响后续清理。
	for i := 0; i < maxConcurrentIncoming-1; i++ {
		select {
		case <-run.a.incomingSem:
		default:
		}
	}
	if err := run.stop(t); err != nil {
		t.Fatalf("Run 应返回 nil，实际: %v", err)
	}
}
