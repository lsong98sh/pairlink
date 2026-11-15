package relay

import (
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"
)

// connWarnInterval 是连接上限 WARN 的最小间隔。
//
// 超限通常是持续攻击或配置过小：每条被拒连接都打 WARN 会自己把日志/磁盘打满，
// 因此按"每个 limiter 每 5 秒最多一条 WARN"限速，其余降为 Debug（SEC-D2）。
const connWarnInterval = 5 * time.Second

// connLimiter 限制隧道控制面与 TCP 数据面的并发连接总数与单 IP 并发数（SEC-D2）。
//
// 两个上限都按"0 = 不限制"处理；每次 acquire 成功必须配对一次 release。
// 隧道与 TCP 数据面共用同一个 limiter（总数与单 IP 计数都是两路之和）。
type connLimiter struct {
	max      int
	maxPerIP int

	mu    sync.Mutex
	total int
	perIP map[string]int

	// lastWarn 只在持锁时读写，用于限速 WARN。
	lastWarn time.Time
}

// newConnLimiter 创建连接限制器。
func newConnLimiter(max, maxPerIP int) *connLimiter {
	return &connLimiter{max: max, maxPerIP: maxPerIP, perIP: make(map[string]int)}
}

// remoteHost 提取来源 IP（失败时返回原始地址文本，仍可用于分桶）。
func remoteHost(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	s := addr.String()
	host, _, err := net.SplitHostPort(s)
	if err != nil {
		return strings.ToLower(s)
	}
	return strings.ToLower(strings.Trim(host, "[]"))
}

// acquire 尝试登记一条新连接：ok=false 时调用方必须立即关闭连接。
//
// reason 用于日志（"max_conns" / "max_conns_per_ip"）。
func (l *connLimiter) acquire(addr net.Addr) (host string, ok bool, reason string) {
	host = remoteHost(addr)
	if l == nil {
		return host, true, ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.max > 0 && l.total >= l.max {
		return host, false, "max_conns"
	}
	if l.maxPerIP > 0 && l.perIP[host] >= l.maxPerIP {
		return host, false, "max_conns_per_ip"
	}
	l.total++
	l.perIP[host]++
	return host, true, ""
}

// release 释放一条已登记连接（host 必须与 acquire 返回的一致）。
func (l *connLimiter) release(host string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.total > 0 {
		l.total--
	}
	if n, ok := l.perIP[host]; ok {
		if n <= 1 {
			delete(l.perIP, host)
		} else {
			l.perIP[host] = n - 1
		}
	}
}

// count 返回当前连接总数（测试与诊断用）。
func (l *connLimiter) count() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.total
}

// warnLimited 记录一条限速 WARN，返回是否真的写入了 WARN。
//
// 未到间隔时返回 false，调用方可选择写 Debug。
func (l *connLimiter) warnLimited(log *slog.Logger, addr net.Addr, reason string) bool {
	if l == nil || log == nil {
		return false
	}
	now := time.Now()
	l.mu.Lock()
	if !l.lastWarn.IsZero() && now.Sub(l.lastWarn) < connWarnInterval {
		l.mu.Unlock()
		return false
	}
	l.lastWarn = now
	l.mu.Unlock()

	log.Warn("连接数超过上限，已关闭新连接",
		"event", "conn_reject",
		"reason", reason,
		"remote", remoteHost(addr),
		"detail", "调整 [relay] max_conns / max_conns_per_ip（0 = 不限制）",
	)
	return true
}
