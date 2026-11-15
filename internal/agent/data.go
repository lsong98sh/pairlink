package agent

import (
	"io"
	"net"
	"sync/atomic"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/forward"
	"github.com/lsong98sh/pairlink/internal/pairing"
	"github.com/lsong98sh/pairlink/internal/protocol"
)

// countingRW 统计一个方向上的字节数，并保留 CloseWrite 半关闭能力，
// 让 forward.Bidirectional 的正常半关闭语义不被破坏。
type countingRW struct {
	io.ReadWriteCloser
	read  *atomic.Int64
	write *atomic.Int64
}

// Read 读取数据并计入 read 计数。
func (c *countingRW) Read(p []byte) (int, error) {
	n, err := c.ReadWriteCloser.Read(p)
	if n > 0 && c.read != nil {
		c.read.Add(int64(n))
	}
	return n, err
}

// Write 写入数据并计入 write 计数。
func (c *countingRW) Write(p []byte) (int, error) {
	n, err := c.ReadWriteCloser.Write(p)
	if n > 0 && c.write != nil {
		c.write.Add(int64(n))
	}
	return n, err
}

// CloseWrite 透传半关闭能力；底层不支持时退化为整体关闭。
func (c *countingRW) CloseWrite() error {
	if cw, ok := c.ReadWriteCloser.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.ReadWriteCloser.Close()
}

// handleTCP 处理 proto=tcp 的配对：连 relay 的 tcp_listen，首帧 MsgBind（不等应答），
// 再连本地目标并做纯字节双向转发。
//
// TCP 数据面 relay 不回任何帧，因此写 Bind 后立刻开始转发，绝不读取确认。
func (a *agent) handleTCP(sess *localSession, key pairing.PairingKey, svc config.ServerService, token string) {
	defer a.endSession(sess)

	tcpDataAddr := a.tcpDataAddrSnapshot()
	if tcpDataAddr == "" {
		a.log.Warn("缺少 TCP 数据面地址，放弃本次配对", "event", "data_open",
			"pairing_key", key.String(), "detail", "tcp_data_addr 不可用")
		return
	}
	dataConn, err := a.dialDataPlane(a.ctx, tcpDataAddr, a.dialTimeout())
	if err != nil {
		a.log.Warn("连接 relay TCP 数据面失败，放弃本次配对", "event", "data_open",
			"pairing_key", key.String(), "remote", tcpDataAddr, "err", err)
		return
	}
	if !sess.track(dataConn) {
		return
	}
	sess.setCarrier(carrierTCP)
	sess.setRemote(remoteString(dataConn, tcpDataAddr))

	bind := &protocol.Bind{
		PairingKey:   key.String(),
		PairingToken: token,
		Role:         protocol.RoleAgent,
		Carrier:      protocol.CarrierTCP,
	}
	if err := protocol.WriteFrame(dataConn, protocol.MsgBind, bind); err != nil {
		a.log.Warn("发送数据面报到帧失败，放弃本次配对", "event", "bind",
			"pairing_key", key.String(), "err", err)
		return
	}
	a.log.Debug("已发送 TCP 数据面报到帧", "event", "bind",
		"pairing_key", key.String(), "remote", sess.carrierName())

	upstream, err := a.dialTCP(a.ctx, svc.Addr, a.dialTimeout())
	if err != nil {
		// 本地目标不可用只影响本次配对：关闭数据连接并返回，Run 继续运行。
		a.log.Warn("连接本地 TCP 目标失败，仅关闭本次配对", "event", "upstream_dial_failed",
			"pairing_key", key.String(), "service_id", svc.ID, "detail", "local="+svc.Addr, "err", err)
		return
	}
	if !sess.track(upstream) {
		return
	}
	sess.markReady()
	sess.touch()
	a.log.Info("TCP 数据面已建立", "event", "data_open",
		"pairing_key", key.String(),
		"service_id", svc.ID,
		"proto", "tcp",
		"remote", remoteString(dataConn, tcpDataAddr),
		"detail", "local="+svc.Addr,
	)

	// 计数：dataConn 上 Read 的是 relay→本地目标（bytesIn），Write 的是本地目标→relay（bytesOut）。
	relaySide := &countingRW{ReadWriteCloser: dataConn, read: &sess.bytesIn, write: &sess.bytesOut}
	forward.Bidirectional(a.ctx, relaySide, upstream)
}

