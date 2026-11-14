package forward

import (
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"
)

// tcpPair 创建一对本地 TCP 连接（c1 为拨号端，c2 为 accept 端）。
func tcpPair(t *testing.T) (c1, c2 net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer ln.Close()

	type acceptResult struct {
		conn net.Conn
		err  error
	}
	accepted := make(chan acceptResult, 1)
	go func() {
		conn, err := ln.Accept()
		accepted <- acceptResult{conn: conn, err: err}
	}()

	c1, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	res := <-accepted
	if res.err != nil {
		t.Fatalf("accept 失败: %v", res.err)
	}
	c2 = res.conn
	t.Cleanup(func() {
		_ = c1.Close()
		_ = c2.Close()
	})
	return c1, c2
}

// waitResult 在超时时间内等待双向转发结果。
func waitResult(t *testing.T, ch <-chan [2]int64) [2]int64 {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("Bidirectional 未在 5s 内返回")
		return [2]int64{}
	}
}

func TestBidirectionalEcho(t *testing.T) {
	aRelay, aRemote := tcpPair(t)
	bRelay, bRemote := tcpPair(t)
	_ = aRemote.SetDeadline(time.Now().Add(5 * time.Second))
	_ = bRemote.SetDeadline(time.Now().Add(5 * time.Second))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan [2]int64, 1)
	go func() {
		aToB, bToA := Bidirectional(ctx, aRelay, bRelay)
		done <- [2]int64{aToB, bToA}
	}()

	if _, err := aRemote.Write([]byte("ping")); err != nil {
		t.Fatalf("写入 a 失败: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(bRemote, buf); err != nil {
		t.Fatalf("b 读取失败: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("b 收到 %q, want ping", buf)
	}

	if _, err := bRemote.Write([]byte("pong")); err != nil {
		t.Fatalf("写入 b 失败: %v", err)
	}
	if _, err := io.ReadFull(aRemote, buf); err != nil {
		t.Fatalf("a 读取失败: %v", err)
	}
	if string(buf) != "pong" {
		t.Fatalf("a 收到 %q, want pong", buf)
	}

	_ = aRemote.Close()
	_ = bRemote.Close()
	r := waitResult(t, done)
	if r[0] != 4 || r[1] != 4 {
		t.Fatalf("字节数 = a→b:%d b→a:%d, want 4/4", r[0], r[1])
	}
}

func TestBidirectionalHalfClose(t *testing.T) {
	aRelay, aRemote := tcpPair(t)
	bRelay, bRemote := tcpPair(t)
	_ = aRemote.SetDeadline(time.Now().Add(5 * time.Second))
	_ = bRemote.SetDeadline(time.Now().Add(5 * time.Second))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan [2]int64, 1)
	go func() {
		aToB, bToA := Bidirectional(ctx, aRelay, bRelay)
		done <- [2]int64{aToB, bToA}
	}()

	// a 发完数据后半关闭，b 应读到数据 + EOF，但仍可继续回写。
	if _, err := aRemote.Write([]byte("hello")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := aRemote.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("a 半关闭失败: %v", err)
	}
	got, err := io.ReadAll(bRemote)
	if err != nil {
		t.Fatalf("b 读取失败: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("b 收到 %q, want hello", got)
	}

	if _, err := bRemote.Write([]byte("bye")); err != nil {
		t.Fatalf("b 回写失败（半关闭后应仍可写）: %v", err)
	}
	_ = bRemote.Close()

	got, err = io.ReadAll(aRemote)
	if err != nil {
		t.Fatalf("a 读取失败: %v", err)
	}
	if string(got) != "bye" {
		t.Fatalf("a 收到 %q, want bye", got)
	}

	r := waitResult(t, done)
	if r[0] != 5 || r[1] != 3 {
		t.Fatalf("字节数 = a→b:%d b→a:%d, want 5/3", r[0], r[1])
	}
}

func TestBidirectionalContextCancel(t *testing.T) {
	aRelay, aRemote := tcpPair(t)
	bRelay, bRemote := tcpPair(t)
	defer aRemote.Close()
	defer bRemote.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan [2]int64, 1)
	go func() {
		aToB, bToA := Bidirectional(ctx, aRelay, bRelay)
		done <- [2]int64{aToB, bToA}
	}()

	// 两个方向都没有流量，转发 goroutine 阻塞在 Read 上。
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ctx 取消后 Bidirectional 未返回")
	}
}

