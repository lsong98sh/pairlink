package tunnelqueue

import (
	"bytes"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/protocol"
)

// recordingConn 是只用于写方向的 net.Conn：把写出的字节追加到缓冲区。
type recordingConn struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	closed bool
	// block 非 nil 时 Write 会阻塞到该 channel 被关闭（模拟慢对端）。
	block chan struct{}
	// entered 非 nil 时每次进入 Write 尝试发一个信号（测试用来确认 writer 已开始写）。
	entered chan struct{}
}

func newRecordingConn() *recordingConn { return &recordingConn{} }

// setBlock 设置阻塞 channel（并发安全，writer goroutine 会读它）。
func (c *recordingConn) setBlock(ch chan struct{}) {
	c.mu.Lock()
	c.block = ch
	c.mu.Unlock()
}

// releaseBlock 关闭阻塞 channel 并解除阻塞（幂等由调用方的 sync.Once 保证）。
func (c *recordingConn) releaseBlock() {
	c.mu.Lock()
	ch := c.block
	c.block = nil
	c.mu.Unlock()
	if ch != nil {
		close(ch)
	}
}

func (c *recordingConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	block := c.block
	entered := c.entered
	c.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if block != nil {
		<-block
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	return c.buf.Write(p)
}

func (c *recordingConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *recordingConn) Close() error                     { c.mu.Lock(); c.closed = true; c.mu.Unlock(); return nil }
func (c *recordingConn) LocalAddr() net.Addr              { return dummyAddr("local") }
func (c *recordingConn) RemoteAddr() net.Addr             { return dummyAddr("remote") }
func (c *recordingConn) SetDeadline(time.Time) error      { return nil }
func (c *recordingConn) SetReadDeadline(time.Time) error  { return nil }
func (c *recordingConn) SetWriteDeadline(time.Time) error { return nil }

// frames 解析缓冲区里写出的隧道帧。
func (c *recordingConn) frames(t *testing.T) []frame {
	t.Helper()
	c.mu.Lock()
	data := append([]byte(nil), c.buf.Bytes()...)
	c.mu.Unlock()
	var out []frame
	r := bytes.NewReader(data)
	for r.Len() > 0 {
		session, payload, err := protocol.ReadUDPTunnelFrame(r)
		if err != nil {
			t.Fatalf("解析隧道帧: %v", err)
		}
		out = append(out, frame{session: session, payload: append([]byte(nil), payload...)})
	}
	return out
}

type dummyAddr string

func (d dummyAddr) Network() string { return "tcp" }
func (d dummyAddr) String() string  { return string(d) }

// TestTunnelQueueDropPolicy 覆盖有界队列的丢弃策略：
// 数据队列满 → 丢弃新帧并计数；控制帧走独立优先队列，不被数据洪泛饿死。
func TestTunnelQueueDropPolicy(t *testing.T) {
	conn := newRecordingConn()
	conn.setBlock(make(chan struct{})) // writer 卡在第一次写，队列无法排空
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() { conn.releaseBlock() })
		conn.setBlock(nil)
	}

	var dropsMu sync.Mutex
	drops := map[string]int64{}
	q := New(conn, Options{
		DataCap:    2,
		ControlCap: 2,
		MaxBytes:   1 << 20,
		OnDrop: func(kind string, dropped int64, queued int) {
			dropsMu.Lock()
			drops[kind] = dropped
			dropsMu.Unlock()
		},
	})
	defer func() {
		release()
		_ = q.Close()
	}()
	// 让 writer 先进入 Write 并卡住，队列从空开始计数（断言确定）。
	entered := make(chan struct{}, 1)
	conn.entered = entered
	if !q.SendData("svc/conn", []byte("inflight")) {
		t.Fatal("在途帧应入队成功")
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("writer 未开始写第一帧")
	}

	// 数据队列容量 2：前 2 帧入队成功，其余被丢弃。
	for i := 0; i < 5; i++ {
		if ok := q.SendData("svc/conn", []byte("data")); !ok && i < 2 {
			t.Fatalf("第 %d 帧应入队成功", i)
		}
	}
	if data, _ := q.Dropped(); data != 3 {
		t.Fatalf("数据帧丢弃 = %d，期望 3", data)
	}

	// 控制队列独立：数据满不影响控制帧入队。
	if !q.SendControl([]byte("ctrl-1")) || !q.SendControl([]byte("ctrl-2")) {
		t.Fatal("数据队列满时控制帧仍应入队")
	}
	if q.SendControl([]byte("ctrl-3")) {
		t.Fatal("控制队列满（2）时应丢弃第 3 帧")
	}
	if _, control := q.Dropped(); control != 1 {
		t.Fatalf("控制帧丢弃 = %d，期望 1", control)
	}

	// 丢弃回调被限速：同一 kind 每秒最多一次。
	dropsMu.Lock()
	for kind, n := range drops {
		if n <= 0 {
			t.Fatalf("%s 的丢弃计数 = %d", kind, n)
		}
	}
	dropsMu.Unlock()

	// 关闭后不再接受新帧。
	release()
	if err := q.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if q.SendData("svc/conn", []byte("late")) {
		t.Fatal("Close 之后 SendData 必须返回 false")
	}
	if q.SendControl([]byte("late")) {
		t.Fatal("Close 之后 SendControl 必须返回 false")
	}
}

