package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/pion/transport/v4/packetio"

	"github.com/lsong98sh/pairlink/internal/pairing"
	"github.com/lsong98sh/pairlink/internal/protocol"
	"github.com/lsong98sh/pairlink/internal/registry"
	"github.com/lsong98sh/pairlink/internal/udpdtls"
)

// 本文件实现原生 UDP 数据面的 DTLS-PSK 层（v0.5.0）。
//
// 为什么不用 github.com/pion/transport/v4/udp.Listener：
// 该包 import golang.org/x/net/ipv4，而本模块的 go.sum 里没有 golang.org/x/net
// 的任何校验和（go get 才能补），且本阶段禁止改 go.mod / go.sum。最小实验
// （TestZZExperiment，已删除）确认：启用 udp.Listen 后 go test 直接报
//
//	missing go.sum entry for module providing package golang.org/x/net/ipv4
//
// 因此这里用等价的自研实现：仍然只保留「单个 UDP socket + 单个读循环」，
// 每个对端一个 packetio.Buffer（保持报文边界，与 udp.Listener 的做法一致），
// 由读循环把数据报投递到队列；再用 net.Conn → net.PacketConn 适配器交给
// dtls.Server。收益是 server.go 现有的 *net.UDPConn 可以直接复用（不必改
// 监听方式），且不引入新的模块依赖。
//
// DTLS 记录的最大入站报文受 pion/dtls 的 8192 字节读缓冲限制（见
// inboundBufferSize），与这里的队列长度无关：超过上限的记录会让该对端
// 因 "dtls fatal: short buffer" 关闭（实验实测 8000 字节可通、8192 失败）。

const (
	// PSK 参数（info 前缀 / 长度 / 派生函数）统一在 internal/udpdtls，
	// 三端共用一份实现，避免只改一端导致握手失败。
	// udpDTLSIdentityMaxLen 限定 PSK identity 的最大长度。
	//
	// identity 就是 pairing_key（"serviceID/clientConnID"，两段各 1~64 字节，
	// 最长 64+1+64=129）。
	udpDTLSIdentityMaxLen = 129
	// udpDTLSHandshakeWarnInterval 是 UDP DTLS 握手失败 WARN 的最小间隔（限流）。
	udpDTLSHandshakeWarnInterval = 5 * time.Second
	// udpPeerQueueLimit 是单个 DTLS 对端的接收队列长度（报文数）。
	//
	// 队列满时丢弃新数据报（UDP 语义允许丢包），绝不阻塞唯一读循环。
	udpPeerQueueLimit = 32
	// maxUDPDTLSPeers 是同时在册的 DTLS 对端上限（含握手中的对端）。
	//
	// 伪造 DTLS 记录头的报文会让 relay 为每个源地址建一个握手中的对端，因此
	// 需要硬上限兜底（与默认 max_pending_pairs=4096 同量级），避免 goroutine
	// 与内存无界增长。
	maxUDPDTLSPeers = 4096
	// udpPeerSweepInterval 是 DTLS 对端空闲清理周期（与 relay 的 UDP sweep ticker 一致）。
	udpPeerSweepInterval = time.Second
	// udpPeerWriteTimeout 是向单个 DTLS 对端写一帧的超时。
	udpPeerWriteTimeout = 2 * time.Second
	// dtlsRecordHeaderSize 是 DTLS 记录固定头长度（type 1 + version 2 + epoch 2 + seq 6 + length 2）。
	dtlsRecordHeaderSize = 13
)

// errUDPDTLSIdentity 表示 PSK identity 为空/超长/不是合法配对键（握手必须失败）。
var errUDPDTLSIdentity = errors.New("relay: UDP DTLS PSK identity 非法")

// errUDPDTLSNotRecord 表示收到的数据报不是 DTLS 记录（v0.4.0 明文端）。
var errUDPDTLSNotRecord = errors.New("relay: 非 DTLS 记录")

// errUDPDTLSPeerLimit 表示 DTLS 对端数量已达上限。
var errUDPDTLSPeerLimit = errors.New("relay: DTLS 对端数量已达上限")

