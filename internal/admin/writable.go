package admin

import (
	"net"
	"strings"
)

// IsLoopbackListen 报告管理页监听地址是否只绑定回环接口。
//
// 阶段 7 的安全门禁：只有回环绑定（127.0.0.0/8、::1、localhost）才允许页面
// 写配置并写回 ini；绑定 0.0.0.0 / :: / 具体网卡地址时页面一律只读，
// 所有写 Action 返回 403（docs/contract.md §3.10）。
//
// 空地址、无法解析的主机名都按"非回环"处理（宁可拒绝）。
func IsLoopbackListen(addr string) bool {
	s := strings.TrimSpace(addr)
	if s == "" {
		return false
	}
	host, _, err := net.SplitHostPort(s)
	if err != nil {
		// 容忍不带端口的写法（配置校验会拒绝，但这里不能误判为可写）。
		host = s
	}
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// Writable 报告管理页是否允许写操作（由监听地址是否为回环决定）。
func (s *Server) Writable() bool { return s.writable }
