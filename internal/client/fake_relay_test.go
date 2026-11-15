package client

import (
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/protocol"
)

// fakeRelayOptions 配置假 relay 的握手应答与数据面监听。
type fakeRelayOptions struct {
	// helloVersion 是 HelloOK.Version（默认 protocol.ProtocolVersion）。
	helloVersion int
	// helloErr 非空时在收到 Hello 后回 MsgError 并关闭连接。
	helloErr *protocol.Error
	// services 是 ServiceList.Services。
	services []protocol.ServiceInfo
	// revision 是 ServiceList.Revision。
	revision uint64
	// udpMode 是下发的 udp_mode（默认 native）。
	udpMode string
	// withUDP 为 true 时额外监听一个 UDP 数据面端口。
	withUDP bool
	// noDataPorts 为 true 时 HelloOK 不下发 tcp_data_port / udp_data_port
	// （模拟旧 relay 或端口信息缺失）。
	noDataPorts bool
	// listFn 非 nil 时按「第几条隧道连接」（从 1 开始）返回服务列表，
	// 用于模拟 relay 重启后 revision 归零/服务变化。
	listFn func(connIndex int) (services []protocol.ServiceInfo, revision uint64)
	// dataTLS 是 HelloOK.data_tls；nil 表示模拟 v0.3.1 旧 relay（未下发该字段）。
	dataTLS *bool
}

// relayFrame 是假 relay 收到的控制面消息。
type relayFrame struct {
	typ protocol.MessageType
	msg any
}

// fakeDataConn 是假 relay 接受的数据面连接及其首帧 MsgBind。
//
// 首帧读完后连接所有权交给测试，后续字节由测试自行收发。
type fakeDataConn struct {
	conn net.Conn
	bind *protocol.Bind
}

// fakeDatagram 是假 relay UDP 数据面收到的数据报。
type fakeDatagram struct {
	data []byte
	from *net.UDPAddr
}

// fakeRelay 是测试用的最小 relay：实现 Hello/HelloOK、ListServices/ServiceList、
// 控制面消息收集、TCP 数据面 accept 与（可选）UDP 数据面收发。
type fakeRelay struct {
	t    *testing.T
	opts fakeRelayOptions

	tunnelLn net.Listener
	dataLn   net.Listener
	udpConn  *net.UDPConn

	mu    sync.Mutex
	ctrl  net.Conn
	hello *protocol.Hello

	frames chan relayFrame
	dataCh chan *fakeDataConn
	udpCh  chan fakeDatagram

	ready     chan struct{}
	readyOnce sync.Once
	closed    chan struct{}
	closeOnce sync.Once

	// connSeq 是隧道连接序号（从 1 开始），hellos 是收到的 Hello 数量。
	connSeq atomic.Int32
	hellos  atomic.Int32
}

// newFakeRelay 启动假 relay，测试结束自动关闭。
func newFakeRelay(t *testing.T, opts fakeRelayOptions) *fakeRelay {
	t.Helper()
	if opts.helloVersion == 0 {
		opts.helloVersion = protocol.ProtocolVersion
	}
	if opts.udpMode == "" {
		opts.udpMode = protocol.UDPModeNative
	}
	tunnelLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("假 relay 监听隧道端口失败: %v", err)
	}
	dataLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = tunnelLn.Close()
		t.Fatalf("假 relay 监听 TCP 数据面失败: %v", err)
	}
	r := &fakeRelay{
		t:        t,
		opts:     opts,
		tunnelLn: tunnelLn,
		dataLn:   dataLn,
		frames:   make(chan relayFrame, 128),
		dataCh:   make(chan *fakeDataConn, 8),
		udpCh:    make(chan fakeDatagram, 128),
		ready:    make(chan struct{}),
		closed:   make(chan struct{}),
	}
	if opts.withUDP {
		udpConn, udpErr := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if udpErr != nil {
			_ = tunnelLn.Close()
			_ = dataLn.Close()
			t.Fatalf("假 relay 监听 UDP 数据面失败: %v", udpErr)
		}
		r.udpConn = udpConn
		go r.readUDP()
	}
	go r.acceptTunnel()
	go r.acceptData()
	t.Cleanup(r.close)
	return r
}

// addr 返回隧道端口地址（用于 client 的 relay_addr）。
func (r *fakeRelay) addr() string { return r.tunnelLn.Addr().String() }