// errUDPDTLSPeerGone 表示目标端点当前没有 DTLS 对端（已回收）。
var errUDPDTLSPeerGone = errors.New("relay: DTLS 对端不存在")

// errUDPDTLSClosed 表示 UDP 数据面已关闭，不再接受新对端。
var errUDPDTLSClosed = errors.New("relay: UDP 数据面已关闭")

// newDTLSConfig 构造 relay 侧的 DTLS-PSK 配置（DTLS 1.2 + AES-128-GCM）。
//
// PSK 回调的 identity 是 client 发来的 pairing_key：为空、超长或不是合法
// 配对键一律返回错误 → 握手失败；绝不回退到任何全局/默认密钥。
func (l *UDPDataListener) newDTLSConfig() *dtls.Config {
	token := l.srv.cfg.Relay.AuthToken
	return &dtls.Config{
		CipherSuites: []dtls.CipherSuiteID{dtls.TLS_PSK_WITH_AES_128_GCM_SHA256},
		PSK: func(identity []byte) ([]byte, error) {
			if len(identity) == 0 || len(identity) > udpDTLSIdentityMaxLen {
				return nil, fmt.Errorf("%w: 长度 %d", errUDPDTLSIdentity, len(identity))
			}
			key, err := pairing.ParsePairingKey(string(identity))
			if err != nil {
				return nil, fmt.Errorf("%w: %v", errUDPDTLSIdentity, err)
			}
			return udpdtls.Derive(token, key.String())
		},
	}
}

// looksLikeDTLSRecord 只看 13 字节记录固定头，判断数据报是否"可能是 DTLS"。
//
// 记录头布局：content_type(1) version(2) epoch(2) sequence_number(6) length(2)。
//
// 目的：把 v0.4.0 明文端与随机垃圾挡在握手之外，避免为每个明文数据报建一个
// 握手中的对端（每个对端一个 goroutine + 队列）。PairLink 的 MsgBind 首字节
// 是 0x0C，与 DTLS 的 content type（0x14~0x17）天然不冲突。
//
// 这只是快速预筛，不是安全判断：伪造记录头仍会进入 DTLS 握手，由 PSK 与
// DTLS 自身的校验决定成败。
func looksLikeDTLSRecord(b []byte) bool {
	if len(b) < dtlsRecordHeaderSize {
		return false
	}
	switch b[0] {
	case 20, 21, 22, 23: // change_cipher_spec / alert / handshake / application_data
	default:
		return false
	}
	// 版本高字节恒为 0xFE（DTLS 1.0/1.2/1.3）。
	version := uint16(b[1])<<8 | uint16(b[2])
	return version == 0xFEFD || version == 0xFEFF || version == 0xFEFC
}

// udpPeerConn 是「单个对端」的虚拟 net.Conn：
//
//   - Read 从该对端的报文队列取一个数据报（packetio.Buffer 保持报文边界）；
//   - Write 直接写到共享的 UDP socket（对端地址构造时固定）；
//   - deadline 只作用于本对端的队列，绝不动共享 socket 的 deadline。
//
// 唯一读循环（loop）只负责把数据报投递进队列，绝不为每个数据报起 goroutine。
type udpPeerConn struct {
	l      *UDPDataListener
	remote *net.UDPAddr
	buf    *packetio.Buffer
}

// newUDPPeerConn 为一个对端创建虚拟连接（带长度上限的报文队列）。
func newUDPPeerConn(l *UDPDataListener, remote *net.UDPAddr) *udpPeerConn {
	buf := packetio.NewBuffer()
	buf.SetLimitCount(udpPeerQueueLimit)
	return &udpPeerConn{l: l, remote: remote, buf: buf}
}

// enqueue 把一个数据报放进该对端的接收队列（由唯一读循环调用）。
func (c *udpPeerConn) enqueue(b []byte) (int, error) { return c.buf.Write(b) }

// Read 实现 net.Conn。
func (c *udpPeerConn) Read(p []byte) (int, error) { return c.buf.Read(p) }

// Write 实现 net.Conn：写到共享 socket 的固定对端。
func (c *udpPeerConn) Write(p []byte) (int, error) { return c.l.conn.WriteToUDP(p, c.remote) }

