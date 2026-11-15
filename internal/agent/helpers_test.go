package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/logx"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// ---------------------------------------------------------------------------
// 日志
// ---------------------------------------------------------------------------

// testLogger 返回写 io.Discard 的 logger，保持测试输出干净。
func testLogger() *slog.Logger {
	return logx.New(logx.Config{Level: "error", Format: "text", Output: io.Discard})
}

// ---------------------------------------------------------------------------
// 假 relay
// ---------------------------------------------------------------------------

// relayOptions 控制假 relay 的行为。
type relayOptions struct {
	// token 非空时校验 agent 的 Hello.token，不匹配回 auth_failed。
	token string
	// helloVersion 非 0 时用作 HelloOK.version（用于版本不匹配测试）。
	helloVersion int
	// udpMode 是 HelloOK / ServiceRegistered 下发的 udp_mode。
	udpMode string
	// heartbeatSecs 是 HelloOK.heartbeat_seconds。
	heartbeatSecs int
	// failServiceID 非空时对该服务的注册回 MsgError。
	failServiceID string
	// failUnregisterID 非空时对该服务的注销回 MsgError。
	failUnregisterID string
	// silentServiceID 非空时对该服务的注册/注销都不回应（模拟超时）。
	silentServiceID string
	// agentID 是 HelloOK.agent_id。
	agentID string
	// enableTCPData 为 true 时监听 TCP 数据面（127.0.0.1:0）。
	enableTCPData bool
	// enableUDPData 为 true 时监听原生 UDP 数据面。
	enableUDPData bool
	// dropBindOK 为 N 时，原生 UDP 数据面前 N 个 Bind 不回应 BindOK（模拟丢包）。
	dropBindOK int
	// dataTLS 是 HelloOK.data_tls；nil 表示模拟 v0.3.1 旧 relay（未下发该字段）。
	dataTLS *bool
}

// tunnelBind 是承载连接上收到的 MsgBind（首帧或会话帧）。
type tunnelBind struct {
	Bind    *protocol.Bind
	Session string
	First   bool
}

// tunnelData 是承载连接上收到的数据帧。
type tunnelData struct {
	Session string
	Payload []byte
}

// udpBind 是原生 UDP 数据面收到的报到数据报。
type udpBind struct {
	Bind *protocol.Bind
	From *net.UDPAddr
}

// fakeRelay 是包内测试用的最小 relay。
//
// 隧道端口：完成 Hello 校验、HelloOK 应答、注册应答与心跳应答；
// TCP 数据面：读一帧 MsgBind 后把连接交给测试（纯透传）或转为 UDP 隧道帧解析；
// 原生 UDP 数据面：收到 MsgBind 数据报后回 MsgBindOK 并记录 agent 端点。
type fakeRelay struct {
	t    *testing.T
	opts relayOptions

	tunnelLn net.Listener
	tcpLn    net.Listener
	udpConn  *net.UDPConn

	helloOK protocol.HelloOK

	mu      sync.Mutex
	ctrl    net.Conn
	carrier net.Conn
	udpFrom *net.UDPAddr
	tracked []net.Conn

	// registrations 记录 agent 发来的注册请求。
	registrations chan *protocol.RegisterService
	// unregistrations 记录 agent 发来的注销请求。
	unregistrations chan *protocol.UnregisterService
	// binds 记录 TCP 数据面报到帧。
	binds chan *protocol.Bind
	// dataConns 交出 TCP 数据面连接（纯透传）。
	dataConns chan net.Conn
	// carrierConns 交出 UDP over TCP 承载连接。
	carrierConns chan net.Conn
	// tunnelBinds 记录承载连接上的 MsgBind（首帧 + 会话帧）。
	tunnelBinds chan tunnelBind
	// tunnelData 记录承载连接上的数据帧。
	tunnelData chan tunnelData
	// controlFrames 记录承载连接上的控制帧（session=""）。
	controlFrames chan []byte
	// udpBinds 记录原生 UDP 报到。
	udpBinds chan udpBind
	// udpData 记录原生 UDP 数据报（agent 写回的数据）。
	udpData chan []byte

	// tunnels 统计接受过的隧道（控制面）连接数（重连测试用）。
	tunnels atomic.Int32
	// heartbeats 统计收到的 MsgHeartbeat 数量（重连后心跳重建用）。
	heartbeats atomic.Int32
	// dropBindOK 剩余需要丢弃的 BindOK 数量（模拟丢包）。
	dropBindOK atomic.Int32
}

