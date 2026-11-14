package config

import (
	"fmt"
	"strings"
)

// TLSMode 是三态 TLS 开关，对应 ini 中 [relay] / [server] / [client] 的 tls 键。
//
// 语义（docs/architecture.md §7.6）：
//   - TLSAuto（默认）：完全保持阶段 6 之前的行为，按证书/CA 材料推断；
//   - TLSTrue：强制使用 TLS；relay 侧要求证书成对，拨号侧未配 tls_ca 时使用系统根证书；
//   - TLSFalse：强制明文；即使配置了证书/CA 也不启用 TLS（WARN 提示材料被忽略）。
//
// 零值（""）按 auto 处理，便于测试/调用方绕过 ini 直接构造 Config 时保持旧行为。
type TLSMode string

const (
	// TLSAuto 是默认值：按已有证书/CA 配置推断是否启用 TLS。
	TLSAuto TLSMode = "auto"
	// TLSTrue 表示强制使用 TLS。
	TLSTrue TLSMode = "true"
	// TLSFalse 表示强制明文。
	TLSFalse TLSMode = "false"
)

// ParseTLSMode 把字符串解析为 TLSMode（大小写不敏感，空串按 auto）。
//
// 只接受 auto / true / false；"on"/"yes"/"1" 等布尔别名一律拒绝，
// 避免把「显式 TLS 开关」误读成另一个布尔配置。
func ParseTLSMode(s string) (TLSMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return TLSAuto, nil
	case "true":
		return TLSTrue, nil
	case "false":
		return TLSFalse, nil
	default:
		return "", fmt.Errorf("不是合法的 tls 值 %q（只允许 auto / true / false）", s)
	}
}

// normalized 把零值归一化为 auto。
func (m TLSMode) normalized() TLSMode {
	if m == "" {
		return TLSAuto
	}
	return m
}

// Valid 报告 m 是否合法（零值按 auto 视为合法）。
func (m TLSMode) Valid() bool {
	switch m.normalized() {
	case TLSAuto, TLSTrue, TLSFalse:
		return true
	default:
		return false
	}
}

// String 实现 fmt.Stringer；零值输出 auto。
func (m TLSMode) String() string { return string(m.normalized()) }

// DialTLS 报告拨号侧（agent 的 [server] / client 的 [client]）是否应使用 TLS。
//
//   - true：始终 TLS（tls_ca 为空时由调用方使用系统根证书）；
//   - false：始终明文（忽略 tls_ca / tls_insecure_skip_verify）；
//   - auto / 零值：tls_ca 非空或 insecure=true 时 TLS，否则明文（阶段 1~6 的既有行为）。
func (m TLSMode) DialTLS(ca string, insecure bool) bool {
	switch m.normalized() {
	case TLSTrue:
		return true
	case TLSFalse:
		return false
	default:
		return strings.TrimSpace(ca) != "" || insecure
	}
}

// ListenTLS 报告 relay 侧是否应以 TLS 监听。
//
// 只有 auto/true 且 cert、key 均非空才返回 true；false 一律明文。
// true 但证书不完整属于配置错误，由 Validate 与 relay.listenTunnel 报错，
// 本方法只表达「材料是否足以启用 TLS」。
func (m TLSMode) ListenTLS(certPath, keyPath string) bool {
	if m.normalized() == TLSFalse {
		return false
	}
	return strings.TrimSpace(certPath) != "" && strings.TrimSpace(keyPath) != ""
}