// Close 关闭队列（dtls.Conn.Close 会调用到这里）。
func (c *udpPeerConn) Close() error { return c.buf.Close() }

// LocalAddr 返回共享 socket 的本端地址。
func (c *udpPeerConn) LocalAddr() net.Addr { return c.l.conn.LocalAddr() }

// RemoteAddr 返回该对端地址。
func (c *udpPeerConn) RemoteAddr() net.Addr { return c.remote }

// SetDeadline 只设置队列的读超时；对端共享 socket，写超时按对端无法建模。
func (c *udpPeerConn) SetDeadline(t time.Time) error     { return c.buf.SetReadDeadline(t) }
func (c *udpPeerConn) SetReadDeadline(t time.Time) error { return c.buf.SetReadDeadline(t) }
func (c *udpPeerConn) SetWriteDeadline(time.Time) error  { return nil }

// udpPacketConn 把单对端 net.Conn 适配成 dtls 需要的 net.PacketConn：
//
//   - ReadFrom 返回该对端的地址（dtls 用它做 cookie / 重传校验）；
//   - WriteTo 忽略目标地址（对端在构造时已固定，不与其它对端共享连接）；
//   - deadline 直接透传给底层虚拟连接（不影响共享 socket）。
type udpPacketConn struct{ net.Conn }

// ReadFrom 实现 net.PacketConn。
func (p *udpPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, err := p.Conn.Read(b)
	return n, p.Conn.RemoteAddr(), err
}

// WriteTo 实现 net.PacketConn（忽略 addr，写向固定对端）。
func (p *udpPacketConn) WriteTo(b []byte, _ net.Addr) (int, error) { return p.Conn.Write(b) }

// udpPeer 是原生 UDP 数据面上的一个 DTLS 对端。
type udpPeer struct {
	l    *UDPDataListener
	addr registry.UDPEndpoint
	log  *slog.Logger

	ctx    context.Context // 对端生命周期上下文（close / 停机时取消）
	cancel context.CancelFunc

	conn   *udpPeerConn
	dtls   *dtls.Conn
	remote *net.UDPAddr

	lastActive atomic.Int64
	writeMu    sync.Mutex
	closeOnce  sync.Once
}

// touch 记录一次活动（读或写）。
func (p *udpPeer) touch() { p.lastActive.Store(time.Now().UnixNano()) }

// lastActiveAt 返回最后一次活动时间。
func (p *udpPeer) lastActiveAt() time.Time { return time.Unix(0, p.lastActive.Load()) }

// write 向该对端写一帧应用数据（DTLS Write 内部串行，这里再串行化 deadline）。
func (p *udpPeer) write(data []byte) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	p.touch()
	if err := p.dtls.SetWriteDeadline(time.Now().Add(udpPeerWriteTimeout)); err != nil {
		return err
	}
	_, err := p.dtls.Write(data)
	return err
}

// close 关闭 DTLS 会话与虚拟连接（幂等）。
//
// 先取消握手中上下文再 Close：dtls.Conn.Close 会等待握手 goroutine 退出，
// 不取消的话，握手中的对端最坏要等满 handshakeTimeout。
func (p *udpPeer) close() {
	p.closeOnce.Do(func() {
		if p.cancel != nil {
			p.cancel()
		}
		if p.dtls != nil {
			_ = p.dtls.Close()
		}
		_ = p.conn.Close()
	})
}