// newFakeRelay 启动一个假 relay 并注册清理函数。
func newFakeRelay(t *testing.T, opts relayOptions) *fakeRelay {
	t.Helper()
	f := &fakeRelay{
		t:               t,
		opts:            opts,
		registrations:   make(chan *protocol.RegisterService, 16),
		unregistrations: make(chan *protocol.UnregisterService, 16),
		binds:           make(chan *protocol.Bind, 16),
		dataConns:       make(chan net.Conn, 16),
		carrierConns:    make(chan net.Conn, 4),
		tunnelBinds:     make(chan tunnelBind, 16),
		tunnelData:      make(chan tunnelData, 32),
		controlFrames:   make(chan []byte, 16),
		udpBinds:        make(chan udpBind, 16),
		udpData:         make(chan []byte, 32),
	}
	f.dropBindOK.Store(int32(opts.dropBindOK))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("假 relay 监听隧道端口: %v", err)
	}
	f.tunnelLn = ln
	if opts.enableTCPData {
		l, lerr := net.Listen("tcp", "127.0.0.1:0")
		if lerr != nil {
			t.Fatalf("假 relay 监听 TCP 数据面: %v", lerr)
		}
		f.tcpLn = l
	}
	if opts.enableUDPData {
		addr, aerr := net.ResolveUDPAddr("udp", "127.0.0.1:0")
		if aerr != nil {
			t.Fatalf("解析 UDP 监听地址: %v", aerr)
		}
		c, cerr := net.ListenUDP("udp", addr)
		if cerr != nil {
			t.Fatalf("假 relay 监听 UDP 数据面: %v", cerr)
		}
		f.udpConn = c
	}

	version := opts.helloVersion
	if version == 0 {
		version = protocol.ProtocolVersion
	}
	agentID := opts.agentID
	if agentID == "" {
		agentID = "a-0123456789abcdef"
	}
	f.helloOK = protocol.HelloOK{
		AgentID:          agentID,
		Version:          version,
		HeartbeatSeconds: opts.heartbeatSecs,
		UDPMode:          opts.udpMode,
		TCPDataPort:      f.tcpPort(),
		UDPDataPort:      f.udpPort(),
		DataTLS:          opts.dataTLS,
	}

	go f.serveTunnel()
	if f.tcpLn != nil {
		go f.serveTCPData()
	}
	if f.udpConn != nil {
		go f.serveUDPData()
	}
	t.Cleanup(f.close)
	return f
}

// tunnelAddr 返回隧道端口地址。
func (f *fakeRelay) tunnelAddr() string { return f.tunnelLn.Addr().String() }

// tcpPort 返回 TCP 数据面端口（未监听时为 0）。
func (f *fakeRelay) tcpPort() int {
	if f.tcpLn == nil {
		return 0
	}
	_, port := splitHostPortForTest(f.tcpLn.Addr().String())
	return port
}

// udpPort 返回原生 UDP 数据面端口（未监听时为 0）。
func (f *fakeRelay) udpPort() int {
	if f.udpConn == nil {
		return 0
	}
	_, port := splitHostPortForTest(f.udpConn.LocalAddr().String())
	return port
}

// splitHostPortForTest 从 host:port 取端口。
func splitHostPortForTest(addr string) (string, int) {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0
	}
	port := 0
	for i := 0; i < len(portText); i++ {
		if portText[i] < '0' || portText[i] > '9' {
			return host, 0
		}
		port = port*10 + int(portText[i]-'0')
	}
	return host, port
}

// close 关闭全部监听与连接。
func (f *fakeRelay) close() {
	f.mu.Lock()
	conns := append([]net.Conn(nil), f.tracked...)
	f.tracked = nil
	f.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
	if f.tunnelLn != nil {
		_ = f.tunnelLn.Close()
	}
	if f.tcpLn != nil {
		_ = f.tcpLn.Close()
	}
	if f.udpConn != nil {
		_ = f.udpConn.Close()
	}
}

// track 记录连接，供清理时关闭。
func (f *fakeRelay) track(conn net.Conn) {
	f.mu.Lock()
	f.tracked = append(f.tracked, conn)
	f.mu.Unlock()
}

// writeCtrl 串行向隧道连接写一帧。
func (f *fakeRelay) writeCtrl(typ protocol.MessageType, payload any) error {
	f.mu.Lock()
	c := f.ctrl
	f.mu.Unlock()
	if c == nil {
		return errors.New("假 relay: 控制连接尚未建立")
	}
	return protocol.WriteFrame(c, typ, payload)
}

