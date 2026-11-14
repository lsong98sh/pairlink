// Package forward 提供双向字节流转发与超时辅助。
//
// Bidirectional 用两个 goroutine 各跑一个方向的 io.Copy：
// 任一方向结束后对对端调用 CloseWrite 形成半关闭，另一方向继续排空；
// ctx 取消时立即 Close 两端；异常错误时关闭两端以避免悬挂。
// 函数返回前保证两个拷贝 goroutine 与 context 看护 goroutine 都已退出，
// 因此不会泄漏 goroutine。
package forward

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

// closeWriter 是支持半关闭的连接（*net.TCPConn 等）。
type closeWriter interface {
	CloseWrite() error
}

// Outcome 是一次双向转发的统计与结束原因。
//
// 新增于阶段 4：Bidirectional 只返回字节数，调用方无法区分"正常 EOF 结束"
// 与"空闲超时（WithIdleTimeout）/异常错误"结束，日志里也说不出原因。
// Outcome 让调用方能按需记录 event=idle_timeout 之类的可运维事件。
type Outcome struct {
	// AToB / BToA 分别是 a→b 与 b→a 方向转发的字节数。
	AToB, BToA int64
	// AToBErr / BToAErr 是对应方向 io.Copy 的结束错误；nil 表示读到 EOF 正常结束。
	AToBErr, BToAErr error
}

// Err 返回第一个"异常"错误（超时、连接重置等）；没有则返回 nil。
//
// 正常 EOF、对端/本端已关闭、ctx 取消都视为正常结束。
func (o Outcome) Err() error {
	if abnormal(o.AToBErr) {
		return o.AToBErr
	}
	if abnormal(o.BToAErr) {
		return o.BToAErr
	}
	return nil
}

// TimedOut 报告是否有方向因超时结束（典型场景：WithIdleTimeout 的空闲超时）。
func (o Outcome) TimedOut() bool {
	return isTimeout(o.AToBErr) || isTimeout(o.BToAErr)
}

// isTimeout 判断错误是否实现了 Timeout() bool 且为 true。
func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return ne.Timeout()
	}
	return false
}

// Bidirectional 在 a、b 之间双向转发，返回 a→b 与 b→a 的字节数。
//
// 语义：
//   - a→b 方向结束（读到 EOF）时，对对端 b 调用 CloseWrite（若不支持则 Close），
//     b→a 方向继续排空，直到 b 侧也结束；
//   - 某个方向出现异常错误（非 EOF、非 ctx 取消、非已关闭）时关闭两端，避免
//     另一方向永久阻塞；
//   - ctx 取消时立即 Close 两端；
//   - 返回前关闭 done 信号并等待 ctx 看护 goroutine 退出，不留下后台 goroutine。
//
// 注意：函数返回前会关闭 a、b 两端（正常路径下两端已 EOF/半关闭，重复关闭无害）。
func Bidirectional(ctx context.Context, a, b io.ReadWriteCloser) (aToB, bToA int64) {
	out := BidirectionalWithOutcome(ctx, a, b)
	return out.AToB, out.BToA
}

// BidirectionalWithOutcome 与 Bidirectional 行为完全一致，额外返回结束原因。
func BidirectionalWithOutcome(ctx context.Context, a, b io.ReadWriteCloser) Outcome {
	if a == nil || b == nil {
		return Outcome{}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	var (
		once      sync.Once
		aToBRes   = make(chan Outcome, 1)
		bToARes   = make(chan Outcome, 1)
		done      = make(chan struct{})
		watchDone = make(chan struct{})
	)

	abort := func() {
		once.Do(func() {
			_ = a.Close()
			_ = b.Close()
		})
	}

	go func() {
		defer close(watchDone)
		select {
		case <-ctx.Done():
			abort()
		case <-done:
		}
	}()

	go func() {
		n, err := io.Copy(b, a)
		shutdownWrite(b)
		if abnormal(err) {
			abort()
		}
		aToBRes <- Outcome{AToB: n, AToBErr: err}
	}()

	go func() {
		n, err := io.Copy(a, b)
		shutdownWrite(a)
		if abnormal(err) {
			abort()
		}
		bToARes <- Outcome{BToA: n, BToAErr: err}
	}()

	first := <-aToBRes
	second := <-bToARes

	close(done)
	<-watchDone
	abort()
	return Outcome{
		AToB:    first.AToB,
		BToA:    second.BToA,
		AToBErr: first.AToBErr,
		BToAErr: second.BToAErr,
	}
}

// abnormal 报告错误是否需要中止两端（正常 EOF / ctx 取消 / 连接已关闭都返回 false）。
func abnormal(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) {
		return false
	}
	return true
}

// shutdownWrite 对连接做半关闭；不支持 CloseWrite 时退化为整体 Close。
func shutdownWrite(c io.ReadWriteCloser) {
	if cw, ok := c.(closeWriter); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// idleConn 在每次读写前刷新 deadline，实现"连接空闲超时"。
type idleConn struct {
	net.Conn
	idle time.Duration
}

// WithIdleTimeout 包装 net.Conn：每次 Read/Write 前把读写 deadline 设为 now+idle，
// 从而在无流量时让阻塞中的读写以超时错误返回。idle <= 0 时原样返回。
//
// 包装后的连接仍支持 CloseWrite（若底层支持），便于 Bidirectional 半关闭；
// 超时错误通常满足 net.Error.Timeout()，调用方据此判定空闲超时。
func WithIdleTimeout(conn net.Conn, idle time.Duration) net.Conn {
	if conn == nil || idle <= 0 {
		return conn
	}
	return &idleConn{Conn: conn, idle: idle}
}

// Read 刷新读 deadline 后读取。
func (c *idleConn) Read(p []byte) (int, error) {
	if err := c.Conn.SetReadDeadline(time.Now().Add(c.idle)); err != nil {
		return 0, err
	}
	return c.Conn.Read(p)
}

// Write 刷新写 deadline 后写出。
func (c *idleConn) Write(p []byte) (int, error) {
	if err := c.Conn.SetWriteDeadline(time.Now().Add(c.idle)); err != nil {
		return 0, err
	}
	return c.Conn.Write(p)
}

// CloseWrite 透传半关闭能力；底层不支持时退化为 Close。
func (c *idleConn) CloseWrite() error {
	if cw, ok := c.Conn.(closeWriter); ok {
		return cw.CloseWrite()
	}
	return c.Conn.Close()
}

// NetConn 返回被包装的原始连接。
func (c *idleConn) NetConn() net.Conn { return c.Conn }

// JoinContext 在 ctx 取消时关闭 conn；返回的 stop 用于提前解除绑定。
//
// stop 幂等，并会等待看护 goroutine 退出，便于测试统计 goroutine 数量。
func JoinContext(ctx context.Context, conn io.Closer) (stop func()) {
	if conn == nil {
		return func() {}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	var once sync.Once
	done := make(chan struct{})
	finished := make(chan struct{})

	go func() {
		defer close(finished)
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	return func() {
		once.Do(func() {
			close(done)
			<-finished
		})
	}
}
