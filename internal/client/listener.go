package client

import (
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/lsong98sh/pairlink/internal/protocol"
)

// localListener 是一个本地监听器：把本机端口映射到某个远端 service_id。
type localListener struct {
	svcID  string
	proto  string // "tcp" / "udp"
	listen string // ini 中配置的监听地址

	tcp net.Listener
	udp *net.UDPConn
}

// actualAddr 返回实际绑定地址（配置为 :0 时是内核分配的真实端口）。
func (l *localListener) actualAddr() string {
	if l.tcp != nil {
		return l.tcp.Addr().String()
	}
	if l.udp != nil {
		return l.udp.LocalAddr().String()
	}
	return l.listen
}

// close 关闭监听器（幂等）。
func (l *localListener) close() {
	if l.tcp != nil {
		_ = l.tcp.Close()
	}
	if l.udp != nil {
		_ = l.udp.Close()
	}
}

// startListeners 按 cfg.Client.Services 逐个开本地监听。
//
// 协议以 relay 返回的服务列表为准（docs/contract.md §2.5 第 4 条）：
//   - 列表中存在该 service_id → 按 proto 开 TCP/UDP 监听；
//   - 不存在 → 按 TCP 开监听并记 WARN（event=service_missing），
//     ConnectRequest 失败时关闭本地连接；
//   - 列表中的 proto 未知 → 按 TCP 处理并 WARN。
//
// 任何本地监听失败（端口冲突等）都返回错误，由 run 结束进程，不静默跳过。
func (c *client) startListeners() error {
	services, _ := c.servicesView()

	for _, svc := range c.cfg.Client.Services {
		info, inList := services[svc.ID]
		proto := "tcp"
		switch {
		case !inList:
			c.log.Warn("relay 服务列表中不存在该服务，按 TCP 打开本地监听；接入请求会被 relay 拒绝",
				"event", "service_missing", "service_id", svc.ID, "detail", "listen="+svc.Listen)
		case info.Proto == "udp":
			proto = "udp"
		case info.Proto == "" || info.Proto == "tcp":
			proto = "tcp"
		default:
			c.log.Warn("relay 返回的服务协议未知，按 TCP 打开本地监听",
				"event", "listen", "service_id", svc.ID, "detail", "proto="+info.Proto)
		}

		if proto == "udp" {
			if c.udpMode == protocol.UDPModeNative && c.udpDataErr != nil {
				// 数据面地址不可用是运行期问题：仍然开本地监听，
				// 等到真有接入时在配对阶段失败并关闭会话，Run 不退出。
				c.log.Warn("原生 UDP 数据面地址不可用，该服务的 UDP 接入将失败",
					"event", "listen", "service_id", svc.ID, "err", c.udpDataErr)
			}
			addr, err := net.ResolveUDPAddr("udp", svc.Listen)
			if err != nil {
				c.closeListeners()
				return fmt.Errorf("client: 解析本地 UDP 监听地址 %q（服务 %s）: %w", svc.Listen, svc.ID, err)
			}
			conn, err := net.ListenUDP("udp", addr)
			if err != nil {
				c.closeListeners()
				return fmt.Errorf("client: 监听本地 UDP %s（服务 %s）: %w", svc.Listen, svc.ID, err)
			}
			l := &localListener{svcID: svc.ID, proto: proto, listen: svc.Listen, udp: conn}
			c.addListener(l)
			c.log.Info("本地 UDP 监听已开启", "event", "listen", "service_id", svc.ID,
				"detail", fmt.Sprintf("proto=udp listen=%s addr=%s udp_mode=%s", svc.Listen, l.actualAddr(), c.udpMode))
			c.tasks.Go(func() { c.serveUDP(l) })
			continue
		}

		if c.tcpDataErr != nil {
			c.log.Warn("TCP 数据面地址不可用，该服务的接入将失败",
				"event", "listen", "service_id", svc.ID, "err", c.tcpDataErr)
		}
		ln, err := net.Listen("tcp", svc.Listen)
		if err != nil {
			c.closeListeners()
			return fmt.Errorf("client: 监听本地 TCP %s（服务 %s）: %w", svc.Listen, svc.ID, err)
		}
		l := &localListener{svcID: svc.ID, proto: proto, listen: svc.Listen, tcp: ln}
		c.addListener(l)
		c.log.Info("本地 TCP 监听已开启", "event", "listen", "service_id", svc.ID,
			"detail", fmt.Sprintf("proto=tcp listen=%s addr=%s", svc.Listen, l.actualAddr()))
		c.tasks.Go(func() { c.serveTCP(l) })
	}
	return nil
}

// addListener 把监听器登记到管理页展示用的列表。
func (c *client) addListener(l *localListener) {
	c.listenerMu.Lock()
	c.listeners = append(c.listeners, l)
	c.listenerMu.Unlock()
}

// closeListeners 关闭全部本地监听器（幂等）。
func (c *client) closeListeners() {
	c.listenerMu.Lock()
	listeners := c.listeners
	c.listeners = nil
	c.listenerMu.Unlock()
	for _, l := range listeners {
		l.close()
	}
}