// closeControl 关闭隧道连接（模拟控制连接意外断开）。
func (f *fakeRelay) closeControl() {
	f.mu.Lock()
	c := f.ctrl
	f.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}

// sendIncoming 向 agent 下发接入通知。
func (f *fakeRelay) sendIncoming(key, token, proto string) error {
	return f.writeCtrl(protocol.MsgIncoming, &protocol.Incoming{
		PairingKey:   key,
		PairingToken: token,
		Proto:        proto,
		Role:         protocol.RoleAgent,
		ClientAddr:   "203.0.113.9:5555",
	})
}

// sendKicked 下发踢出。
func (f *fakeRelay) sendKicked(reason string) error {
	return f.writeCtrl(protocol.MsgKicked, &protocol.Kicked{Reason: reason})
}

// sendServerShutdown 下发停机通知。
func (f *fakeRelay) sendServerShutdown(reason string) error {
	return f.writeCtrl(protocol.MsgServerShutdown, &protocol.ServerShutdown{Reason: reason})
}

// carrierConn 返回当前承载连接。
func (f *fakeRelay) carrierConn() net.Conn {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.carrier
}

// sendTunnelFrame 在承载连接上写一个 UDP 隧道帧。
func (f *fakeRelay) sendTunnelFrame(session string, payload []byte) error {
	c := f.carrierConn()
	if c == nil {
		return errors.New("假 relay: 承载连接尚未建立")
	}
	return protocol.WriteUDPTunnelFrame(c, session, payload)
}

// sendTunnelData 在承载连接上向指定会话发送一个数据报。
func (f *fakeRelay) sendTunnelData(session string, data []byte) error {
	return f.sendTunnelFrame(session, data)
}

// sendTunnelError 发送 session="" 的 MsgError 控制帧；detail 用于会话归属。
func (f *fakeRelay) sendTunnelError(code, detail string) error {
	frame, err := protocol.EncodeFrame(protocol.MsgError, &protocol.Error{
		Code:    code,
		Message: "测试错误",
		Detail:  detail,
	})
	if err != nil {
		return err
	}
	return f.sendTunnelFrame("", frame)
}

// sendUDP 向 agent 的原生 UDP 端点发送一个数据报。
func (f *fakeRelay) sendUDP(data []byte) error {
	f.mu.Lock()
	from := f.udpFrom
	f.mu.Unlock()
	if from == nil || f.udpConn == nil {
		return errors.New("假 relay: 尚未收到 UDP 报到")
	}
	_, err := f.udpConn.WriteToUDP(data, from)
	return err
}

// serveTunnel 接受隧道连接。
func (f *fakeRelay) serveTunnel() {
	for {
		conn, err := f.tunnelLn.Accept()
		if err != nil {
			return
		}
		f.tunnels.Add(1)
		f.track(conn)
		go f.handleTunnel(conn)
	}
}

// tunnelConns 返回已接受的隧道（控制面）连接总数。
func (f *fakeRelay) tunnelConns() int { return int(f.tunnels.Load()) }

