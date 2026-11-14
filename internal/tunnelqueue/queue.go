// Package tunnelqueue 为 UDP over TCP 承载连接提供「单 writer goroutine +
// 有界发送队列」的发送侧，消除慢对端把读循环一起拖住（队头阻塞放大）。
//
// 设计（docs/architecture.md §4.6、§5）：
//   - 每条承载连接一个独立 writer goroutine，读循环只入队，**绝不因写阻塞**；
//   - 两条队列：控制帧（session=""，如 MsgBindOK / MsgError）优先、数据帧其次，
//     保证会话级控制帧不会被数据洪泛饿死；
//   - 队列满时按策略丢弃并计数，由调用方的 OnDrop 回调限速打印 WARN（event=tunnel_drop）；
//   - 整帧原子写出：每帧只调用一次 protocol.WriteUDPTunnelFrame（内部单次 Write），
//     不会拆帧，也不会与其它 goroutine 交错；
//   - **入队即复制 payload**：调用方可以复用读缓冲（agent/client 的 UDP 转发循环都是
//     单缓冲复用），复制之后内存归队列所有，不会出现"写出时缓冲已被下一次读取覆盖"；
//   - 关闭顺序固定：停止入队 → 关闭 stop → writer 排空 → 关闭连接；
//     Close 返回时 writer 已退出（不泄漏 goroutine）。
package tunnelqueue

import (
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lsong98sh/pairlink/internal/protocol"
)

const (
	// DefaultDataCap 是数据帧队列的默认容量（帧数）。
	DefaultDataCap = 1024
	// DefaultControlCap 是控制帧队列的默认容量（帧数）。
	DefaultControlCap = 256
	// DefaultMaxBytes 是数据帧队列的默认字节上限，避免 1024 × 64KiB 撑爆内存。
	DefaultMaxBytes int64 = 8 << 20
	// DefaultWriteTimeout 是单帧写超时：对端长时间不读时让 writer 以错误退出，
	// 而不是永远阻塞在 Write 上（连接随后会被关闭）。
	DefaultWriteTimeout = 30 * time.Second
	// DefaultDrainTimeout 是 Close 时排空剩余帧的上限。
	DefaultDrainTimeout = 2 * time.Second
)

// Options 是发送队列的参数。
type Options struct {
	// DataCap 是数据帧队列容量（<=0 用 DefaultDataCap）。
	DataCap int
	// ControlCap 是控制帧队列容量（<=0 用 DefaultControlCap）。
	ControlCap int
	// MaxBytes 是数据帧队列的字节上限（<=0 用 DefaultMaxBytes）。
	MaxBytes int64
	// WriteTimeout 是单帧写超时（<=0 用 DefaultWriteTimeout）。
	WriteTimeout time.Duration
	// DrainTimeout 是关闭时排空上限（<=0 用 DefaultDrainTimeout）。
	DrainTimeout time.Duration
	// OnDrop 在发生丢弃时被调用，调用频率被限制为每 kind 每秒最多一次。
	// kind 为 "data" 或 "control"；dropped 是该类累计丢弃帧数；queued 是当前排队帧数。
	OnDrop func(kind string, dropped int64, queued int)
}

// frame 是队列中的一帧：session 为空表示控制帧（payload 是完整 PairLink 帧）。
type frame struct {
	session string
	payload []byte
}

// Queue 是一条承载连接的发送队列（自带 writer goroutine）。
type Queue struct {
	conn net.Conn
	opts Options

	ctrl chan frame
	data chan frame

	// mu 保证「停止入队」与「入队」严格有序：Close 拿到写锁后不会再有新帧入队。
	mu     sync.RWMutex
	closed bool

	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}

	queuedBytes atomic.Int64
	droppedData atomic.Int64
	droppedCtrl atomic.Int64
	lastLogData atomic.Int64
	lastLogCtrl atomic.Int64

	closeOnce sync.Once
}

// New 构造发送队列并立即启动 writer goroutine。
func New(conn net.Conn, opts Options) *Queue {
	opts = normalize(opts)
	q := &Queue{
		conn: conn,
		opts: opts,
		ctrl: make(chan frame, opts.ControlCap),
		data: make(chan frame, opts.DataCap),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	go q.writeLoop()
	return q
}

// normalize 填充默认值。
func normalize(o Options) Options {
	if o.DataCap <= 0 {
		o.DataCap = DefaultDataCap
	}
	if o.ControlCap <= 0 {
		o.ControlCap = DefaultControlCap
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMaxBytes
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = DefaultWriteTimeout
	}
	if o.DrainTimeout <= 0 {
		o.DrainTimeout = DefaultDrainTimeout
	}
	return o
}

// SendControl 把一帧控制帧（session=""）放入优先队列；返回是否成功入队。
//
// 绝不阻塞：队列满或已关闭时返回 false（调用方按丢包处理）。payload 会被复制。
func (q *Queue) SendControl(payload []byte) bool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.closed {
		return false
	}
	select {
	case q.ctrl <- frame{payload: append([]byte(nil), payload...)}:
		return true
	default:
		q.reportDrop("control")
		return false
	}
}