// udpBindResendInterval 是原生 UDP 报到帧的重发间隔（约 300ms）。
const udpBindResendInterval = 300 * time.Millisecond

// udpBindMaxResends 是 pair_timeout 内最多重发报到帧的次数。
const udpBindMaxResends = 3

// handleUDPNative 处理 udp_mode=native 的配对：
// 向 relay 的 udp_listen 发 MsgBind 数据报、等 MsgBindOK（丢包时重发），
// 然后与本地 UDP 目标双向转发。
//
// 每个配对键一个本地 UDP 会话（两个 connected socket：一个到 relay、一个到本地目标）。
func (a *agent) handleUDPNative(sess *localSession, key pairing.PairingKey, svc config.ServerService, token string) {
	defer a.endSession(sess)

	udpDataAddr := a.udpDataAddrSnapshot()
	if udpDataAddr == "" {
		a.log.Warn("缺少 UDP 数据面地址，放弃本次 UDP 配对", "event", "data_open",
			"pairing_key", key.String(), "detail", "udp_data_addr 不可用")
		return
	}
	relayConn, err := a.dialUDP(udpDataAddr)
	if err != nil {
		a.log.Warn("连接 relay UDP 数据面失败，放弃本次配对", "event", "data_open",
			"pairing_key", key.String(), "remote", udpDataAddr, "err", err)
		return
	}
	if !sess.track(relayConn) {
		return
	}
	sess.setCarrier(carrierNative)
	sess.setRemote(remoteString(relayConn, udpDataAddr))

	localConn, err := a.dialUDP(svc.Addr)
	if err != nil {
		a.log.Warn("连接本地 UDP 目标失败，仅关闭本次配对", "event", "upstream_dial_failed",
			"pairing_key", key.String(), "service_id", svc.ID, "detail", "local="+svc.Addr, "err", err)
		return
	}
	if !sess.track(localConn) {
		return
	}

	frame, err := protocol.EncodeFrame(protocol.MsgBind, &protocol.Bind{
		PairingKey:   key.String(),
		PairingToken: token,
		Role:         protocol.RoleAgent,
	})
	if err != nil {
		a.log.Warn("编码 UDP 报到帧失败", "event", "bind", "pairing_key", key.String(), "err", err)
		return
	}
	if _, err := relayConn.Write(frame); err != nil {
		a.log.Warn("发送 UDP 报到数据报失败，放弃本次配对", "event", "bind",
			"pairing_key", key.String(), "err", err)
		return
	}
	sess.touch()

	if err := a.awaitBindOK(sess, key, relayConn, localConn, frame); err != nil {
		a.log.Warn("UDP 报到未被确认，放弃本次配对", "event", "bind",
			"pairing_key", key.String(), "remote", udpDataAddr, "err", err)
		return
	}
	sess.markReady()
	sess.touch()
	a.log.Info("UDP 数据面已建立", "event", "data_open",
		"pairing_key", key.String(),
		"service_id", svc.ID,
		"proto", "udp",
		"carrier", carrierNative,
		"remote", udpDataAddr,
		"detail", "local="+svc.Addr,
	)

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.udpLocalToRelay(sess, localConn, relayConn)
	}()
	a.udpRelayToLocal(sess, relayConn, localConn)
}

