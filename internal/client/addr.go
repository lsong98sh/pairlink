package client

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
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

// dialTCPData 连接 relay 的 TCP 数据面（明文哑管道，永远不包 TLS）。
func (c *client) dialTCPData() (net.Conn, error) {
	neg := c.negotiated()
	if neg.tcpDataErr != nil {
		return nil, neg.tcpDataErr
	}
	if neg.tcpDataAddr == "" {
		return nil, errors.New("client: TCP 数据面地址未解析")
	}
	d := &net.Dialer{Timeout: c.dialTimeout()}
	conn, err := d.DialContext(c.ctx, "tcp", neg.tcpDataAddr)
	if err != nil {
		return nil, fmt.Errorf("client: 连接 relay TCP 数据面 %s: %w", neg.tcpDataAddr, err)
	}
	return conn, nil
}