// handleTunnel 处理一条隧道（控制面）连接。
func (f *fakeRelay) handleTunnel(conn net.Conn) {
	defer conn.Close()

	typ, payload, err := protocol.ReadFrame(conn)
	if err != nil || typ != protocol.MsgHello {
		return
	}
	decoded, derr := protocol.Decode(typ, payload)
	if derr != nil {
		return
	}
	hello, ok := decoded.(*protocol.Hello)
	if !ok {
		return
	}
	if f.opts.token != "" && hello.Token != f.opts.token {
		_ = protocol.WriteFrame(conn, protocol.MsgError, &protocol.Error{
			Code:    protocol.ErrCodeAuthFailed,
			Message: "接入密钥错误",
		})
		return
	}

	f.mu.Lock()
	f.ctrl = conn
	f.mu.Unlock()

	okMsg := f.helloOK
	if err := protocol.WriteFrame(conn, protocol.MsgHelloOK, &okMsg); err != nil {
		return
	}
	for {
		typ, payload, err := protocol.ReadFrame(conn)
		if err != nil {
			return
		}
		switch typ {
		case protocol.MsgRegisterService:
			decoded, derr := protocol.Decode(typ, payload)
			if derr != nil {
				continue
			}
			req, isReq := decoded.(*protocol.RegisterService)
			if !isReq {
				continue
			}
			if f.opts.failServiceID != "" && req.ServiceID == f.opts.failServiceID {
				_ = protocol.WriteFrame(conn, protocol.MsgError, &protocol.Error{
					Code:    protocol.ErrCodeBadRequest,
					Message: "服务ID 非法",
				})
				continue
			}
			select {
			case f.registrations <- req:
			default:
			}
			if f.opts.silentServiceID != "" && req.ServiceID == f.opts.silentServiceID {
				continue
			}
			if err := protocol.WriteFrame(conn, protocol.MsgServiceRegistered, &protocol.ServiceRegistered{
				ServiceID: req.ServiceID,
				Proto:     req.Proto,
				UDPMode:   f.opts.udpMode,
			}); err != nil {
				return
			}
		case protocol.MsgUnregisterService:
			decoded, derr := protocol.Decode(typ, payload)
			if derr != nil {
				continue
			}
			req, isReq := decoded.(*protocol.UnregisterService)
			if !isReq {
				continue
			}
			select {
			case f.unregistrations <- req:
			default:
			}
			if f.opts.silentServiceID != "" && req.ServiceID == f.opts.silentServiceID {
				continue
			}
			if f.opts.failUnregisterID != "" && req.ServiceID == f.opts.failUnregisterID {
				_ = protocol.WriteFrame(conn, protocol.MsgError, &protocol.Error{
					Code:    protocol.ErrCodeBadRequest,
					Message: "服务未注册",
				})
				continue
			}
			if err := protocol.WriteFrame(conn, protocol.MsgServiceUnregistered, &protocol.ServiceUnregistered{
				ServiceID: req.ServiceID,
			}); err != nil {
				return
			}
		case protocol.MsgHeartbeat:
			f.heartbeats.Add(1)
			_ = protocol.WriteFrame(conn, protocol.MsgHeartbeatAck, &protocol.HeartbeatAck{})
		}
	}
}

// serveTCPData 接受 TCP 数据面连接。
func (f *fakeRelay) serveTCPData() {
	for {
		conn, err := f.tcpLn.Accept()
		if err != nil {
			return
		}
		f.track(conn)
		go f.handleTCPData(conn)
	}
}

// handleTCPData 读一帧 MsgBind 后按 carrier 分流。
func (f *fakeRelay) handleTCPData(conn net.Conn) {
	typ, payload, err := protocol.ReadFrame(conn)
	if err != nil {
		_ = conn.Close()
		return
	}
	if typ != protocol.MsgBind {
		_ = conn.Close()
		return
	}
	decoded, derr := protocol.Decode(typ, payload)
	if derr != nil {
		_ = conn.Close()
		return
	}
	bind, ok := decoded.(*protocol.Bind)
	if !ok {
		_ = conn.Close()
		return
	}
	if bind.Carrier == protocol.CarrierUDPOverTCP {
		f.handleCarrier(conn, bind)
		return
	}
	select {
	case f.binds <- bind:
	default:
	}
	select {
	case f.dataConns <- conn:
	default:
	}
}

// handleCarrier 把连接转为 UDP 隧道帧解析。
func (f *fakeRelay) handleCarrier(conn net.Conn, first *protocol.Bind) {
	f.mu.Lock()
	f.carrier = conn
	f.mu.Unlock()

	select {
	case f.carrierConns <- conn:
	default:
	}
	select {
	case f.tunnelBinds <- tunnelBind{Bind: first, Session: first.PairingKey, First: true}:
	default:
	}

	// 回 session="" 的 MsgBindOK 控制帧表示承载连接就绪。
	okFrame, err := protocol.EncodeFrame(protocol.MsgBindOK, &protocol.BindOK{PairingKey: first.PairingKey})
	if err != nil {
		_ = conn.Close()
		return
	}
	if err := protocol.WriteUDPTunnelFrame(conn, "", okFrame); err != nil {
		_ = conn.Close()
		return
	}

	for {
		session, payload, err := protocol.ReadUDPTunnelFrame(conn)
		if err != nil {
			return
		}
		if session == "" {
			select {
			case f.controlFrames <- append([]byte(nil), payload...):
			default:
			}
			continue
		}
		if typ, body, perr := protocol.ParseFrame(payload); perr == nil && typ == protocol.MsgBind {
			decoded, derr := protocol.Decode(typ, body)
			if derr == nil {
				if bind, ok := decoded.(*protocol.Bind); ok {
					select {
					case f.tunnelBinds <- tunnelBind{Bind: bind, Session: session}:
					default:
					}
					continue
				}
			}
		}
		select {
		case f.tunnelData <- tunnelData{Session: session, Payload: append([]byte(nil), payload...)}:
		default:
		}
	}
}

