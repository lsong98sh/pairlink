package agent

import (
	"context"
	"net"
	"time"

	"github.com/pion/dtls/v3"

	"github.com/lsong98sh/pairlink/internal/pairing"
	"github.com/lsong98sh/pairlink/internal/udpdtls"
)

// 本文件是 agent 侧的原生 UDP 数据面 DTLS-PSK 客户端（v0.5.0）。
//
// 与 relay / client 使用完全相同的参数：
//
//	PSK = HKDF-SHA256(ikm=auth_token, salt=nil,
//	                  info="pairlink-udp-dtls/v1|"+pairing_key, L=32)
//
// PSK identity 固定为 pairing_key（relay 端据此派生同一把密钥，非法 identity
// 直接握手失败）。内部帧格式（len(4B)|sess_len(1B)|session|payload）与
// 授权逻辑（DTLS 内的 MsgBind）都不变，DTLS 只是把同一条 UDP 链路包起来。

// udpDTLSPacketConn 把「已 connected 的 UDP socket」适配成 dtls 需要的
// net.PacketConn：ReadFrom 返回对端地址，WriteTo 忽略地址（socket 已连接）。
type udpDTLSPacketConn struct{ conn *net.UDPConn }

// ReadFrom 实现 net.PacketConn。
func (c *udpDTLSPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, err := c.conn.Read(b)
	return n, c.conn.RemoteAddr(), err
}

// WriteTo 实现 net.PacketConn（忽略 addr）。
func (c *udpDTLSPacketConn) WriteTo(b []byte, _ net.Addr) (int, error) { return c.conn.Write(b) }

// Close 实现 net.PacketConn。
func (c *udpDTLSPacketConn) Close() error { return c.conn.Close() }

// LocalAddr 实现 net.PacketConn。
func (c *udpDTLSPacketConn) LocalAddr() net.Addr { return c.conn.LocalAddr() }

// SetDeadline 实现 net.PacketConn。
func (c *udpDTLSPacketConn) SetDeadline(t time.Time) error { return c.conn.SetDeadline(t) }

// SetReadDeadline 实现 net.PacketConn。
func (c *udpDTLSPacketConn) SetReadDeadline(t time.Time) error { return c.conn.SetReadDeadline(t) }

// SetWriteDeadline 实现 net.PacketConn。
func (c *udpDTLSPacketConn) SetWriteDeadline(t time.Time) error { return c.conn.SetWriteDeadline(t) }

// dialUDPDTLS 在已拨号的 relay UDP socket 上完成 DTLS-PSK 握手。
//
// 失败时连接已关闭，调用方直接放弃本次配对即可（下次数据报会重新握手）。
func (a *agent) dialUDPDTLS(raw *net.UDPConn, key pairing.PairingKey) (*dtls.Conn, error) {
	cfg := &dtls.Config{
		CipherSuites:    []dtls.CipherSuiteID{dtls.TLS_PSK_WITH_AES_128_GCM_SHA256},
		PSKIdentityHint: []byte(key.String()),
		PSK: func([]byte) ([]byte, error) {
			// 客户端回调收到的是 relay 的 identity hint（默认 nil）：PSK 只能由
			// 本地已知的 auth_token + 配对键派生，参数一律忽略。
			return udpdtls.Derive(a.cfg.Server.Token, key.String())
		},
	}
	conn, err := dtls.Client(&udpDTLSPacketConn{conn: raw}, raw.RemoteAddr(), cfg)
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	ctx, cancel := context.WithTimeout(a.ctx, a.pairTimeout())
	defer cancel()
	if err := conn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}
