package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/lsong98sh/pairlink/internal/reconnect"
)

// permanentErr 表示重连没有意义的错误（relay 明确拒绝、协议版本不兼容、本端配置非法）。
type permanentErr struct{ err error }

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

// reconnectEnabled 报告是否启用控制连接自动重连（[client] reconnect，默认 true）。
func (c *client) reconnectEnabled() bool { return c.cfg.Client.Reconnect }

// backoff 返回本次运行的退避序列（首次使用时按配置构造）。
func (c *client) backoff() *reconnect.Backoff {
	c.backoffOnce.Do(func() {
		max := c.cfg.Client.ReconnectMaxBackoff
		if max <= 0 {
			max = reconnect.DefaultMaxBackoff
		}
		seq := reconnect.New(max)
		if c.reconnectBase > 0 {
			seq.Base = c.reconnectBase
		}
		c.backoffSeq = seq
	})
	return c.backoffSeq
}

// waitRetry 记录 reconnect_scheduled 并按退避等待；ctx 取消返回 ctx.Err()。
func (c *client) waitRetry(attempt int) error {
	d := c.backoff().Delay(attempt)
	c.log.Info("已安排重连", "event", "reconnect_scheduled",
		"attempt", attempt, "delay", d.Round(time.Millisecond).String())
	return reconnect.Sleep(c.ctx, d)
}

// supervise 是控制连接监督循环：维持长连接，非踢出/非永久错误断开后按指数退避重连。
//
// 本地监听只在首次接入成功后启动一次；重连不重建监听（第三方无感），只在拿到
// 新的服务列表后校对协议（reconcileListeners，必要时热切换）。
//
// 返回值即 Run 的返回值：
//   - ctx 取消 → nil；
//   - MsgServerShutdown → nil；
//   - MsgKicked / 永久错误 / reconnect=false 的断开 / 本地监听失败 → 非 nil。
func (c *client) supervise() error {
	var (
		attempt        int
		downAt         time.Time
		listenersReady bool
	)
	for {
		if c.ctx.Err() != nil {
			return nil
		}
		if c.controlConn() == nil {
			if err := c.connectOnce(); err != nil {
				if c.ctx.Err() != nil {
					return nil
				}
				if !c.reconnectEnabled() || isPermanent(err) {
					return err
				}
				if downAt.IsZero() {
					downAt = time.Now()
				}
				attempt++
				c.log.Warn("接入 relay 失败，准备重连", "event", "reconnect_failed",
					"attempt", attempt, "err", err)
				if werr := c.waitRetry(attempt); werr != nil {
					return nil
				}
				continue
			}
			if attempt > 0 {
				c.log.Info("控制连接已恢复", "event", "reconnect_ok",
					"attempt", attempt,
					"elapsed", time.Since(downAt).Round(time.Millisecond).String())
			}
			if !listenersReady {
				// 本地监听必须等首次 Hello + 服务列表就绪后再开（协议以列表为准）。
				if err := c.startListeners(); err != nil {
					return err
				}
				listenersReady = true
				// 管理页可能在首次列表到达前就先加了映射（按 TCP 占位）；
				// 这里用刚拿到的基线再校对一次协议。
				c.reconcileListeners()
			} else {
				// 断线期间服务协议可能变化：用新基线校对本地监听。
				c.reconcileListeners()
			}
			attempt, downAt = 0, time.Time{}
		}

		err := c.serveControl()
		if c.ctx.Err() != nil {
			return nil
		}
		select {
		case <-c.stopSig.done:
			return c.stopSig.Err()
		default:
		}
		if err == nil {
			return nil
		}
		if errors.Is(err, errKicked) || isPermanent(err) {
			return err
		}
		if !c.reconnectEnabled() {
			return err
		}
		if downAt.IsZero() {
			downAt = time.Now()
		}
		c.log.Warn("控制连接已断开，准备重连", "event", "reconnect_failed", "err", err)
		// 重连期间关闭全部在途本地会话（reason=reconnect），不做迁移；
		// 本地监听保持不变，重连后的新接入照常走完整配对。
		c.closeSessions("reconnect")
		attempt++
		if werr := c.waitRetry(attempt); werr != nil {
			return nil
		}
	}
}

// serveControl 运行一条控制连接的读循环与心跳，返回即代表该连接终止。
//
// 返回前保证本连接的心跳与 ctx 看护 goroutine 都已退出（不泄漏 goroutine）。
func (c *client) serveControl() error {
	conn := c.controlConn()
	if conn == nil {
		return errControlClosed
	}
	connCtx, cancel := context.WithCancel(c.ctx)
	release := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.heartbeatLoop(connCtx, conn)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-c.ctx.Done():
			// 关停：关闭连接以唤醒阻塞中的读循环。
			c.closeControl()
		case <-release:
		}
	}()

	err := c.controlLoop(conn)
	close(release)
	cancel()
	c.closeControl()
	wg.Wait()
	return err
}

// watchCancel 在 ctx 取消时关闭控制连接，返回的 stop 会等看护 goroutine 退出。
//
// 用于（重）连接的握手阶段：即使 relay 迟迟不回 HelloOK，ctx 取消也能立刻打断
// 阻塞读，而不是等 dial_timeout。
func (c *client) watchCancel(conn net.Conn) (stop func()) {
	stopCh := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-c.ctx.Done():
			_ = conn.Close()
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

// fatal 记录不可恢复错误并唤醒控制读循环，让 supervise 立即返回。
func (c *client) fatal(err error) {
	c.stopSig.trigger(err)
	c.closeControl()
}

// controlWriteFailed 处理「写控制连接失败」：启用重连时只影响本次本地接入
// （关闭该连接，等控制面自行重连），否则按旧行为结束进程。
func (c *client) controlWriteFailed(err error) {
	if c.reconnectEnabled() {
		return
	}
	c.fatal(fmt.Errorf("client: 写控制连接: %w", err))
}