// serveUDPData 处理原生 UDP 数据面数据报。
func (f *fakeRelay) serveUDPData() {
	buf := make([]byte, 65535)
	for {
		n, from, err := f.udpConn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		data := append([]byte(nil), buf[:n]...)
		if typ, payload, perr := protocol.ParseFrame(data); perr == nil && typ == protocol.MsgBind {
			decoded, derr := protocol.Decode(typ, payload)
			if derr == nil {
				if bind, ok := decoded.(*protocol.Bind); ok {
					f.mu.Lock()
					f.udpFrom = from
					f.mu.Unlock()
					select {
					case f.udpBinds <- udpBind{Bind: bind, From: from}:
					default:
					}
					if f.dropBindOK.Add(-1) >= 0 {
						// 模拟 BindOK 丢包：不回应，等 agent 重发。
						continue
					}
					okFrame, eerr := protocol.EncodeFrame(protocol.MsgBindOK, &protocol.BindOK{PairingKey: bind.PairingKey})
					if eerr == nil {
						_, _ = f.udpConn.WriteToUDP(okFrame, from)
					}
					continue
				}
			}
		}
		select {
		case f.udpData <- data:
		default:
		}
	}
}

// ---------------------------------------------------------------------------
// 本地 echo 服务
// ---------------------------------------------------------------------------

// startTCPEcho 启动一个 TCP echo 服务，返回其地址。
func startTCPEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动 TCP echo: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()
	return ln.Addr().String()
}

// startUDPEcho 启动一个 UDP echo 服务，返回其地址。
func startUDPEcho(t *testing.T) string {
	t.Helper()
	return startUDPEchoTag(t, "")
}

// startUDPEchoTag 启动一个会给回包加前缀 tag 的 UDP echo 服务，返回其地址。
//
// 前缀用于验证「数据被转发到了正确的本地目标」（多服务路由测试）。
func startUDPEchoTag(t *testing.T, tag string) string {
	t.Helper()
	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("解析 UDP echo 地址: %v", err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		t.Fatalf("启动 UDP echo: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, 65535)
		for {
			n, from, rerr := conn.ReadFromUDP(buf)
			if rerr != nil {
				return
			}
			reply := append([]byte(tag), buf[:n]...)
			_, _ = conn.WriteToUDP(reply, from)
		}
	}()
	return conn.LocalAddr().String()
}

// closedTCPAddr 返回一个当前无人监听（拨号会被拒绝）的 TCP 地址。
func closedTCPAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("获取空闲端口: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("关闭临时监听: %v", err)
	}
	return addr
}

// freeTCPAddr 返回一个当前空闲的 TCP 地址（用于管理页监听）。
func freeTCPAddr(t *testing.T) string {
	t.Helper()
	return closedTCPAddr(t)
}

// ---------------------------------------------------------------------------
// agent 运行夹具
// ---------------------------------------------------------------------------

// agentRun 是一次 agent.Run 的运行夹具，便于测试观察内部状态。
type agentRun struct {
	a      *agent
	cancel context.CancelFunc
	errCh  chan error
	done   chan struct{}

	once     sync.Once
	err      error
	timedOut bool
}

// startAgent 在后台启动 agent.Run。
func startAgent(t *testing.T, cfg *config.Config) *agentRun {
	t.Helper()
	return startAgentWithLogger(t, cfg, testLogger())
}

// startAgentWithLogger 用指定 logger 在后台启动 agent.Run。
//
// 重连退避的基准等待被缩短到 10ms（默认 1s），让重连相关用例快速收敛；
// 不启用重连的用例不受影响。
func startAgentWithLogger(t *testing.T, cfg *config.Config, log *slog.Logger) *agentRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	a := newAgent(ctx, cfg, log)
	a.reconnectBase = 10 * time.Millisecond
	r := &agentRun{
		a:      a,
		cancel: cancel,
		errCh:  make(chan error, 1),
		done:   make(chan struct{}),
	}
	go func() {
		err := a.run()
		r.errCh <- err
		close(r.done)
	}()
	t.Cleanup(func() {
		cancel()
		r.waitQuiet()
	})
	return r
}