// awaitBindOK 在 pair_timeout 内等待 relay 的 MsgBindOK 数据报。
//
// 丢包处理（阶段 6C，docs/protocol.md §5.2.1）：每约 300ms 未收到应答就重发报到帧，
// 最多重发 udpBindMaxResends 次（重发只在 pair_timeout 内进行）；
// 收到 MsgBindOK 或**首个业务数据报**即视为就绪（relay 半开会重复回 BindOK，
// 因此重发是幂等的）。若先收到业务数据，先把该数据报转给本地目标。
func (a *agent) awaitBindOK(sess *localSession, key pairing.PairingKey, relayConn, localConn *net.UDPConn, frame []byte) error {
	timeout := a.pairTimeout()
	deadline := time.Now().Add(timeout)
	buf := make([]byte, udpDatagramSize)
	resends := 0
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return errWaitBindOK{timeout: timeout}
		}
		wait := remaining
		if resends < udpBindMaxResends && wait > udpBindResendInterval {
			wait = udpBindResendInterval
		}
		if err := relayConn.SetReadDeadline(time.Now().Add(wait)); err != nil {
			return err
		}
		n, err := relayConn.Read(buf)
		if err != nil {
			if !isTimeout(err) {
				return err
			}
			if resends < udpBindMaxResends {
				resends++
				if _, werr := relayConn.Write(frame); werr != nil {
					return werr
				}
				a.log.Debug("未收到 UDP 报到应答，重发报到帧", "event", "bind",
					"pairing_key", key.String(), "attempt", resends)
			}
			continue
		}
		sess.touch()
		if typ, payload, perr := protocol.ParseFrame(buf[:n]); perr == nil {
			switch typ {
			case protocol.MsgBindOK:
				if err := relayConn.SetReadDeadline(time.Time{}); err != nil {
					return err
				}
				a.log.Info("UDP 报到成功", "event", "bind", "pairing_key", key.String())
				return nil
			case protocol.MsgError:
				return errRelayRejected{reason: protocolErrText(payload)}
			}
		}
		// 当作业务数据：先落到本地目标，再进入正常转发。
		sess.bytesIn.Add(int64(n))
		if _, werr := localConn.Write(buf[:n]); werr != nil {
			return werr
		}
		if err := relayConn.SetReadDeadline(time.Time{}); err != nil {
			return err
		}
		a.log.Info("UDP 报到成功（先收到业务数据）", "event", "bind", "pairing_key", key.String())
		return nil
	}
}

// udpRelayToLocal 读 relay 数据报并转发给本地目标。
func (a *agent) udpRelayToLocal(sess *localSession, relayConn, localConn *net.UDPConn) {
	buf := make([]byte, udpDatagramSize)
	for {
		n, err := relayConn.Read(buf)
		if err != nil {
			if sess.isClosed() || a.ctx.Err() != nil || isClosedErr(err) {
				return
			}
			a.log.Warn("从 relay 读取 UDP 数据报失败，结束本次会话", "event", "data_close",
				"pairing_key", sess.key.String(), "err", err)
			return
		}
		sess.touch()
		sess.bytesIn.Add(int64(n))
		if _, err := localConn.Write(buf[:n]); err != nil {
			if sess.isClosed() || a.ctx.Err() != nil {
				return
			}
			a.log.Warn("写入本地 UDP 目标失败，结束本次会话", "event", "data_close",
				"pairing_key", sess.key.String(), "err", err)
			return
		}
	}
}

// udpLocalToRelay 读本地目标的 UDP 回包并写回 relay。
func (a *agent) udpLocalToRelay(sess *localSession, localConn, relayConn *net.UDPConn) {
	buf := make([]byte, udpDatagramSize)
	for {
		n, err := localConn.Read(buf)
		if err != nil {
			if sess.isClosed() || a.ctx.Err() != nil || isClosedErr(err) {
				return
			}
			a.log.Warn("从本地 UDP 目标读取失败，结束本次会话", "event", "data_close",
				"pairing_key", sess.key.String(), "err", err)
			return
		}
		sess.touch()
		sess.bytesOut.Add(int64(n))
		if _, err := relayConn.Write(buf[:n]); err != nil {
			if sess.isClosed() || a.ctx.Err() != nil {
				return
			}
			a.log.Warn("写回 relay UDP 数据报失败，结束本次会话", "event", "data_close",
				"pairing_key", sess.key.String(), "err", err)
			return
		}
	}
}

// remoteString 返回连接的对端地址，取不到时返回 fallback。
func remoteString(conn net.Conn, fallback string) string {
	if conn != nil {
		if addr := conn.RemoteAddr(); addr != nil {
			return addr.String()
		}
	}
	return fallback
}

// errWaitBindOK 表示等待 MsgBindOK 超时。
type errWaitBindOK struct{ timeout time.Duration }

// Error 实现 error。
func (e errWaitBindOK) Error() string {
	return "等待 MsgBindOK 超时（" + e.timeout.String() + "）"
}

// errRelayRejected 表示 relay 明确拒绝了本次数据面报到。
type errRelayRejected struct{ reason string }

// Error 实现 error。
func (e errRelayRejected) Error() string { return "relay 拒绝报到: " + e.reason }