// run 是单个对端的 goroutine：先完成握手，再持续读应用数据交给 handleDatagram。
func (p *udpPeer) run() {
	defer p.l.srv.wg.Done()
	defer func() {
		p.close()
		p.l.removePeer(p)
	}()

	// 握手只受 handshakeTimeout 约束；对端生命周期由 p.ctx（close / 停机）控制。
	// 握手成功后再也不会因为"握手超时"而影响读循环。
	hctx, hcancel := context.WithTimeout(p.ctx, p.l.handshakeTimeout)
	err := p.dtls.HandshakeContext(hctx)
	timedOut := errors.Is(hctx.Err(), context.DeadlineExceeded)
	hcancel()
	if err != nil {
		if p.l.srv.closing.Load() || p.ctx.Err() != nil {
			return
		}
		reason := "handshake"
		if timedOut {
			reason = "handshake_timeout"
		}
		p.l.warnDTLSFail(p.log, p.addr, reason, err)
		return
	}
	p.touch()
	p.log.Debug("UDP DTLS 握手完成", "event", "udp_dtls_handshake_ok")

	buf := make([]byte, protocol.MaxUDPDatagram)
	for {
		n, err := p.dtls.Read(buf)
		if err != nil {
			if p.l.srv.closing.Load() || isClosedNetErr(err) {
				return
			}
			// 建连后读失败（对端重启、超限记录等）：回收该对端，不刷 WARN。
			p.log.Debug("UDP DTLS 对端读结束，回收对端", "event", "udp_evict", "err", err)
			return
		}
		p.touch()
		p.l.handleDatagram(buf[:n], p.remote, p.log)
	}
}

// isClosedNetErr 报告 err 是否是"连接已关闭/已 EOF"这类正常终止错误。
func isClosedNetErr(err error) bool {
	return err == nil ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, dtls.ErrConnClosed)
}

// init 惰性初始化 DTLS 相关状态。
//
// server.go 直接以 &UDPDataListener{srv: s, conn: udpConn} 构造监听器，因此
// DTLS 开关、对端表与关闭信号都在首次使用（loop/close）时初始化。
func (l *UDPDataListener) init() {
	l.initOnce.Do(func() {
		l.closeCh = make(chan struct{})
		l.peers = make(map[registry.UDPEndpoint]*udpPeer)
		// 用 atomic.Bool 发布开关：DTLS 模式下的转发/写回路径在其它 goroutine 里
		// 也要读它，避免"init 里写、goroutine 里无锁读"的数据竞争。
		enabled := l.srv.cfg.Relay.UDPDTLS
		l.dtlsOn.Store(enabled)
		l.handshakeTimeout = l.srv.pairTimeout()
		if enabled {
			l.dtlsCfg = l.newDTLSConfig()
		}
	})
}

// startPeer 为一个新源地址建立 DTLS 对端并启动读 goroutine。
//
// wg.Add 与 closed 检查都在 l.mu 内完成：close() 也在 l.mu 内取走全部对端，
// 因此不会出现 "close 之后又冒出一个对端 goroutine" 的窗口。
func (l *UDPDataListener) startPeer(from *net.UDPAddr, ep registry.UDPEndpoint) (*udpPeer, error) {
	q := newUDPPeerConn(l, from)
	dtlsConn, err := dtls.Server(&udpPacketConn{Conn: q}, from, l.dtlsCfg)
	if err != nil {
		_ = q.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(l.srv.ctx)
	p := &udpPeer{
		l:      l,
		addr:   ep,
		log:    l.log().With("remote", ep.String()),
		ctx:    ctx,
		cancel: cancel,
		conn:   q,
		dtls:   dtlsConn,
		remote: from,
	}

	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		p.close()
		return nil, errUDPDTLSClosed
	}
	if len(l.peers) >= maxUDPDTLSPeers {
		l.mu.Unlock()
		p.close()
		return nil, errUDPDTLSPeerLimit
	}
	l.peers[ep] = p
	l.srv.wg.Add(1)
	l.mu.Unlock()

	go p.run()
	return p, nil
}

// lookupPeer 按端点查对端（不存在返回 nil）。
func (l *UDPDataListener) lookupPeer(ep registry.UDPEndpoint) *udpPeer {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.peers[ep]
}

// removePeer 从对端表移除并彻底关闭一个对端（幂等）。
func (l *UDPDataListener) removePeer(p *udpPeer) {
	l.mu.Lock()
	if cur, ok := l.peers[p.addr]; ok && cur == p {
		delete(l.peers, p.addr)
	}
	l.mu.Unlock()
	l.forgetSessions(p.addr, "peer_closed")
}

// peerCount 返回当前在册对端数（含握手中的对端）。
func (l *UDPDataListener) peerCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.peers)
}