func TestBidirectionalAbnormalError(t *testing.T) {
	// 一端被强制关闭，应能及时返回而不是挂住另一方向。
	aRelay, aRemote := tcpPair(t)
	bRelay, bRemote := tcpPair(t)
	defer bRemote.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan [2]int64, 1)
	go func() {
		aToB, bToA := Bidirectional(ctx, aRelay, bRelay)
		done <- [2]int64{aToB, bToA}
	}()

	_ = aRemote.Close()
	_ = bRemote.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("异常关闭后 Bidirectional 未返回")
	}
}

func TestBidirectionalNil(t *testing.T) {
	if a, b := Bidirectional(context.Background(), nil, nil); a != 0 || b != 0 {
		t.Fatalf("nil 连接应返回 0,0，实际 %d,%d", a, b)
	}
}

func TestBidirectionalNoGoroutineLeak(t *testing.T) {
	baseline := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		aRelay, aRemote := net.Pipe()
		bRelay, bRemote := net.Pipe()
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			Bidirectional(ctx, aRelay, bRelay)
		}()
		_ = aRemote.Close()
		_ = bRemote.Close()
		cancel()
		_ = aRemote.Close()
		_ = bRemote.Close()
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		if runtime.NumGoroutine() <= baseline+2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("疑似 goroutine 泄漏：baseline=%d now=%d", baseline, runtime.NumGoroutine())
}

func TestWithIdleTimeout(t *testing.T) {
	server, client := tcpPair(t)
	defer client.Close()

	idle := 80 * time.Millisecond
	wrapped := WithIdleTimeout(server, idle)
	if wrapped == nil {
		t.Fatal("WithIdleTimeout 不应返回 nil")
	}
	if WithIdleTimeout(server, 0) != server {
		t.Fatal("idle<=0 应原样返回")
	}
	if got := WithIdleTimeout(server, time.Second).(*idleConn).NetConn(); got != server {
		t.Fatal("NetConn 应返回原始连接")
	}

	// 有数据时应正常透传。
	if _, err := client.Write([]byte("x")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	buf := make([]byte, 1)
	if _, err := io.ReadFull(wrapped, buf); err != nil {
		t.Fatalf("读取失败: %v", err)
	}

	// 无数据时应因空闲超时返回。
	_, err := wrapped.Read(buf)
	if err == nil {
		t.Fatal("空闲读取应超时")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("应为超时错误，实际: %v", err)
	}
}

func TestJoinContext(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()

	ctx, cancel := context.WithCancel(context.Background())
	stop := JoinContext(ctx, a)
	cancel()

	// ctx 取消后 a 被关闭，b 的写入应失败。
	_ = b.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := b.Write([]byte("x")); err == nil {
		t.Fatal("ctx 取消后连接应被关闭")
	}
	stop()
	stop()

	// stop 先于 ctx 取消：连接不应被关闭。
	c, d := net.Pipe()
	defer c.Close()
	defer d.Close()
	ctx2, cancel2 := context.WithCancel(context.Background())
	stop2 := JoinContext(ctx2, c)
	stop2()
	cancel2()
	_ = d.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	buf := make([]byte, 1)
	_, err := d.Read(buf)
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("stop 后连接不应被关闭，期望超时，实际: %v", err)
	}

	// nil 连接不应 panic。
	if stop := JoinContext(ctx2, nil); stop == nil {
		t.Fatal("JoinContext(nil) 应返回可调用的 stop")
	}
}

func TestJoinContextNoLeak(t *testing.T) {
	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		a, b := net.Pipe()
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stop := JoinContext(ctx, a)
			if i%2 == 0 {
				cancel()
			}
			stop()
			_ = b.Close()
		}(i)
	}
	wg.Wait()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("JoinContext 疑似 goroutine 泄漏：baseline=%d now=%d", baseline, runtime.NumGoroutine())
}