// close 关闭假 relay 的全部监听与已建立的控制连接。
func (r *fakeRelay) close() {
	r.closeOnce.Do(func() {
		close(r.closed)
		_ = r.tunnelLn.Close()
		_ = r.dataLn.Close()
		if r.udpConn != nil {
			_ = r.udpConn.Close()
		}
		r.mu.Lock()
		conn := r.ctrl
		r.mu.Unlock()
		if conn != nil {
			_ = conn.Close()
		}
	})
}

// waitReady 等待控制面握手完成（HelloOK + ServiceList 已发出）。
func (r *fakeRelay) waitReady(t *testing.T) {
	t.Helper()
	select {
	case <-r.ready:
	case <-time.After(5 * time.Second):
		t.Fatalf("假 relay 控制面握手未完成")
	}
}

// helloMsg 返回收到的 MsgHello。
func (r *fakeRelay) helloMsg(t *testing.T) *protocol.Hello {
	t.Helper()
	r.waitReady(t)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hello == nil {
		t.Fatalf("假 relay 没有收到 hello")
	}
	return r.hello
}

// acceptTunnel 接受隧道连接并为每个连接起一个 handler。
func (r *fakeRelay) acceptTunnel() {
	for {
		conn, err := r.tunnelLn.Accept()
		if err != nil {
			return
		}
		go r.serveTunnel(conn)
	}
}

// serveTunnel 处理一条隧道连接：Hello/HelloOK、ListServices/ServiceList，
// 之后把收到的控制面消息放入 frames 供测试断言。
func (r *fakeRelay) serveTunnel(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	connIndex := int(r.connSeq.Add(1))

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	typ, payload, err := protocol.ReadFrame(conn)
	if err != nil {
		return
	}
	msg, err := protocol.Decode(typ, payload)
	if err != nil || typ != protocol.MsgHello {
		return
	}
	hello, ok := msg.(*protocol.Hello)
	if !ok {
		return
	}
	r.hellos.Add(1)
	r.mu.Lock()
	r.hello = hello
	r.ctrl = conn
	r.mu.Unlock()

	if r.opts.helloErr != nil {
		_ = protocol.WriteFrame(conn, protocol.MsgError, r.opts.helloErr)
		return
	}

	helloOK := &protocol.HelloOK{
		ClientID:         "c-0123456789abcdef",
		Version:          r.opts.helloVersion,
		HeartbeatSeconds: 30,
		UDPMode:          r.opts.udpMode,
		DataTLS:          r.opts.dataTLS,
	}
	if !r.opts.noDataPorts {
		helloOK.TCPDataPort = portOf(r.dataLn.Addr().String())
		if r.udpConn != nil {
			helloOK.UDPDataPort = portOf(r.udpConn.LocalAddr().String())
		}
	}
	if err := protocol.WriteFrame(conn, protocol.MsgHelloOK, helloOK); err != nil {
		return
	}

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	typ, payload, err = protocol.ReadFrame(conn)
	if err != nil {
		return
	}
	msg, err = protocol.Decode(typ, payload)
	if err != nil || typ != protocol.MsgListServices {
		return
	}
	list := &protocol.ServiceList{
		Services: r.opts.services,
		Revision: r.opts.revision,
		UDPMode:  r.opts.udpMode,
	}
	if r.opts.listFn != nil {
		services, revision := r.opts.listFn(connIndex)
		list.Services = services
		list.Revision = revision
	}
	if err := protocol.WriteFrame(conn, protocol.MsgServiceList, list); err != nil {
		return
	}
	r.readyOnce.Do(func() { close(r.ready) })
	_ = conn.SetReadDeadline(time.Time{})

	for {
		typ, payload, err := protocol.ReadFrame(conn)
		if err != nil {
			return
		}
		msg, decErr := protocol.Decode(typ, payload)
		if decErr != nil {
			continue
		}
		select {
		case r.frames <- relayFrame{typ: typ, msg: msg}:
		case <-r.closed:
			return
		}
	}
}

// acceptData 接受 TCP 数据面连接。
func (r *fakeRelay) acceptData() {
	for {
		conn, err := r.dataLn.Accept()
		if err != nil {
			return
		}
		go r.serveData(conn)
	}
}

