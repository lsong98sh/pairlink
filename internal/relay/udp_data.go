package relay

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/lsong98sh/pairlink/internal/logx"
	"github.com/lsong98sh/pairlink/internal/pairing"
	"github.com/lsong98sh/pairlink/internal/protocol"
	"github.com/lsong98sh/pairlink/internal/registry"
)

// UDPDataListener 是原生 UDP 数据面监听器（仅 udp_mode=native）。
//
// 单 goroutine 读循环：已登记端点直接按源地址反查转发；未知端点尝试按
// MsgBind 数据报完成报到。绝不为每个数据报起 goroutine。
type UDPDataListener struct {
	srv  *server
	conn *net.UDPConn
}

// loop 是唯一读循环。
func (l *UDPDataListener) loop() {
	defer l.srv.wg.Done()
	log := logx.Component(l.srv.log, "udp-data").With("listen", l.conn.LocalAddr().String())
	buf := make([]byte, 65535)

	for {
		n, from, err := l.conn.ReadFromUDP(buf)
		if err != nil {
			if l.srv.closing.Load() || l.srv.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			l.srv.fail(fmt.Errorf("relay: UDP 数据面读取: %w", err))
			return
		}
		l.handleDatagram(buf[:n], from, log)
	}
}

// handleDatagram 在单读循环内同步处理一个数据报。
func (l *UDPDataListener) handleDatagram(data []byte, from *net.UDPAddr, log *slog.Logger) {
	ep, err := registry.UDPEndpointFromNetAddr(from)
	if err != nil {
		log.Debug("丢弃非法 UDP 源地址", "event", "udp_evict", "err", err)
		return
	}

	// 已登记端点：直接转发（不再解析任何内容）。
	if key, peer, ok := l.srv.udpTable.LookupByEndpoint(ep); ok {
		if !peer.IsValid() {
			// 半会话：对端尚未报到，丢弃。
			return
		}
		l.srv.udpTable.Touch(key, time.Now())
		if _, err := l.conn.WriteToUDPAddrPort(data, peer.AddrPort()); err != nil {
			log.Debug("UDP 转发失败",
				"event", "udp_evict",
				"pairing_key", key.String(),
				"err", err,
			)
		}
		return
	}

	// 未知端点：只接受 MsgBind 数据报。
	typ, payload, err := protocol.ParseFrame(data)
	if err != nil || typ != protocol.MsgBind {
		log.Debug("丢弃未登记端点的非 Bind 数据报",
			"event", "udp_evict",
			"remote", ep.String(),
			"err", err,
		)
		return
	}
	raw, err := protocol.Decode(typ, payload)
	if err != nil {
		log.Debug("UDP Bind 解析失败", "event", "bind_fail", "remote", ep.String(), "err", err)
		return
	}
	bind, ok := raw.(*protocol.Bind)
	if !ok {
		return
	}
	side, ok := sideFromRole(bind.Role)
	if !ok {
		log.Debug("UDP Bind 角色非法", "event", "bind_fail", "role", bind.Role)
		return
	}
	key, err := pairing.ParsePairingKey(bind.PairingKey)
	if err != nil {
		log.Debug("UDP Bind 配对键非法", "event", "bind_fail", "remote", ep.String(), "err", err)
		return
	}
	if err := l.srv.tokens.Validate(key, bind.PairingToken); err != nil {
		log.Warn("UDP 报到令牌校验失败",
			"event", "bind_fail",
			"pairing_key", key.String(),
			"remote", ep.String(),
			"err", err,
		)
		return
	}

	full, peer, err := l.srv.udpTable.RegisterHalf(key, side, ep)
	if err != nil {
		log.Warn("UDP 端点登记失败",
			"event", "bind_fail",
			"pairing_key", key.String(),
			"remote", ep.String(),
			"err", err,
		)
		return
	}
	if full {
		l.srv.tokens.Consume(key)
	}

	// 只在首次登记（未知端点）时回一次 MsgBindOK。
	bindOK, err := protocol.EncodeFrame(protocol.MsgBindOK, &protocol.BindOK{PairingKey: key.String()})
	if err != nil {
		log.Warn("编码 MsgBindOK 失败", "event", "internal", "err", err)
		return
	}
	if _, err := l.conn.WriteToUDPAddrPort(bindOK, ep.AddrPort()); err != nil {
		log.Debug("写 MsgBindOK 失败", "event", "bind_fail", "remote", ep.String(), "err", err)
		return
	}

	log.Info("UDP 报到成功",
		"event", "bind_ok",
		"pairing_key", key.String(),
		"role", bind.Role,
		"full", full,
		"peer", peer.String(),
	)
}

// close 关闭 UDP 数据面监听。
func (l *UDPDataListener) close() {
	if l.conn != nil {
		_ = l.conn.Close()
	}
}