// TestBidirectionalWithOutcomeIdleTimeout 覆盖 idle 超时的结束原因可被调用方识别。
//
// 场景：两端都用 WithIdleTimeout 包装、且没有任何流量 —— 两个方向的 io.Copy
// 都会因读 deadline 到期而返回超时错误；Outcome 必须把该原因暴露出来，
// 这样 relay 才能在日志里区分 idle_timeout 与正常关闭。
func TestBidirectionalWithOutcomeIdleTimeout(t *testing.T) {
	aRelay, aRemote := tcpPair(t)
	bRelay, bRemote := tcpPair(t)
	defer aRemote.Close()
	defer bRemote.Close()

	// 远端不应答：这里显式把远端读走，避免内核缓冲影响（不影响 idle 超时）。
	idle := 100 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan Outcome, 1)
	start := time.Now()
	go func() {
		done <- BidirectionalWithOutcome(ctx,
			WithIdleTimeout(aRelay, idle), WithIdleTimeout(bRelay, idle))
	}()

	var out Outcome
	select {
	case out = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("idle 超时后 BidirectionalWithOutcome 未返回")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("idle 超时耗时 %v，过长", elapsed)
	}
	if !out.TimedOut() {
		t.Fatalf("Outcome.TimedOut 应为 true，实际 AToBErr=%v BToAErr=%v", out.AToBErr, out.BToAErr)
	}
	if out.Err() == nil {
		t.Fatal("Outcome.Err 不应为 nil")
	}
	if out.AToB != 0 || out.BToA != 0 {
		t.Fatalf("无流量时字节数应为 0，实际 %d/%d", out.AToB, out.BToA)
	}
}

// TestBidirectionalWithOutcomeNormalEOF 覆盖正常 EOF 不算异常、也不算超时。
func TestBidirectionalWithOutcomeNormalEOF(t *testing.T) {
	aRelay, aRemote := tcpPair(t)
	bRelay, bRemote := tcpPair(t)
	defer aRemote.Close()
	defer bRemote.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan Outcome, 1)
	go func() {
		done <- BidirectionalWithOutcome(ctx,
			WithIdleTimeout(aRelay, 5*time.Second), WithIdleTimeout(bRelay, 5*time.Second))
	}()

	if _, err := aRemote.Write([]byte("abc")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	buf := make([]byte, 3)
	if _, err := io.ReadFull(bRemote, buf); err != nil {
		t.Fatalf("b 读取失败: %v", err)
	}
	// 两端依次关闭：应正常 EOF 结束，不产生异常/超时。
	_ = aRemote.Close()
	_ = bRemote.Close()

	select {
	case out := <-done:
		if out.TimedOut() {
			t.Fatalf("正常 EOF 不应报告超时: %+v", out)
		}
		if err := out.Err(); err != nil {
			t.Fatalf("正常 EOF 不应报告异常: %v", err)
		}
		if out.AToB != 3 {
			t.Fatalf("a→b 字节数 = %d, want 3", out.AToB)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("正常关闭后 BidirectionalWithOutcome 未返回")
	}
}

// TestBidirectionalHalfCloseThroughIdleWrapper 覆盖 WithIdleTimeout 包装后
// CloseWrite 半关闭仍然生效（半数关闭不丢数据）。
func TestBidirectionalHalfCloseThroughIdleWrapper(t *testing.T) {
	aRelay, aRemote := tcpPair(t)
	bRelay, bRemote := tcpPair(t)
	defer aRemote.Close()
	defer bRemote.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan Outcome, 1)
	go func() {
		done <- BidirectionalWithOutcome(ctx,
			WithIdleTimeout(aRelay, 5*time.Second), WithIdleTimeout(bRelay, 5*time.Second))
	}()

	if _, err := aRemote.Write([]byte("hello")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := aRemote.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("a 半关闭失败: %v", err)
	}
	got, err := io.ReadAll(bRemote)
	if err != nil {
		t.Fatalf("b 读取失败: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("b 收到 %q, want hello", got)
	}
	// a 已半关闭，b 仍能回写：证明 idleConn 的 CloseWrite 透传没有把整个连接关掉。
	if _, err := bRemote.Write([]byte("bye")); err != nil {
		t.Fatalf("b 回写失败（半关闭后应仍可写）: %v", err)
	}
	_ = bRemote.Close()
	got, err = io.ReadAll(aRemote)
	if err != nil {
		t.Fatalf("a 读取失败: %v", err)
	}
	if string(got) != "bye" {
		t.Fatalf("a 收到 %q, want bye", got)
	}

	select {
	case out := <-done:
		if out.AToB != 5 || out.BToA != 3 {
			t.Fatalf("字节数 = %d/%d, want 5/3", out.AToB, out.BToA)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("半关闭路径未在超时内返回")
	}
}
