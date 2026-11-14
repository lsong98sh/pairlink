package client

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/logx"
)

// lockedBuffer 是并发安全的日志缓冲（slog 可能从多个 goroutine 写入）。
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

// Write 实现 io.Writer。
func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// String 返回当前日志文本。
func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// testClient 封装一次 client 运行，便于测试断言返回值与日志。
type testClient struct {
	t      *testing.T
	c      *client
	cancel context.CancelFunc
	logs   *lockedBuffer
	done   chan struct{}
	err    error
}

// testConfig 构造一份仅用于 client 的配置（默认订阅关闭、超时较短）。
func testConfig(relayAddr string) *config.Config {
	return &config.Config{
		Common: config.CommonConfig{LogLevel: "debug", LogFormat: "text"},
		Client: config.ClientConfig{
			RelayAddr:   relayAddr,
			Token:       "test-token",
			Heartbeat:   30 * time.Second,
			Subscribe:   false,
			DialTimeout: 2 * time.Second,
			PairTimeout: 2 * time.Second,
		},
	}
}

// startTestClient 在后台启动 client.run，并在测试结束时确保退出。
func startTestClient(t *testing.T, cfg *config.Config) *testClient {
	t.Helper()
	return startTestClientWith(t, cfg, nil)
}

// startTestClientWith 在启动前对 client 做一次注入（缩短重连退避、注入 listenFn 等）。
func startTestClientWith(t *testing.T, cfg *config.Config, mutate func(*client)) *testClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	logs := &lockedBuffer{}
	log := logx.New(logx.Config{Level: "debug", Format: "text", Mode: "client", Output: logs})
	c := newClientWithLogger(ctx, cfg, log)
	// 重连退避的基准等待缩短到 10ms，让重连类用例快速收敛。
	c.reconnectBase = 10 * time.Millisecond
	if mutate != nil {
		mutate(c)
	}

	tc := &testClient{t: t, c: c, cancel: cancel, logs: logs, done: make(chan struct{})}
	go func() {
		tc.err = c.run()
		close(tc.done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-tc.done:
		case <-time.After(10 * time.Second):
			t.Errorf("client.Run 在测试清理时仍未退出")
		}
		if t.Failed() {
			t.Logf("client.Run 返回: %v", tc.err)
			t.Logf("client 日志:\n%s", logs.String())
		}
	})
	return tc
}

// stop 取消 ctx 并等待 Run 返回。
func (tc *testClient) stop() error {
	tc.cancel()
	select {
	case <-tc.done:
	case <-time.After(10 * time.Second):
		tc.t.Fatalf("client.Run 未在超时内退出")
	}
	return tc.err
}

// waitResult 在 timeout 内等待 Run 返回。
func (tc *testClient) waitResult(timeout time.Duration) (error, bool) {
	select {
	case <-tc.done:
		return tc.err, true
	case <-time.After(timeout):
		return nil, false
	}
}

// requireErr 断言 Run 返回非 nil 错误。
func (tc *testClient) requireErr(timeout time.Duration) error {
	tc.t.Helper()
	err, done := tc.waitResult(timeout)
	if !done {
		tc.t.Fatalf("期望 client.Run 返回错误，但它仍在运行")
	}
	if err == nil {
		tc.t.Fatalf("期望 client.Run 返回错误，实际返回 nil")
	}
	return err
}

// requireNil 断言 Run 正常返回 nil。
func (tc *testClient) requireNil(timeout time.Duration) {
	tc.t.Helper()
	err, done := tc.waitResult(timeout)
	if !done {
		tc.t.Fatalf("期望 client.Run 退出，但它仍在运行")
	}
	if err != nil {
		tc.t.Fatalf("期望 client.Run 返回 nil，实际返回 %v", err)
	}
}

// listenerAddr 等待指定服务的本地监听就绪并返回实际地址。
func (tc *testClient) listenerAddr(serviceID string) string {
	tc.t.Helper()
	waitFor(tc.t, 5*time.Second, "本地监听 "+serviceID, func() bool {
		for _, l := range tc.c.listenerViews() {
			if l.ServiceID == serviceID {
				return true
			}
		}
		return false
	})
	for _, l := range tc.c.listenerViews() {
		if l.ServiceID == serviceID {
			return l.Addr
		}
	}
	return ""
}

// waitFor 轮询等待条件成立。
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待 %s 超时", what)
}

// logContains 断言日志里出现给定子串。
func (tc *testClient) logContains(t *testing.T, substr string) {
	t.Helper()
	if !bytes.Contains([]byte(tc.logs.String()), []byte(substr)) {
		t.Fatalf("日志中未找到 %q；日志:\n%s", substr, tc.logs.String())
	}
}