// SendData 把一个数据帧（session=<配对键>）放入数据队列；返回是否成功入队。
//
// 绝不阻塞：队列满（帧数或字节数上限）、已关闭时返回 false 并计入丢弃统计。
// payload 会被复制，调用方可以立即复用读缓冲。
func (q *Queue) SendData(session string, payload []byte) bool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.closed {
		return false
	}
	n := int64(len(payload))
	if !q.admit(n) {
		q.reportDrop("data")
		return false
	}
	cp := append([]byte(nil), payload...)
	select {
	case q.data <- frame{session: session, payload: cp}:
		return true
	default:
		q.queuedBytes.Add(-n)
		q.reportDrop("data")
		return false
	}
}

// admit 检查字节上限（成功时已计入）。
func (q *Queue) admit(n int64) bool {
	if q.opts.MaxBytes <= 0 || n <= 0 {
		return true
	}
	if q.queuedBytes.Add(n) > q.opts.MaxBytes {
		q.queuedBytes.Add(-n)
		return false
	}
	return true
}

// reportDrop 累计丢弃并按 kind 限速触发 OnDrop（每秒最多一次）。
func (q *Queue) reportDrop(kind string) {
	var (
		total int64
		last  *atomic.Int64
	)
	if kind == "control" {
		total = q.droppedCtrl.Add(1)
		last = &q.lastLogCtrl
	} else {
		total = q.droppedData.Add(1)
		last = &q.lastLogData
	}
	if q.opts.OnDrop == nil {
		return
	}
	now := time.Now().UnixNano()
	prev := last.Load()
	if now-prev < int64(time.Second) {
		return
	}
	if !last.CompareAndSwap(prev, now) {
		return
	}
	q.opts.OnDrop(kind, total, q.Queued())
}

// Dropped 返回累计丢弃帧数（data, control）。
func (q *Queue) Dropped() (data, control int64) {
	return q.droppedData.Load(), q.droppedCtrl.Load()
}

// Queued 返回当前排队帧数（控制 + 数据）。
func (q *Queue) Queued() int {
	return len(q.ctrl) + len(q.data)
}

// Closed 报告队列是否已停止接收新帧。
func (q *Queue) Closed() bool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return q.closed
}

// Close 执行「停止入队 → 关闭队列 → writer 排空 → 关连接」，幂等。
//
// 返回时 writer goroutine 已退出；若排空超时（对端不读），强制关闭连接让
// writer 的 Write 失败退出，保证关停不会被慢对端无限期拖住。
func (q *Queue) Close() error {
	q.closeOnce.Do(func() {
		q.mu.Lock()
		q.closed = true
		q.mu.Unlock()
		q.stopOnce.Do(func() { close(q.stop) })

		timer := time.NewTimer(q.opts.DrainTimeout)
		select {
		case <-q.done:
		case <-timer.C:
			_ = q.conn.Close()
			<-q.done
		}
		timer.Stop()
		_ = q.conn.Close()
	})
	<-q.done
	return nil
}

// writeLoop 是唯一的 writer goroutine。
func (q *Queue) writeLoop() {
	defer close(q.done)
	for {
		if f, ok := q.tryPop(); ok {
			if err := q.writeFrame(f, time.Time{}); err != nil {
				q.abort()
				return
			}
			continue
		}
		select {
		case <-q.stop:
			q.drain()
			return
		case f := <-q.ctrl:
			if err := q.writeFrame(f, time.Time{}); err != nil {
				q.abort()
				return
			}
		case f := <-q.data:
			if err := q.writeFrame(f, time.Time{}); err != nil {
				q.abort()
				return
			}
		}
	}
}

// tryPop 非阻塞地按「控制优先」取一帧。
func (q *Queue) tryPop() (frame, bool) {
	select {
	case f := <-q.ctrl:
		return f, true
	default:
	}
	select {
	case f := <-q.data:
		return f, true
	default:
	}
	return frame{}, false
}

// drain 在排空上限内写完剩余帧（关闭后调用）。
//
// 用同一个绝对 deadline 覆盖 WriteTimeout：即使对端不读，排空也不会超过 DrainTimeout。
func (q *Queue) drain() {
	deadline := time.Now().Add(q.opts.DrainTimeout)
	for {
		f, ok := q.tryPop()
		if !ok {
			return
		}
		if err := q.writeFrame(f, deadline); err != nil {
			q.abort()
			return
		}
	}
}

// writeFrame 单次整帧写出（不拆帧）。
//
// deadline 非零时使用该绝对写 deadline（排空阶段），否则使用 per-frame 的 WriteTimeout。
func (q *Queue) writeFrame(f frame, deadline time.Time) error {
	if !deadline.IsZero() {
		_ = q.conn.SetWriteDeadline(deadline)
	} else if q.opts.WriteTimeout > 0 {
		_ = q.conn.SetWriteDeadline(time.Now().Add(q.opts.WriteTimeout))
	}
	if err := protocol.WriteUDPTunnelFrame(q.conn, f.session, f.payload); err != nil {
		return err
	}
	if f.session != "" {
		q.queuedBytes.Add(-int64(len(f.payload)))
	}
	return nil
}

// abort 在写失败后停止接收新帧并关闭连接（由既有清理路径收敛）。
func (q *Queue) abort() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.stopOnce.Do(func() { close(q.stop) })
	_ = q.conn.Close()
}
