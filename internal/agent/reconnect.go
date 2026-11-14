package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/lsong98sh/pairlink/internal/reconnect"
)

// permanentErr 表示重连没有意义的错误（relay 明确拒绝、协议版本不兼容、本端配置非法）。
type permanentErr struct{ err error }

// errKicked 表示被 relay 管理页踢出：清理后 Run 返回错误，不重连。
var errKicked = errors.New("agent: 已被 relay 踢出")

// Error 实现 error。
func (e permanentErr) Error() string { return e.err.Error() }

// Unwrap 支持 errors.Is/As 透传。
func (e permanentErr) Unwrap() error { return e.err }

// permanent 把错误标记为不可重试。
func permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentErr{err: err}
}

// isPermanent 报告 err 是否不可重试。
func isPermanent(err error) bool {
	var pe permanentErr
	return errors.As(err, &pe)
}

// reconnectEnabled 报告是否启用控制连接自动重连（[server] reconnect，默认 true）。
func (a *agent) reconnectEnabled() bool { return a.cfg.Server.Reconnect }

// backoff 返回本次运行的退避序列（首次使用时按配置构造）。
func (a *agent) backoff() *reconnect.Backoff {
	a.backoffOnce.Do(func() {
		max := a.cfg.Server.ReconnectMaxBackoff
		if max <= 0 {
			max = reconnect.DefaultMaxBackoff
		}
		seq := reconnect.New(max)
		if a.reconnectBase > 0 {
			seq.Base = a.reconnectBase
		}
		a.backoffSeq = seq
	})
	return a.backoffSeq
}

// waitRetry 记录 reconnect_scheduled 并按退避等待；ctx 取消返回 ctx.Err()。
func (a *agent) waitRetry(attempt int) error {
	d := a.backoff().Delay(attempt)
	a.log.Info("已安排重连", "event", "reconnect_scheduled",
		"attempt", attempt, "delay", d.Round(time.Millisecond).String())
	return reconnect.Sleep(a.ctx, d)
}

// supervise 是控制连接监督循环：维持长连接，非踢出/非永久错误断开后按指数退避重连。
//
// 返回值即 Run 的返回值：
//   - ctx 取消 → nil；
//   - MsgServerShutdown → nil；
//   - MsgKicked / 永久错误 → 非 nil（不重连）；
//   - reconnect = false 时的控制连接断开 → 非 nil（旧行为）。
func (a *agent) supervise() error {
	var (
		attempt int
		downAt  time.Time
	)
	for {
		if a.ctx.Err() != nil {
			return nil
		}
		if err := a.connect(); err != nil {
			if a.ctx.Err() != nil {
				return nil
			}
			if !a.reconnectEnabled() || isPermanent(err) {
				return err
			}
			if downAt.IsZero() {
				downAt = time.Now()
			}
			attempt++
			a.log.Warn("接入 relay 失败，准备重连", "event", "reconnect_failed",
				"attempt", attempt, "err", err)
			if werr := a.waitRetry(attempt); werr != nil {
				return nil
			}
			continue
		}
		if attempt > 0 {
			a.log.Info("控制连接已恢复", "event", "reconnect_ok",
				"attempt", attempt,
				"elapsed", time.Since(downAt).Round(time.Millisecond).String())
			attempt, downAt = 0, time.Time{}
		}

		err := a.serveControl()
		if a.ctx.Err() != nil {
			return nil
		}
		if err == nil {
			return nil
		}
		if errors.Is(err, errKicked) || isPermanent(err) {
			return err
		}
		if !a.reconnectEnabled() {
			return err
		}
		if downAt.IsZero() {
			downAt = time.Now()
		}
		a.log.Warn("控制连接已断开，准备重连", "event", "reconnect_failed", "err", err)
		// 重连期间关闭全部在途数据连接/本地会话（reason=reconnect），不做迁移。
		a.closeInFlight("reconnect")
		attempt++
		if werr := a.waitRetry(attempt); werr != nil {
			return nil
		}
	}
}

// serveControl 运行一条控制连接的读循环与心跳，返回即代表该连接终止。
//
// 返回 nil 表示收到 MsgServerShutdown；errKicked（包装后）表示被踢出；
// 其它错误代表连接断开/读失败。函数返回前保证本连接的心跳与 ctx 看护
// goroutine 都已退出，不会留下后台 goroutine。
func (a *agent) serveControl() error {
	ctrl := a.controlConn()
	if ctrl == nil {
		return errors.New("agent: 控制连接未建立")
	}
	connCtx, cancel := context.WithCancel(a.ctx)
	release := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		a.heartbeatLoop(connCtx, ctrl)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-a.ctx.Done():
			// 关停：关闭连接以唤醒阻塞中的读循环（读循环是 supervise goroutine）。
			a.closeControl()
		case <-release:
		}
	}()

	err := a.controlLoop(ctrl)
	close(release)
	cancel()
	a.closeControl()
	wg.Wait()
	return err
}

// watchCancel 在 ctx 取消时关闭 ctrl，返回的 stop 会等看护 goroutine 退出。
//
// 用于（重）连接的握手/注册阶段：即使 relay 迟迟不回 HelloOK，ctx 取消也能立刻
// 打断阻塞读，而不是等 dial_timeout。
func (a *agent) watchCancel(ctrl *controlConn) (stop func()) {
	stopCh := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-a.ctx.Done():
			ctrl.close()
		case <-stopCh:
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(stopCh)
			<-finished
		})
	}
}

// closeInFlight 关闭重连前仍在途的全部会话与承载连接（reason=reconnect）。
//
// 只关连接、不删表项：表项由各会话 goroutine 退出时清理，保持既有清理顺序；
// 不做 UDP/TCP 会话迁移，重连后的新连接照常走完整配对。
func (a *agent) closeInFlight(reason string) {
	n := a.sessions.len()
	a.sessions.closeAll()
	a.carrier.closeAll()
	if n == 0 {
		return
	}
	a.log.Info("关闭在途会话", "event", "data_close",
		"detail", fmt.Sprintf("reason=%s sessions=%d", reason, n))
}
