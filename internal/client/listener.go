package client

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/lsong98sh/pairlink/internal/protocol"
)

// localListener 是一个本地监听器：把本机端口映射到某个远端 service_id。
type localListener struct {
	svcID  string
	proto  string // "tcp" / "udp"
	listen string // ini 中配置的监听地址
	// bind 是实际绑定的地址（配置为 :0 时是内核分配的真实地址；热切换复用该地址）。
	bind string

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
	return l.bind
}

// close 关闭监听器（幂等）。
//
// 只关闭监听 socket：已 accept/已建立的会话拥有各自的连接，不受影响（允许自然结束）。
func (l *localListener) close() {
	if l.tcp != nil {
		_ = l.tcp.Close()
	}
	if l.udp != nil {
		_ = l.udp.Close()
	}
}

// newListener 打开一个本地监听（不启动 serve goroutine）。
//
// 测试可通过 c.listenFn 注入失败以验证热切换回滚。
func (c *client) newListener(proto, addr string) (*localListener, error) {
	if fn := c.listenFn; fn != nil {
		return fn(proto, addr)
	}
	return openLocal(proto, addr)
}

// openLocal 是真实的本地监听实现（TCP/UDP），测试注入时也可直接复用。
func openLocal(proto, addr string) (*localListener, error) {
	switch proto {
	case "udp":
		ua, err := net.ResolveUDPAddr("udp", addr)
		if err != nil {
			return nil, fmt.Errorf("解析本地 UDP 监听地址 %q: %w", addr, err)
		}
		conn, err := net.ListenUDP("udp", ua)
		if err != nil {
			return nil, fmt.Errorf("监听本地 UDP %s: %w", addr, err)
		}
		return &localListener{proto: "udp", listen: addr, bind: addr, udp: conn}, nil
	default:
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("监听本地 TCP %s: %w", addr, err)
		}
		return &localListener{proto: "tcp", listen: addr, bind: addr, tcp: ln}, nil
	}
}

// serveListener 为一个监听器启动 serve goroutine（TCP accept / UDP 读循环）。
func (c *client) serveListener(l *localListener) {
	if l == nil {
		return
	}
	if l.udp != nil {
		c.tasks.Go(func() { c.serveUDP(l) })
		return
	}
	c.tasks.Go(func() { c.serveTCP(l) })
}

// startListeners 按 cfg.Client.Services 逐个开本地监听。
//
// 协议以 relay 返回的服务列表为准（docs/contract.md §2.5 第 4 条）：
//   - 列表中存在该 service_id → 按 proto 开 TCP/UDP 监听；
//   - 不存在 → 按 TCP 开监听并记 WARN（event=service_missing），
//     ConnectRequest 失败时关闭本地连接；之后该服务出现且 proto 不同则由
//     订阅事件触发热切换（hotSwitchProto）；
//   - 列表中的 proto 未知 → 按 TCP 处理并 WARN。
//
// 任何本地监听失败（端口冲突等）都返回错误，由 run 结束进程，不静默跳过。
func (c *client) startListeners() error {
	// 与阶段 7 的管理页映射变更串行化：避免"初始监听尚未完成"时并发新增映射，
	// 导致初始扫描看到新映射后重复绑定同一地址并回滚掉它。
	c.mappingOpMu.Lock()
	defer c.mappingOpMu.Unlock()

	services, _ := c.servicesView()
	neg := c.negotiated()

	for _, svc := range c.mappingSnapshot() {
		// 管理页可能已经先一步为同一 service_id 打开了监听（动态新增），
		// 初始扫描不得重复绑定同一地址。
		if c.listenerByService(svc.ID) != nil {
			continue
		}
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
			if neg.udpMode == protocol.UDPModeNative && neg.udpDataErr != nil {
				// 数据面地址不可用是运行期问题：仍然开本地监听，
				// 等到真有接入时在配对阶段失败并关闭会话，Run 不退出。
				c.log.Warn("原生 UDP 数据面地址不可用，该服务的 UDP 接入将失败",
					"event", "listen", "service_id", svc.ID, "err", neg.udpDataErr)
			}
		} else if neg.tcpDataErr != nil {
			c.log.Warn("TCP 数据面地址不可用，该服务的接入将失败",
				"event", "listen", "service_id", svc.ID, "err", neg.tcpDataErr)
		}

		l, err := c.newListener(proto, svc.Listen)
		if err != nil {
			c.closeListeners()
			return fmt.Errorf("client: %w（服务 %s）", err, svc.ID)
		}
		l.svcID = svc.ID
		l.listen = svc.Listen
		c.addListener(l)
		c.serveListener(l)
		c.log.Info("本地监听已开启", "event", "listen", "service_id", svc.ID,
			"detail", fmt.Sprintf("proto=%s listen=%s addr=%s udp_mode=%s",
				l.proto, svc.Listen, l.actualAddr(), neg.udpMode))
	}
	return nil
}

// addListener 把监听器登记到管理页展示用的列表。
func (c *client) addListener(l *localListener) {
	c.listenerMu.Lock()
	c.listeners = append(c.listeners, l)
	c.listenerMu.Unlock()
}

// replaceListener 用 nl 原子替换 old（按指针匹配；找不到时不做任何事）。
func (c *client) replaceListener(old, nl *localListener) bool {
	c.listenerMu.Lock()
	defer c.listenerMu.Unlock()
	for i, l := range c.listeners {
		if l == old {
			c.listeners[i] = nl
			return true
		}
	}
	return false
}

// removeListener 移除指定监听器（热切换回滚也失败时避免管理页展示已关闭的监听）。
func (c *client) removeListener(target *localListener) {
	c.listenerMu.Lock()
	defer c.listenerMu.Unlock()
	out := c.listeners[:0]
	for _, l := range c.listeners {
		if l != target {
			out = append(out, l)
		}
	}
	c.listeners = out
}

