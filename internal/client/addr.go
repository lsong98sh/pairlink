package client

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// dataAddr 按 docs/contract.md §4.5 推导 relay 数据面拨号地址。
//
// host 来自 relay_addr 的 host；端口优先取 relay 在 HelloOK 中下发的
// tcp_data_port / udp_data_port；未下发（0，旧 relay）时回退到本 ini 的
// [relay] tcp_listen / udp_listen 的端口；仍取不到则返回错误。
//
// 该函数同时覆盖 IPv4、IPv6（自动加方括号）与域名三种 host 形式。
func dataAddr(host string, port int, fallbackListen string) (string, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return "", errors.New("client: relay_addr 缺少主机名，无法推导数据面地址")
	}
	if port <= 0 && strings.TrimSpace(fallbackListen) != "" {
		if _, p, err := net.SplitHostPort(strings.TrimSpace(fallbackListen)); err == nil {
			if n, convErr := strconv.Atoi(p); convErr == nil && n > 0 {
				port = n
			}
		}
	}
	if port <= 0 || port > 65535 {
		return "", fmt.Errorf("client: relay 未下发数据面端口，且本机 ini 未配置可回退的 [relay] 监听地址")
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

// dialTCPData 连接 relay 的 TCP 数据面。
//
// 本次连接是否包 TLS 由握手阶段协商（[client] data_tls × HelloOK.data_tls），
// 使用与隧道相同的 CA / insecure / SNI 规则；明文场景与 v0.3.1 行为一致。
// TCP 透传与 UDP over TCP 承载都走这里，保证两条路径加密语义一致。
func (c *client) dialTCPData() (net.Conn, error) {
	neg := c.negotiated()
	if neg.tcpDataErr != nil {
		return nil, neg.tcpDataErr
	}
	if neg.tcpDataAddr == "" {
		return nil, errors.New("client: TCP 数据面地址未解析")
	}
	if neg.dataTLS && neg.dataTLSCfg == nil {
		return nil, permanent(errors.New("client: 数据面要求 TLS 但本端 TLS 配置缺失（内部状态异常）"))
	}
	timeout := c.dialTimeout()
	d := &net.Dialer{Timeout: timeout}
	raw, err := d.DialContext(c.ctx, "tcp", neg.tcpDataAddr)
	if err != nil {
		return nil, fmt.Errorf("client: 连接 relay TCP 数据面 %s: %w", neg.tcpDataAddr, err)
	}
	if !neg.dataTLS {
		return raw, nil
	}
	tconn := tls.Client(raw, neg.dataTLSCfg)
	_ = tconn.SetDeadline(time.Now().Add(timeout))
	if err := tconn.HandshakeContext(c.ctx); err != nil {
		_ = raw.Close()
		// 数据面握手失败几乎总是配置不匹配（CA/SNI 不对，或 relay 实际未开 TLS）。
		return nil, permanent(fmt.Errorf(
			"client: 数据面 TLS 握手失败（relay 要求数据面 TLS：请检查本端 data_tls / tls_ca / tls_insecure_skip_verify 是否与 relay 证书匹配，relay 日志中可见 data_plane_tls_handshake_fail）: %w", err))
	}
	if err := tconn.SetDeadline(time.Time{}); err != nil {
		_ = tconn.Close()
		return nil, err
	}
	return tconn, nil
}
