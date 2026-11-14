package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/lsong98sh/pairlink/internal/config"
)

// serverTLSConfig 按 docs/architecture.md §7.6 构造控制面 TLS 配置。
//
// 规则：
//   - tls_ca 非空 → RootCAs 只包含该 CA，ServerName 取 relay_addr 的 host；
//   - tls_ca 为空且未开启 insecure → 返回 nil，表示使用明文连接
//     （agent 的 ini 没有显式 tls 开关，明文是阶段 1/2 的默认部署方式，
//     需要 TLS 时配置 tls_ca 或 tls_insecure_skip_verify）；
//   - tls_insecure_skip_verify = true → 跳过校验并 WARN。
//
// 注：数据面永远是明文哑管道，不经过本函数。
func serverTLSConfig(s *config.ServerConfig, host string, log *slog.Logger) (*tls.Config, error) {
	if s.TLSCA == "" && !s.TLSInsecureSkipVerify {
		return nil, nil
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
	if s.TLSCA != "" {
		pem, err := os.ReadFile(s.TLSCA)
		if err != nil {
			return nil, fmt.Errorf("agent: 读取 tls_ca %q: %w", s.TLSCA, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("agent: tls_ca %q 中没有可用的 PEM 证书", s.TLSCA)
		}
		tc.RootCAs = pool
	}
	if s.TLSInsecureSkipVerify {
		tc.InsecureSkipVerify = true
		if log != nil {
			log.Warn("已跳过 relay 证书校验（tls_insecure_skip_verify = true）", "event", "config_warning",
				"detail", "仅建议测试环境使用")
		}
	}
	return tc, nil
}

// dialRelay 连接 relay 隧道端口（tcfg 为 nil 时明文）。
func (a *agent) dialRelay(tcfg *tls.Config) (net.Conn, error) {
	d := &net.Dialer{Timeout: a.dialTimeout()}
	raw, err := d.DialContext(a.ctx, "tcp", a.cfg.Server.RelayAddr)
	if err != nil {
		return nil, err
	}
	if tcfg == nil {
		return raw, nil
	}
	tconn := tls.Client(raw, tcfg)
	deadline := time.Now().Add(a.dialTimeout())
	if err := tconn.SetDeadline(deadline); err != nil {
		_ = raw.Close()
		return nil, err
	}
	if err := tconn.HandshakeContext(a.ctx); err != nil {
		_ = raw.Close()
		// TLS 握手失败几乎总是配置不匹配（relay 没开 TLS / CA 不对），重连无意义。
		return nil, permanent(fmt.Errorf("TLS 握手: %w", err))
	}
	if err := tconn.SetDeadline(time.Time{}); err != nil {
		_ = tconn.Close()
		return nil, err
	}
	return tconn, nil
}

// dialTCP 用给定超时拨号一个明文 TCP 连接（数据面或管理面之外的目标）。
func (a *agent) dialTCP(ctx context.Context, addr string, timeout time.Duration) (net.Conn, error) {
	d := &net.Dialer{Timeout: timeout}
	return d.DialContext(ctx, "tcp", addr)
}

// dialUDP 拨号一个 UDP 连接（connected socket，只收发该对端的数据报）。
func (a *agent) dialUDP(addr string) (*net.UDPConn, error) {
	raddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("解析 UDP 地址 %q: %w", addr, err)
	}
	d := &net.Dialer{Timeout: a.dialTimeout()}
	conn, err := d.DialContext(a.ctx, "udp", raddr.String())
	if err != nil {
		return nil, err
	}
	uc, ok := conn.(*net.UDPConn)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("拨号 UDP %q 返回了非 UDP 连接 %T", addr, conn)
	}
	return uc, nil
}

// splitRelayAddr 把 relay_addr 拆成 host 与端口。
func splitRelayAddr(addr string) (string, int, error) {
	host, portText, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return "", 0, fmt.Errorf("地址 %q 无法解析为 host:port: %w", addr, err)
	}
	if strings.TrimSpace(host) == "" {
		return "", 0, fmt.Errorf("地址 %q 缺少主机名", addr)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return "", 0, fmt.Errorf("地址 %q 的端口 %q 非法", addr, portText)
	}
	return host, port, nil
}

// dataAddr 由 relay 主机名与数据面端口推导拨号地址（docs/contract.md §4.5）。
//
// IPv4、IPv6 字面量与域名都用 net.JoinHostPort 处理（IPv6 自动加方括号）。
func dataAddr(host string, port int) (string, error) {
	if strings.TrimSpace(host) == "" {
		return "", errors.New("relay 主机名为空")
	}
	if port <= 0 || port > 65535 {
		return "", fmt.Errorf("数据面端口 %d 非法", port)
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

// listenPort 从 "host:port" 取出端口；无法解析或非法时返回 0。
//
// 用于 §4.5 的回退路径：HelloOK 未下发端口时，从本 ini 的 [relay] 段取。
func listenPort(addr string) int {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return 0
	}
	_, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return 0
	}
	return port
}

// positiveDuration 返回 d（d > 0）或 def。
func positiveDuration(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// heartbeatInterval 计算实际心跳间隔：min(本地配置, relay 建议值)，非正数回退 30s。
func heartbeatInterval(local, suggested time.Duration) time.Duration {
	if local <= 0 && suggested <= 0 {
		return defaultHeartbeat
	}
	if local <= 0 {
		return suggested
	}
	if suggested <= 0 {
		return local
	}
	if suggested < local {
		return suggested
	}
	return local
}

// isTimeout 报告 err 是否是超时错误。
func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return ne.Timeout()
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// isClosedErr 报告 err 是否是"连接已关闭/已 EOF"这类正常终止错误。
func isClosedErr(err error) bool {
	return err == nil ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, os.ErrClosed)
}