// listenerByService 返回某服务当前的监听器（没有返回 nil）。
func (c *client) listenerByService(serviceID string) *localListener {
	c.listenerMu.Lock()
	defer c.listenerMu.Unlock()
	for _, l := range c.listeners {
		if l.svcID == serviceID {
			return l
		}
	}
	return nil
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

// reopenAddr 返回热切换时重新绑定用的地址。
//
// 配置为 ":0" 时用旧监听的实际地址（否则重开后会换端口，第三方必须重连别的端口，
// 违背「原地址按新 proto 重开」）；其余情况沿用配置地址。
func reopenAddr(l *localListener) string {
	if l == nil {
		return ""
	}
	if strings.HasSuffix(strings.TrimSpace(l.listen), ":0") {
		return l.actualAddr()
	}
	return l.listen
}

// hotSwitchProto 在服务协议变化时把本地监听热切换到新协议（TCP↔UDP）。
//
// 策略（docs/contract.md §2.5 第 4 条）：
//  1. 关闭旧监听 → 在**原地址**按新协议重开；成功则替换映射并记
//     event=listen_hot_switch；
//  2. 新监听打开失败（端口被占等）→ **回滚**：按旧协议在原地址重开并保留旧映射，
//     WARN 后继续用旧协议，绝不静默让本地服务消失；
//     只有连回滚都失败才移除映射并 WARN（本地服务确实不可用）；
//  3. 切换不误杀在途连接：关闭的只是监听 socket，已建立的会话各自持有连接，允许自然结束；
//     新连接走新协议；
//  4. 反复变化由 listenSwitchMu 串行化，关闭的监听器由各自的 serve goroutine 退出，
//     不会泄漏 listener/goroutine。
func (c *client) hotSwitchProto(serviceID, wantProto string) {
	want := strings.ToLower(strings.TrimSpace(wantProto))
	old := c.listenerByService(serviceID)
	if old == nil {
		return
	}
	if want != "tcp" && want != "udp" {
		// 未知协议：保持现状（与启动时「未知按 TCP」不同，运行期不冒险改变可用映射）。
		c.log.Warn("服务协议未知，保持当前本地监听", "event", "listen_hot_switch",
			"service_id", serviceID, "detail", "listen_proto="+old.proto+" relay_proto="+want)
		return
	}
	if old.proto == want {
		return
	}

	c.listenSwitchMu.Lock()
	defer c.listenSwitchMu.Unlock()

	// 加锁后重新确认（并发事件可能已经切换过）。
	old = c.listenerByService(serviceID)
	if old == nil || old.proto == want {
		return
	}
	addr := reopenAddr(old)
	oldProto := old.proto

	// 先关旧监听：同一地址不能同时按两种协议/两种 socket 类型绑定。
	old.close()
	nl, err := c.newListener(want, addr)
	if err != nil {
		// 回滚：按旧协议在原地址重开，保证本地服务不消失。
		rl, rerr := c.newListener(oldProto, addr)
		if rerr != nil {
			c.removeListener(old)
			c.log.Warn("本地监听热切换失败且回滚失败，该服务的本地监听已关闭",
				"event", "listen_hot_switch", "service_id", serviceID,
				"detail", fmt.Sprintf("from=%s to=%s listen=%s rollback=false", oldProto, want, addr),
				"err", rerr)
			return
		}
		rl.svcID = serviceID
		rl.listen = old.listen
		c.replaceListener(old, rl)
		c.serveListener(rl)
		c.log.Warn("本地监听热切换失败，已回滚到旧协议",
			"event", "listen_hot_switch", "service_id", serviceID,
			"detail", fmt.Sprintf("from=%s to=%s listen=%s rollback=true", oldProto, want, addr),
			"err", err)
		return
	}
	nl.svcID = serviceID
	nl.listen = old.listen
	c.replaceListener(old, nl)
	c.serveListener(nl)
	c.log.Info("本地监听已热切换协议", "event", "listen_hot_switch", "service_id", serviceID,
		"detail", fmt.Sprintf("from=%s to=%s listen=%s addr=%s",
			oldProto, want, old.listen, nl.actualAddr()))
}

// reconcileListeners 用当前服务列表校对本地监听协议（重连拿到新基线后调用）。
//
// 只处理「列表里明确给出了不同 proto」的服务；服务不在列表里时保留当前监听协议
// （service_missing 占位只用于首次启动，避免服务短暂消失导致来回切换）。
func (c *client) reconcileListeners() {
	c.mappingOpMu.Lock()
	defer c.mappingOpMu.Unlock()

	services, _ := c.servicesView()
	for _, svc := range c.mappingSnapshot() {
		info, ok := services[svc.ID]
		if !ok {
			continue
		}
		want := strings.ToLower(strings.TrimSpace(info.Proto))
		if want != "tcp" && want != "udp" {
			continue
		}
		c.hotSwitchProto(svc.ID, want)
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
			c.fatal(fmt.Errorf("client: 本地监听 %s 异常: %w", l.listen, err))
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
		c.controlWriteFailed(err)
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
		c.controlWriteFailed(err)
		return
	}
	c.log.Info("已发起接入请求", "event", "connect_request", "service_id", l.svcID,
		"remote", s.srcKey, "pairing_key", s.displayKey())
}

// listenerProto 返回某服务当前本地监听的协议（没有监听返回空串）。
func (c *client) listenerProto(serviceID string) string {
	if l := c.listenerByService(serviceID); l != nil {
		return l.proto
	}
	return ""
}

// udpSessionKey 是本地 UDP 会话的索引：同一服务 + 同一第三方源地址。
type udpSessionKey struct {
	serviceID string
	src       string
}