// sweepLoop 周期性回收空闲 DTLS 对端，直到 close 或读循环结束。
func (l *UDPDataListener) sweepLoop(done <-chan struct{}) {
	ticker := time.NewTicker(udpPeerSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-l.closeCh:
			return
		case <-done:
			return
		case now := <-ticker.C:
			l.sweepPeers(now)
		}
	}
}

// sweepPeers 回收长时间没有活动的对端。
//
// 阈值比 UDP 会话超时多一个 pair_timeout：relay 清理会话后，DTLS 对端仍会
// 多存活一段时间，让「同一 5 元组再发数据报」落进 handleDatagram 的未知端点
// 丢弃分支（与 v0.4.0 明文行为一致），而不是会话刚过期就拆掉 DTLS 连接、
// 逼客户端立刻重新握手。对端最终仍会被回收，不会无界增长。
func (l *UDPDataListener) sweepPeers(now time.Time) {
	timeout := l.peerIdleTimeout()
	if timeout <= 0 {
		return
	}
	var idle []*udpPeer
	l.mu.Lock()
	for ep, p := range l.peers {
		if now.Sub(p.lastActiveAt()) >= timeout {
			delete(l.peers, ep)
			idle = append(idle, p)
		}
	}
	l.mu.Unlock()
	for _, p := range idle {
		p.log.Debug("UDP DTLS 对端空闲超时，回收对端", "event", "udp_evict", "idle", timeout.String())
		p.close()
		l.forgetSessions(p.addr, "peer_idle")
	}
}

// peerIdleTimeout 返回对端空闲回收阈值（0 = 不回收）。
//
// 会话超时 + pair_timeout：给刚被清理的会话留一个重连/重报到的观察窗口，
// 同时保证无主对端最多多占 pair_timeout 这么久就会回收。
func (l *UDPDataListener) peerIdleTimeout() time.Duration {
	timeout := l.srv.udpSessionTimeoutValue()
	if timeout <= 0 {
		return 0
	}
	if extra := l.srv.pairTimeout(); extra > 0 {
		timeout += extra
	}
	return timeout
}

// closePeers 关闭全部对端（停机路径；close() 已先把 closed 置位）。
func (l *UDPDataListener) closePeers() {
	l.mu.Lock()
	peers := make([]*udpPeer, 0, len(l.peers))
	for ep, p := range l.peers {
		delete(l.peers, ep)
		peers = append(peers, p)
	}
	l.mu.Unlock()
	for _, p := range peers {
		p.close()
	}
}

// warnDTLSFail 记录"非 DTLS 数据报 / DTLS 握手失败 / 对端上限"。
//
// 每个 listener 每 udpDTLSHandshakeWarnInterval 最多一条 WARN（其余降为
// Debug）：否则一条伪造记录头的扫描就能把日志打满。
func (l *UDPDataListener) warnDTLSFail(log *slog.Logger, ep registry.UDPEndpoint, reason string, err error) {
	now := time.Now().UnixNano()
	last := l.lastDTLSWarn.Load()
	if last == 0 || now-last >= int64(udpDTLSHandshakeWarnInterval) {
		if l.lastDTLSWarn.CompareAndSwap(last, now) {
			log.Warn("原生 UDP 数据面收到非 DTLS 数据报或 DTLS 握手失败，已丢弃",
				"event", "udp_dtls_handshake_fail",
				"remote", ep.String(), "reason", reason, "err", err)
			return
		}
	}
	log.Debug("原生 UDP 数据面收到非 DTLS 数据报或 DTLS 握手失败，已丢弃",
		"event", "udp_dtls_handshake_fail",
		"remote", ep.String(), "reason", reason, "err", err)
}

// forgetSessions 移除该端点对应的 UDP 会话（对端关闭后会话已无意义）。
func (l *UDPDataListener) forgetSessions(ep registry.UDPEndpoint, reason string) {
	if l.srv.udpTable == nil {
		return
	}
	key, _, ok := l.srv.udpTable.LookupByEndpoint(ep)
	if !ok {
		return
	}
	l.srv.udpTable.Remove(key)
	l.srv.log.Info("UDP 会话随 DTLS 对端关闭而移除", "event", "udp_session_close",
		"pairing_key", key.String(), "service_id", key.ServiceID,
		"remote", ep.String(), "reason", reason)
}