// waitQuiet 等待 Run 返回（不触发 Fatal）。
func (r *agentRun) waitQuiet() error {
	r.once.Do(func() {
		select {
		case r.err = <-r.errCh:
		case <-time.After(10 * time.Second):
			r.timedOut = true
		}
	})
	return r.err
}

// wait 等待 Run 返回并返回其错误。
func (r *agentRun) wait(t *testing.T) error {
	t.Helper()
	err := r.waitQuiet()
	if r.timedOut {
		t.Fatalf("agent.Run 未在 10s 内退出")
	}
	return err
}

// running 报告 Run 是否仍在运行。
func (r *agentRun) running() bool {
	select {
	case <-r.done:
		return false
	default:
		return true
	}
}

// stop 取消 ctx 并等待 Run 返回。
func (r *agentRun) stop(t *testing.T) error {
	t.Helper()
	r.cancel()
	return r.wait(t)
}

// ---------------------------------------------------------------------------
// 配置与断言辅助
// ---------------------------------------------------------------------------

// newTestConfig 构造一份 agent 测试配置；services 的第一个元素作为主服务。
func newTestConfig(relayAddr string, services ...config.ServerService) *config.Config {
	cfg := &config.Config{
		Common: config.CommonConfig{LogLevel: "error", LogFormat: "text"},
		Relay:  config.RelayConfig{PairTimeout: 2 * time.Second},
		Server: config.ServerConfig{
			RelayAddr:         relayAddr,
			Token:             "test-token",
			Heartbeat:         time.Hour,
			DialTimeout:       2 * time.Second,
			UDPSessionTimeout: 60 * time.Second,
		},
	}
	if len(services) > 0 {
		cfg.Server.Protocol = services[0].Proto
		cfg.Server.Local = services[0].Addr
		cfg.Server.Services = append([]config.ServerService(nil), services[1:]...)
	}
	return cfg
}

// recvTimeout 是"等待对端事件"的上限，不是被测行为本身。
//
// 取 10s：`go test -race ./...` 会并行跑多个包，WSL2 上 CPU 争用会把
// "300ms 重发 + 调度"放大到数秒——5s 曾在全量 -race 中把 UDP 重发用例
// 误判为超时（单跑稳定 0.31s）。真实行为断言（如重发间隔 < 2s）仍由用例自己把住。
const recvTimeout = 10 * time.Second

// recv 从通道接收一个值，超时则 Fatal。
func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(recvTimeout):
		t.Fatalf("等待 %s 超时", what)
	}
	var zero T
	return zero
}

// noValue 断言通道在短时间内没有值（用于验证没有出现第二条规定/连接）。
func noValue[T any](t *testing.T, ch <-chan T, d time.Duration, what string) {
	t.Helper()
	select {
	case v := <-ch:
		t.Fatalf("预期没有 %s，但收到了 %+v", what, v)
	case <-time.After(d):
	}
}

// waitCond 轮询等待条件成立。
func waitCond(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 %s 超时", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// readN 带 deadline 地从连接读取恰好 n 字节。
func readN(t *testing.T, conn net.Conn, n int, what string) []byte {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("设置读 deadline: %v", err)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("读取 %s: %v", what, err)
	}
	return buf
}

// writeAll 向连接写完整字节并设置写 deadline。
func writeAll(t *testing.T, conn net.Conn, data []byte, what string) {
	t.Helper()
	if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("设置写 deadline: %v", err)
	}
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("写入 %s: %v", what, err)
	}
}

// expectEOF 断言连接很快被对端关闭。
func expectEOF(t *testing.T, conn net.Conn, what string) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("设置读 deadline: %v", err)
	}
	buf := make([]byte, 1)
	n, err := conn.Read(buf)
	if err == nil {
		t.Fatalf("%s: 预期连接被关闭，却读到 %d 字节", what, n)
	}
	if n != 0 {
		t.Fatalf("%s: 预期 EOF，读到 %d 字节", what, n)
	}
}

// checkNoGoroutineLeak 在 agent 退出后校验 goroutine 数量未增长（±2 容差）。
func checkNoGoroutineLeak(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		runtime.GC()
		n := runtime.NumGoroutine()
		if n <= before+2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine 泄漏：agent 启动前 %d，退出后 %d", before, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// tryHTTPGet 发起一次 GET，返回响应体与是否成功（非 200 或网络错误都算失败）。
func tryHTTPGet(url string) (string, bool) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", false
	}
	return string(body), resp.StatusCode == http.StatusOK
}
