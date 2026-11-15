package config

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
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

// RelayDataTLS 报告 relay 的 TCP 数据面（tcp_listen）最终是否使用 TLS。
//
// 语义（docs/architecture.md §7.11）：
//   - true：强制数据面 TLS（证书与隧道共用，Validate 要求证书成对）；
//   - false：强制数据面明文（即使隧道是 TLS）；
//   - auto / 零值：**跟随隧道判定**（tunnelTLS 为 true 时数据面也 TLS）。
//
// tunnelTLS 由调用方用 TLS.ListenTLS(cert, key)（已通过 Validate）计算。
func (m TLSMode) RelayDataTLS(tunnelTLS bool) bool {
	switch m.normalized() {
	case TLSTrue:
		return true
	case TLSFalse:
		return false
	default:
		return tunnelTLS
	}
}

// DataTLSNegotiation 是 agent/client 数据面 TLS 的判定结果。
//
// 输入是本地 [server]/[client] data_tls 与 relay 在 MsgHelloOK.data_tls 中
// 下发的值（nil 表示旧 relay 未下发该字段）。
type DataTLSNegotiation struct {
	// UseTLS 表示数据面连接是否应包 TLS。
	UseTLS bool
	// Warning 非空时调用方应打印一条 WARN（内容已包含原因与建议）。
	Warning string
	// Err 非空表示本地配置与 relay 不兼容，属**永久错误**（不应重连）：
	// 要么本地要求 TLS 而 relay 未启用，要么 relay 要求 TLS 而本地强制明文。
	Err error
}

// NegotiateDataTLS 计算拨号侧数据面是否使用 TLS（docs/contract.md §4.3.1）。
//
// 规则：
//   - data_tls = true：必须 TLS。relay 未下发 data_tls（旧 relay）或显式 false
//     都返回可读错误——不静默降级；
//   - data_tls = false：强制明文。relay 要求 TLS（data_tls=true）时返回错误，
//     同样不静默降级；
//   - data_tls = auto（默认）：跟随 relay 的 data_tls；旧 relay 未下发时按明文
//     处理并给出 WARN（这是 v0.3.1 及更早版本的行为）。
func NegotiateDataTLS(mode TLSMode, advertised *bool) DataTLSNegotiation {
	relayTLS := advertised != nil && *advertised
	switch mode.normalized() {
	case TLSTrue:
		switch {
		case advertised == nil:
			return DataTLSNegotiation{Err: fmt.Errorf(
				"relay 未下发 data_tls（旧 relay < v0.4.0），本端 data_tls = true 要求数据面必须 TLS；请把 relay 升级到 v0.4.0+ 并启用数据面 TLS，或把本端 data_tls 改为 auto/false")}
		case !relayTLS:
			return DataTLSNegotiation{Err: fmt.Errorf(
				"relay 未启用数据面 TLS（HelloOK.data_tls=false），本端 data_tls = true 要求必须 TLS；请在 relay 配置 [relay] data_tls = true（或 auto + 隧道 TLS）")}
		}
		return DataTLSNegotiation{UseTLS: true}
	case TLSFalse:
		if relayTLS {
			return DataTLSNegotiation{Err: fmt.Errorf(
				"relay 要求数据面 TLS（HelloOK.data_tls=true），但本端 data_tls = false；请把本端改为 data_tls = auto/true（并配置 tls_ca），或让 relay 显式关闭数据面 TLS")}
		}
		return DataTLSNegotiation{}
	default: // auto / 零值
		if advertised == nil {
			return DataTLSNegotiation{Warning: "relay 未下发 data_tls（旧 relay < v0.4.0），数据面按明文处理；建议升级 relay 并启用 [relay] data_tls 以保护业务数据与 pairing_token"}
		}
		return DataTLSNegotiation{UseTLS: relayTLS}
	}
}

// ClientTLSConfig 构造拨号侧 TLS 配置（隧道与数据面共用同一套规则）。
//
//   - MinVersion 固定 TLS 1.2（与 relay 监听侧一致）；
//   - ServerName 取 relay_addr 的 host（证书必须包含该主机名或 IP SAN）；
//   - caPath 非空时 RootCAs 只含该 CA；为空时保持 nil（系统根证书）；
//   - insecureSkipVerify 为 true 时跳过证书校验（仅测试环境，调用方负责 WARN）。
//
// 返回的 *tls.Config 在并发拨号中只读，可安全复用。
func ClientTLSConfig(caPath string, insecureSkipVerify bool, serverName string) (*tls.Config, error) {
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: serverName,
	}
	if insecureSkipVerify {
		cfg.InsecureSkipVerify = true
	}
	if ca := strings.TrimSpace(caPath); ca != "" {
		pem, err := os.ReadFile(ca)
		if err != nil {
			return nil, fmt.Errorf("读取 tls_ca %q: %w", ca, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("tls_ca %q 中没有可用的 PEM 证书", ca)
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}