// TestTunnelQueueCloseDrains 覆盖关闭顺序：停止入队 → 排空 → writer 退出；
// 并验证控制帧在「已排队的数据帧」之前写出（控制优先）。
func TestTunnelQueueCloseDrains(t *testing.T) {
	conn := newRecordingConn()
	// 先让 writer 卡在第一次写上，保证控制帧入队时数据帧都还在队列里（断言可确定）。
	conn.setBlock(make(chan struct{}))
	entered := make(chan struct{}, 1)
	conn.entered = entered
	q := New(conn, Options{DataCap: 64, ControlCap: 8})

	// 第 0 帧：等 writer 真正进入 Write（在途帧）后再入队其余帧。
	if !q.SendData("svc/conn", []byte{0}) {
		t.Fatal("第 0 帧应入队成功")
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("writer 未开始写第一帧")
	}
	const n = 20
	for i := 1; i < n; i++ {
		if !q.SendData("svc/conn", []byte{byte(i)}) {
			t.Fatalf("第 %d 帧应入队成功", i)
		}
	}
	if !q.SendControl([]byte("last-ctrl")) {
		t.Fatal("控制帧应入队成功")
	}
	// 放开 writer：它会写完在途帧，然后按优先级取下一条（控制帧）。
	conn.releaseBlock()
	if err := q.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	frames := conn.frames(t)
	if len(frames) != n+1 {
		t.Fatalf("排空写出 %d 帧，期望 %d", len(frames), n+1)
	}
	// 第 0 帧是 writer 阻塞期间已经在写的那一帧（数据），第 1 帧必须是控制帧。
	if string(frames[0].payload) != string([]byte{0}) {
		t.Fatalf("在途帧 = %q，期望第一个数据帧", frames[0].payload)
	}
	if frames[1].session != "" || string(frames[1].payload) != "last-ctrl" {
		t.Fatalf("控制帧未优先写出：session=%q payload=%q", frames[1].session, frames[1].payload)
	}
	for i := 0; i < len(frames); i++ {
		if i == 1 {
			continue
		}
		if frames[i].session != "svc/conn" {
			t.Fatalf("第 %d 帧 session = %q", i, frames[i].session)
		}
		want := byte(i)
		if i > 1 {
			want = byte(i - 1)
		}
		if len(frames[i].payload) != 1 || frames[i].payload[0] != want {
			t.Fatalf("第 %d 个数据帧内容 = %v，期望 %d", i, frames[i].payload, want)
		}
	}
	if q.Queued() != 0 {
		t.Fatalf("Close 后仍有 %d 帧排队", q.Queued())
	}
}

// TestTunnelQueueCopiesPayload 校验入队即复制：调用方复用缓冲不会污染已入队帧。
func TestTunnelQueueCopiesPayload(t *testing.T) {
	conn := newRecordingConn()
	conn.setBlock(make(chan struct{}))
	q := New(conn, Options{DataCap: 4, ControlCap: 2})

	buf := []byte("first")
	if !q.SendData("svc/conn", buf) {
		t.Fatal("入队失败")
	}
	copy(buf, "XXXXX") // 复用缓冲
	conn.releaseBlock()
	conn.setBlock(nil)
	if err := q.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	frames := conn.frames(t)
	if len(frames) != 1 || string(frames[0].payload) != "first" {
		t.Fatalf("payload 被调用方复用污染: %v", frames)
	}
}

// TestTunnelQueueEnqueueNeverBlocks 校验慢对端不会阻塞入队（消除队头阻塞放大）。
func TestTunnelQueueEnqueueNeverBlocks(t *testing.T) {
	conn := newRecordingConn()
	conn.setBlock(make(chan struct{})) // 永远不放开，writer 卡死
	q := New(conn, Options{DataCap: 8, ControlCap: 4, MaxBytes: 1 << 20})
	defer func() {
		conn.releaseBlock()
		conn.setBlock(nil)
		_ = q.Close()
	}()

	start := time.Now()
	for i := 0; i < 10000; i++ {
		q.SendData("svc/conn", []byte("payload"))
		q.SendControl([]byte("ctrl"))
	}
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("慢对端下入队被阻塞：10000 轮耗时 %s", elapsed)
	}
	if data, control := q.Dropped(); data == 0 && control == 0 {
		t.Fatal("队列满时应发生丢弃")
	}
	if q.Queued() > 8+4 {
		t.Fatalf("队列超过容量上限：%d", q.Queued())
	}
}

// TestTunnelQueueNoGoroutineLeak 校验 Close 后 writer goroutine 退出；
// "退出"由包级 TestMain 的 goleak 在整包结束时判定，不再用数字容差。
func TestTunnelQueueNoGoroutineLeak(t *testing.T) {
	for i := 0; i < 20; i++ {
		conn := newRecordingConn()
		q := New(conn, Options{})
		q.SendData("svc/conn", []byte("x"))
		if err := q.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
}
