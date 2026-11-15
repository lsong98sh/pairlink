package relay

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync/atomic"
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
	// lastAuthWarn 是上一次令牌校验失败 WARN 的 UnixNano（限速用）。
	lastAuthWarn atomic.Int64
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
//
// 阶段 6C 的丢包语义（docs/protocol.md §5.2.1）：
//   - 半开状态收到**同一端点**重复的 MsgBind → 幂等，再回一次 MsgBindOK
//     （令牌只校验不消费），让 BindOK 丢包的 agent/client 能靠重发完成报到；
//   - Full 状态收到来自已知端点、且能被解析为**同一配对键** MsgBind 帧的数据报 →
//     丢弃（不得转发，否则会污染对端数据流），只记 debug 日志；
//   - 其它数据报照常按源地址转发，不做内容解析。
func (l *UDPDataListener) handleDatagram(data []byte, from *net.UDPAddr, log *slog.Logger) {
	ep, err := registry.UDPEndpointFromNetAddr(from)
	if err != nil {
		log.Debug("丢弃非法 UDP 源地址", "event", "udp_evict", "err", err)
		return
	}

	// 已登记端点：按源地址反查配对键。
	if key, peer, ok := l.srv.udpTable.LookupByEndpoint(ep); ok {
		if !peer.IsValid() {
			// 半会话：对端尚未报到。同一端点重复 Bind（BindOK 丢失后的重发）幂等，
			// 再回一次 BindOK；其它数据报继续丢弃（协议允许）。
			if bindKey, ok := parseBindKey(data); ok && bindKey == key {
				l.sendBindOK(ep, key, log, "半会话重复报到，幂等重发 MsgBindOK")
				l.srv.udpTable.Touch(key, time.Now())
			}
			return
		}

		// Full 会话：已知端点。只做 5 字节帧头嗅探，若是同一配对键的 MsgBind 帧则丢弃，
		// 避免它被当作业务数据转发给对端（那会污染数据流）。
		if bindKey, ok := parseBindKey(data); ok && bindKey == key {
			log.Debug("Full 会话收到重复的报到帧，已丢弃（不转发）",
				"event", "bind_dup_drop", "pairing_key", key.String(), "remote", ep.String())
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
	//
	// 数据报是不可信输入：ParseFrameLimit 先要求「声明长度 == 实际数据报长度」，
	// 再切出 payload；声明 1 MiB 的 5 字节数据报在这里直接被拒绝且不分配
	// 1 MiB（SEC-D1 / P0-1）。
	typ, payload, err := protocol.ParseFrameLimit(data, protocol.NetworkPayloadLimit)
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
		// 未认证端点可以在任何鉴权之前每包触发一次校验失败：每条都打 WARN
		// 会形成日志放大（≈200 B/包）。这里降为限速 WARN + Debug（P2-1 / SEC-M2）。
		l.warnAuthFail(log, ep, key, err)
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

	// 每次登记（含同一端点的幂等重复登记）都回 MsgBindOK：UDP 无连接，
	// 只有重复回才能覆盖 BindOK 丢包。
	l.sendBindOK(ep, key, log, "UDP 报到成功")

	log.Info("UDP 报到成功",
		"event", "bind_ok",
		"pairing_key", key.String(),
		"role", bind.Role,
		"full", full,
		"peer", peer.String(),
	)
}

// authFailWarnInterval 是 UDP 令牌校验失败 WARN 的最小间隔（其余降为 Debug）。
const authFailWarnInterval = 5 * time.Second

// warnAuthFail 以限速方式记录 UDP 报到令牌校验失败。
//
// UDP 数据面在读循环内处理未认证数据报；攻击者可用每包一条 WARN 打满日志。
// 这里每个 listener 每 5 秒最多一条 WARN，其余走 Debug（仍可排障）。
func (l *UDPDataListener) warnAuthFail(log *slog.Logger, ep registry.UDPEndpoint, key pairing.PairingKey, err error) {
	now := time.Now().UnixNano()
	last := l.lastAuthWarn.Load()
	if last == 0 || now-last >= int64(authFailWarnInterval) {
		if l.lastAuthWarn.CompareAndSwap(last, now) {
			log.Warn("UDP 报到令牌校验失败",
				"event", "bind_fail",
				"pairing_key", key.String(),
				"remote", ep.String(),
				"err", err,
			)
			return
		}
	}
	log.Debug("UDP 报到令牌校验失败",
		"event", "bind_fail",
		"pairing_key", key.String(),
		"remote", ep.String(),
		"err", err,
	)
}

// sendBindOK 向指定端点回一个 MsgBindOK 数据报。
func (l *UDPDataListener) sendBindOK(ep registry.UDPEndpoint, key pairing.PairingKey, log *slog.Logger, detail string) {
	bindOK, err := protocol.EncodeFrame(protocol.MsgBindOK, &protocol.BindOK{PairingKey: key.String()})
	if err != nil {
		log.Warn("编码 MsgBindOK 失败", "event", "internal", "err", err)
		return
	}
	if _, err := l.conn.WriteToUDPAddrPort(bindOK, ep.AddrPort()); err != nil {
		log.Debug("写 MsgBindOK 失败", "event", "bind_fail", "remote", ep.String(), "err", err)
		return
	}
	log.Debug(detail, "event", "bind_ok", "pairing_key", key.String(), "remote", ep.String())
}

// parseBindKey 只嗅探数据报的 5 字节帧头：当它是 MsgBind 帧时返回其中的配对键。
//
// 这是「Full 后丢弃重复 Bind」与「半会话重复 Bind 幂等」共用的最小解析：
// 绝大多数业务数据报第一个字节不是 MsgBind，直接返回 false，不做任何解析。
func parseBindKey(data []byte) (pairing.PairingKey, bool) {
	if len(data) < protocol.HeaderSize || protocol.MessageType(data[0]) != protocol.MsgBind {
		return pairing.PairingKey{}, false
	}
	// 数据报按实际长度解析：声明长度必须与收到的字节数一致（不做任何预分配）。
	typ, payload, err := protocol.ParseFrameLimit(data, protocol.NetworkPayloadLimit)
	if err != nil || typ != protocol.MsgBind {
		return pairing.PairingKey{}, false
	}
	msg, err := protocol.Decode(typ, payload)
	if err != nil {
		return pairing.PairingKey{}, false
	}
	bind, ok := msg.(*protocol.Bind)
	if !ok || bind.PairingKey == "" {
		return pairing.PairingKey{}, false
	}
	key, err := pairing.ParsePairingKey(bind.PairingKey)
	if err != nil {
		return pairing.PairingKey{}, false
	}
	return key, true
}

// close 关闭 UDP 数据面监听。
func (l *UDPDataListener) close() {
	if l.conn != nil {
		_ = l.conn.Close()
	}
}