// serveTCP 是本地 TCP 监听器的 accept 循环。
func (c *client) serveTCP(l *localListener) {
	for {
		conn, err := l.tcp.Accept()
		if err != nil {
			if c.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			c.log.Error("本地 TCP 监听异常", "event", "listen", "service_id", l.svcID, "err", err)
			c.stopSig.trigger(fmt.Errorf("client: 本地监听 %s 异常: %w", l.listen, err))
			return
		}
		c.handleTCPAccept(l, conn)
	}
}

// handleTCPAccept 为每个接入的本地连接生成 client_conn_id 并发 ConnectRequest。
func (c *client) handleTCPAccept(l *localListener, conn net.Conn) {
	connID, err := newConnID()
	if err != nil {
		c.log.Error("生成 client_conn_id 失败，关闭本地连接", "event", "local_accept", "service_id", l.svcID, "err", err)
		_ = conn.Close()
		return
	}
	s := &tcpSession{
		svcID:     l.svcID,
		connID:    connID,
		remote:    conn.RemoteAddr().String(),
		local:     conn,
		startedAt: time.Now(),
	}
	c.tcpMu.Lock()
	c.tcpSessions[connID] = s
	c.tcpMu.Unlock()

	c.log.Info("本地连接已接入", "event", "local_accept", "service_id", l.svcID,
		"remote", s.remote, "pairing_key", s.displayKey())

	if err := c.writeControl(protocol.MsgConnectRequest, &protocol.ConnectRequest{
		ServiceID:    l.svcID,
		ClientConnID: connID,
	}); err != nil {
		c.log.Warn("发送接入请求失败，关闭本地连接", "event", "connect_request",
			"service_id", l.svcID, "remote", s.remote, "err", err)
		c.closeTCPSession(s, "connect_request_failed", err)
		c.stopSig.trigger(fmt.Errorf("client: 写控制连接: %w", err))
		return
	}
	c.log.Info("已发起接入请求", "event", "connect_request", "service_id", l.svcID,
		"remote", s.remote, "pairing_key", s.displayKey())
}

// serveUDP 是本地 UDP 监听器的读循环（单 goroutine，不 per-packet 起 goroutine）。
func (c *client) serveUDP(l *localListener) {
	buf := make([]byte, protocol.MaxUDPDatagram+1)
	for {
		n, src, err := l.udp.ReadFromUDP(buf)
		if err != nil {
			if c.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			c.log.Warn("读取本地 UDP 失败", "event", "listen", "service_id", l.svcID, "err", err)
			continue
		}
		c.handleLocalDatagram(l, src, buf[:n])
	}
}

// handleLocalDatagram 把本地 UDP 数据报映射到 (service_id, 源地址) 会话。
//
// 会话不存在时建立会话、缓存首包并发 ConnectRequest；建立过程中该源地址的
// 后续数据报直接丢弃（UDP 允许丢包），配对成功后补发缓存的首包。
func (c *client) handleLocalDatagram(l *localListener, src *net.UDPAddr, data []byte) {
	key := udpSessionKey{serviceID: l.svcID, src: src.String()}

	c.udpMu.Lock()
	s := c.udpSessions[key]
	c.udpMu.Unlock()
	if s != nil {
		s.touch()
		if s.isForwarding() {
			c.sendUDPToRelay(s, data)
		}
		return
	}

	connID, err := newConnID()
	if err != nil {
		c.log.Error("生成 client_conn_id 失败，丢弃本地数据报", "event", "udp_session_open",
			"service_id", l.svcID, "remote", src.String(), "err", err)
		return
	}
	s = &udpSession{
		svcID:     l.svcID,
		connID:    connID,
		src:       src,
		srcKey:    src.String(),
		listener:  l,
		startedAt: time.Now(),
		firstPkt:  append([]byte(nil), data...),
	}
	s.touch()

	c.udpMu.Lock()
	if existing := c.udpSessions[key]; existing != nil {
		c.udpMu.Unlock()
		existing.touch()
		if existing.isForwarding() {
			c.sendUDPToRelay(existing, data)
		}
		return
	}
	c.udpSessions[key] = s
	c.udpByConn[connID] = s
	c.udpMu.Unlock()

	c.log.Info("本地 UDP 会话建立", "event", "udp_session_open", "service_id", l.svcID,
		"remote", s.srcKey, "pairing_key", s.displayKey())

	if err := c.writeControl(protocol.MsgConnectRequest, &protocol.ConnectRequest{
		ServiceID:    l.svcID,
		ClientConnID: connID,
	}); err != nil {
		c.log.Warn("发送接入请求失败，关闭 UDP 会话", "event", "connect_request",
			"service_id", l.svcID, "remote", s.srcKey, "err", err)
		c.closeUDPSession(s, "connect_request_failed", err)
		c.stopSig.trigger(fmt.Errorf("client: 写控制连接: %w", err))
		return
	}
	c.log.Info("已发起接入请求", "event", "connect_request", "service_id", l.svcID,
		"remote", s.srcKey, "pairing_key", s.displayKey())
}

// listenerProto 返回某服务当前本地监听的协议（没有监听返回空串）。
func (c *client) listenerProto(serviceID string) string {
	c.listenerMu.Lock()
	defer c.listenerMu.Unlock()
	for _, l := range c.listeners {
		if l.svcID == serviceID {
			return l.proto
		}
	}
	return ""
}

// udpSessionKey 是本地 UDP 会话的索引：同一服务 + 同一第三方源地址。
type udpSessionKey struct {
	serviceID string
	src       string
}