// serveData 读第一帧 MsgBind，然后把连接交给测试。
func (r *fakeRelay) serveData(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	typ, payload, err := protocol.ReadFrame(conn)
	if err != nil {
		_ = conn.Close()
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	msg, err := protocol.Decode(typ, payload)
	if err != nil || typ != protocol.MsgBind {
		_ = conn.Close()
		return
	}
	bind, ok := msg.(*protocol.Bind)
	if !ok {
		_ = conn.Close()
		return
	}
	select {
	case r.dataCh <- &fakeDataConn{conn: conn, bind: bind}:
	case <-r.closed:
		_ = conn.Close()
	}
}

// readUDP 读取 UDP 数据面数据报并放入 udpCh。
func (r *fakeRelay) readUDP() {
	buf := make([]byte, protocol.MaxUDPDatagram+1)
	for {
		n, from, err := r.udpConn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		dg := fakeDatagram{data: append([]byte(nil), buf[:n]...), from: from}
		select {
		case r.udpCh <- dg:
		case <-r.closed:
			return
		}
	}
}

// send 通过控制连接发一帧。
func (r *fakeRelay) send(t *testing.T, typ protocol.MessageType, payload any) {
	t.Helper()
	r.mu.Lock()
	conn := r.ctrl
	r.mu.Unlock()
	if conn == nil {
		t.Fatalf("假 relay 控制连接尚未建立")
	}
	if err := protocol.WriteFrame(conn, typ, payload); err != nil {
		t.Fatalf("假 relay 写控制帧失败: %v", err)
	}
}

// nextFrame 取下一个控制面消息。
func (r *fakeRelay) nextFrame(t *testing.T, timeout time.Duration) relayFrame {
	t.Helper()
	select {
	case f := <-r.frames:
		return f
	case <-time.After(timeout):
		t.Fatalf("等待控制面消息超时")
		return relayFrame{}
	}
}

// expect 等待指定类型的控制面消息，跳过心跳与订阅这类非目标消息。
func (r *fakeRelay) expect(t *testing.T, want protocol.MessageType) any {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case f := <-r.frames:
			if f.typ == want {
				return f.msg
			}
			switch f.typ {
			case protocol.MsgHeartbeat, protocol.MsgHeartbeatAck, protocol.MsgSubscribe:
				continue
			default:
				t.Fatalf("期望控制面 %s，收到 %s", want, f.typ)
			}
		case <-deadline:
			t.Fatalf("等待控制面 %s 超时", want)
		}
	}
}

// expectSubscribe 等待 MsgSubscribe（跳过心跳）。
func (r *fakeRelay) expectSubscribe(t *testing.T) *protocol.Subscribe {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case f := <-r.frames:
			switch f.typ {
			case protocol.MsgHeartbeat, protocol.MsgHeartbeatAck:
				continue
			case protocol.MsgSubscribe:
				sub, ok := f.msg.(*protocol.Subscribe)
				if !ok {
					t.Fatalf("订阅消息类型异常")
				}
				return sub
			default:
				t.Fatalf("期望 subscribe，收到 %s", f.typ)
			}
		case <-deadline:
			t.Fatalf("等待 subscribe 超时")
		}
	}
}

// nextDataConn 等待一条 TCP 数据面连接。
func (r *fakeRelay) nextDataConn(t *testing.T, timeout time.Duration) *fakeDataConn {
	t.Helper()
	select {
	case dc := <-r.dataCh:
		return dc
	case <-time.After(timeout):
		t.Fatalf("等待数据面连接超时")
		return nil
	}
}

// helloCount 返回收到的 MsgHello 数量（重连测试用）。
func (r *fakeRelay) helloCount() int { return int(r.hellos.Load()) }

// waitHelloCount 等待收到第 want 条 Hello（重连完成）。
func (r *fakeRelay) waitHelloCount(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if r.helloCount() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待第 %d 次 Hello 超时（当前 %d）", want, r.helloCount())
}

// dropControl 关闭当前控制连接（模拟控制连接意外断开，不下发任何消息）。
func (r *fakeRelay) dropControl(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	conn := r.ctrl
	r.mu.Unlock()
	if conn == nil {
		t.Fatalf("假 relay 尚无控制连接")
	}
	_ = conn.Close()
}

// nextDatagram 等待一个 UDP 数据面数据报。
func (r *fakeRelay) nextDatagram(t *testing.T, timeout time.Duration) fakeDatagram {
	t.Helper()
	select {
	case dg := <-r.udpCh:
		return dg
	case <-time.After(timeout):
		t.Fatalf("等待 UDP 数据报超时")
		return fakeDatagram{}
	}
}

// udpSend 向指定地址发一个 UDP 数据报。
func (r *fakeRelay) udpSend(t *testing.T, to *net.UDPAddr, data []byte) {
	t.Helper()
	if _, err := r.udpConn.WriteToUDP(data, to); err != nil {
		t.Fatalf("假 relay 发送 UDP 数据报失败: %v", err)
	}
}

// portOf 从 host:port 中取端口。
func portOf(addr string) int {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return 0
	}
	return n
}
