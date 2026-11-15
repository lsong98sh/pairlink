// Package config 负责 PairLink 的 ini 配置解析、默认值填充与按模式校验。
//
// 设计要点：
//   - 三种模式共享同一份 ini，但各模式只读取自己的段（relay / server / client）
//     以及公共段 [common]；
//   - 未知段与未知键不报错，只记录到 Config.Warnings，由调用方打印；
//   - 所有超时使用 Go duration 字符串（如 10s、5m），解析后为 time.Duration；
//   - 证书/CA 路径使用 filepath 处理，相对路径相对于 ini 文件所在目录解析为绝对路径。
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lsong98sh/pairlink/internal/logx"
)

// Mode 是 PairLink 的运行模式，与命令行 -relay / -agent / -client 一一对应。
type Mode string

const (
	// ModeRelay 表示中继服务器模式。
	ModeRelay Mode = "relay"
	// ModeAgent 表示服务提供者模式。
	ModeAgent Mode = "agent"
	// ModeClient 表示访问侧模式。
	ModeClient Mode = "client"
)

// DefaultServiceID 是 [server] 段主服务（protocol + local）的固定服务ID。
const DefaultServiceID = "default"

// serviceIDPattern 限定 service.<ID> 中 ID 的合法字符与长度（1..64）。
var serviceIDPattern = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// ParseMode 把字符串解析为 Mode（大小写不敏感）。
func ParseMode(s string) (Mode, error) {
	m := Mode(strings.ToLower(strings.TrimSpace(s)))
	if !m.Valid() {
		return "", fmt.Errorf("未知模式 %q（可选 relay / agent / client）", s)
	}
	return m, nil
}

// Valid 报告 m 是否是合法模式。
func (m Mode) Valid() bool {
	switch m {
	case ModeRelay, ModeAgent, ModeClient:
		return true
	default:
		return false
	}
}

// String 实现 fmt.Stringer。
func (m Mode) String() string { return string(m) }

// Config 是一份完整的 ini 配置。
//
// 同一进程只会用到其中一个模式段，其余段仍会被解析但不参与校验。
type Config struct {
	// Path 是配置文件的绝对路径。
	Path string
	// Common 对应 [common] 段。
	Common CommonConfig
	// Relay 对应 [relay] 段。
	Relay RelayConfig
	// Server 对应 [server] 段。
	Server ServerConfig
	// Client 对应 [client] 段。
	Client ClientConfig
	// Warnings 收集解析与校验过程中的非致命提示，由调用方打印。
	Warnings []string
}

// CommonConfig 对应 [common] 段，三种模式共享。
type CommonConfig struct {
	// ManageListen 是管理页监听地址；为空表示不启动管理页。
	ManageListen string
	// Pprof 为 true 时在管理端口挂载 /debug/pprof/。
	Pprof bool
	// LogLevel 是日志级别：debug / info / warn / error，默认 info。
	LogLevel string
	// LogFormat 是日志格式：text / json，默认 text。
	LogFormat string
	// LogKeepDays 是日志日文件的保留天数：默认 7，0 = 永久保留，负数校验失败。
	// 只在 log_path 非空（写文件）时生效；文件名规则见 internal/logx（<base>-YYYY-MM-DD<ext>）。
	LogKeepDays int
}

// RelayConfig 对应 [relay] 段。
type RelayConfig struct {
	// TunnelListen 是隧道（控制面）监听地址，必填。
	TunnelListen string
	// TCPListen 是 TCP 数据面监听地址，必填。
	TCPListen string
	// UDPListen 是原生 UDP 数据面监听地址，可选。
	// 为空表示不监听 UDP 端口，UDP 数据面改走 TCP 通信端口的封装隧道
	// （udp_mode = tcp_tunnel）。
	UDPListen string
	// TLS 是三态 TLS 开关：auto（默认，按证书推断）| true（强制 TLS）| false（强制明文）。
	TLS TLSMode
	// DataTLS 是 TCP 数据面（tcp_listen）的三态 TLS 开关：
	// auto（默认，跟随隧道 TLS 判定）| true（强制 TLS）| false（强制明文）。
	DataTLS TLSMode
	// TLSCert 是隧道 TLS 证书路径；与 TLSKey 成对出现。
	TLSCert string
	// TLSKey 是隧道 TLS 私钥路径；与 TLSCert 成对出现。
	TLSKey string
	// AuthToken 是 agent/client 接入隧道时使用的共享密钥；为空时**只允许回环
	// 来源**接入（并 WARN），非回环来源一律回 auth_failed（SEC-A1）。
	AuthToken string
	// ServiceAllow 是允许注册的服务名白名单（[relay] service_allow，逗号分隔）。
	// 为空表示不限制（启动 WARN），非空时不在名单内的注册一律拒绝（SEC-A2）。
	ServiceAllow []string
	// MaxServicesPerAgent 是单个 agent 可注册的服务数上限（默认 64，0 = 不限）。
	MaxServicesPerAgent int
	// MaxConns 是隧道控制面与 TCP 数据面的并发连接上限，默认 1024；0 表示不限制。
	MaxConns int
	// MaxConnsPerIP 是单个来源 IP 的并发连接上限（含隧道与 TCP 数据面），
	// 默认 128；0 表示不限制。
	MaxConnsPerIP int
	// MaxPendingPairs 是待配对表 + 已签发令牌数的上限，默认 4096；超过后新的
	// ConnectRequest 回 too_many_pending。
	MaxPendingPairs int
	// PairTimeout 是待配对表超时，默认 10s。
	PairTimeout time.Duration
	// IdleTimeout 是数据面空闲超时，默认 300s。
	IdleTimeout time.Duration
	// HeartbeatTimeout 是控制面心跳判死超时，默认 90s。
	HeartbeatTimeout time.Duration
	// UDPSessionTimeout 是 UDP 会话超时，默认 60s。
	UDPSessionTimeout time.Duration
	// TokenTTL 是一次性配对令牌有效期，默认 15s。
	TokenTTL time.Duration
	// UDPDTLS 表示原生 UDP 数据面（udp_listen）使用 DTLS-PSK 加密，默认 true。
	//
	// false 时回退到 v0.4.0 的明文 UDP 数据面（混合版本过渡/排障用）。
	// 注意：默认值只在 Load（ini）路径填充；直接构造 Config 时零值为 false，
	// 即明文路径（与 Reconnect 的做法一致）。
	UDPDTLS bool
	// TokenRatePerIP 是单个来源 IP 每秒可签发的配对令牌数，默认 10；0 = 不限。
	TokenRatePerIP int
	// TokenBurstPerIP 是单个来源 IP 的令牌签发突发上限，默认 30；
	// token_rate_per_ip = 0 时该值被忽略。
	TokenBurstPerIP int
}

// ServerService 描述 [server] 段中的一个服务映射。
type ServerService struct {
	// ID 是服务ID（service.<ID> 中的 <ID>，或主服务的 default）。
	ID string
	// Proto 是协议类型，只允许 tcp / udp。
	Proto string
	// Addr 是本地目标地址 host:port（裸端口已补 127.0.0.1）。
	Addr string
}

// ServerConfig 对应 [server] 段（agent 模式读取）。
type ServerConfig struct {
	// RelayAddr 是 relay 隧道地址，必填。
	RelayAddr string
	// Token 是接入 relay 的共享密钥，必填。
	Token string
	// Protocol 是主服务协议 tcp / udp；Local 非空而 Protocol 为空时按 tcp 处理。
	Protocol string
	// Local 是主服务的本地目标地址；为空表示没有主服务。
	Local string
	// Services 是附加服务映射，按 ini 中出现顺序排列。
	Services []ServerService
	// TLS 是三态 TLS 开关：auto（默认，按 CA/insecure 推断）| true（强制 TLS）| false（强制明文）。
	TLS TLSMode
	// DataTLS 是数据面三态 TLS 开关：auto（默认，跟随 relay 下发）| true（必须 TLS）
	// | false（强制明文）。与 TLS 相互独立：隧道明文时数据面仍可要求 TLS。
	DataTLS TLSMode
	// TLSCA 是可选 CA 文件路径，用于校验 relay 证书。
	TLSCA string
	// TLSInsecureSkipVerify 为 true 时跳过 relay 证书校验（仅建议测试环境）。
	TLSInsecureSkipVerify bool
	// Heartbeat 是控制面心跳间隔，默认 30s。
	Heartbeat time.Duration
	// DialTimeout 是连接 relay 的超时，默认 10s。
	DialTimeout time.Duration
	// UDPSessionTimeout 是客户端侧 UDP 本地会话空闲清理时间，默认 60s。
	UDPSessionTimeout time.Duration
	// UDPDTLS 表示原生 UDP 数据面使用 DTLS-PSK 加密，默认 true（须与 relay 一致）。
	//
	// 注意：默认值只在 Load（ini）路径填充；直接构造 Config 时零值为 false。
	UDPDTLS bool
	// Reconnect 表示控制连接断开后是否自动重连，默认 true。
	//
	// 注意：默认值只在 Load（ini）路径填充；直接构造 Config 时零值为 false，
	// 即保持阶段 6C 之前的「断开即退出」旧行为。
	Reconnect bool
	// ReconnectMaxBackoff 是自动重连指数退避的上限，默认 60s，必须 > 0。
	ReconnectMaxBackoff time.Duration
	// PairTimeout 是等待数据面报到应答（MsgBindOK）的超时，默认 10s。
	//
	// P3-2：此前 agent 侧读的是 [relay] pair_timeout，而 agent 的 ini 通常没有
	// [relay] 段，配置被静默忽略、恒为 10s。现在改为 [server] pair_timeout。
	PairTimeout time.Duration
}

// All 返回主服务（ID 恒为 default，排在最前）与附加服务组成的完整列表。
//
// 没有配置主服务（Local 为空）时只返回附加服务。
func (c ServerConfig) All() []ServerService {
	out := make([]ServerService, 0, len(c.Services)+1)
	if c.Local != "" {
		proto := c.Protocol
		if proto == "" {
			proto = "tcp"
		}
		out = append(out, ServerService{ID: DefaultServiceID, Proto: proto, Addr: c.Local})
	}
	out = append(out, c.Services...)
	return out
}

// ClientService 描述 [client] 段中的一个本地监听映射。
type ClientService struct {
	// ID 是远端服务ID。
	ID string
	// Listen 是本机监听地址 host:port（缺 host 已补 127.0.0.1）。
	Listen string
}

// ClientConfig 对应 [client] 段（client 模式读取）。
type ClientConfig struct {
	// RelayAddr 是 relay 隧道地址，必填。
	RelayAddr string
	// Token 是接入 relay 的共享密钥，必填。
	Token string
	// Services 是本地监听映射，按 ini 中出现顺序排列。
	Services []ClientService
	// TLS 是三态 TLS 开关：auto（默认，按 CA/insecure 推断）| true（强制 TLS）| false（强制明文）。
	TLS TLSMode
	// DataTLS 是数据面三态 TLS 开关：auto（默认，跟随 relay 下发）| true（必须 TLS）
	// | false（强制明文）。与 TLS 相互独立：隧道明文时数据面仍可要求 TLS。
	DataTLS TLSMode
	// TLSCA 是可选 CA 文件路径，用于校验 relay 证书。
	TLSCA string
	// TLSInsecureSkipVerify 为 true 时跳过 relay 证书校验（仅建议测试环境）。
	TLSInsecureSkipVerify bool
	// Heartbeat 是控制面心跳间隔，默认 30s。
	Heartbeat time.Duration
	// Subscribe 表示是否订阅服务列表变更，默认 true。
	Subscribe bool
	// DialTimeout 是连接 relay 的超时，默认 10s。
	DialTimeout time.Duration
	// PairTimeout 是等待 relay 配对结果的超时，默认 10s。
	PairTimeout time.Duration
	// UDPSessionTimeout 是客户端侧 UDP 本地会话空闲清理时间，默认 60s。
	//
	// 与 [server] udp_session_timeout 对应；第一版曾用包内常量，阶段 4 起可配置。
	UDPSessionTimeout time.Duration
	// UDPDTLS 表示原生 UDP 数据面使用 DTLS-PSK 加密，默认 true（须与 relay 一致）。
	//
	// 注意：默认值只在 Load（ini）路径填充；直接构造 Config 时零值为 false。
	UDPDTLS bool
	// Reconnect 表示控制连接断开后是否自动重连，默认 true。
	//
	// 注意：默认值只在 Load（ini）路径填充；直接构造 Config 时零值为 false。
	Reconnect bool
	// ReconnectMaxBackoff 是自动重连指数退避的上限，默认 60s，必须 > 0。
	ReconnectMaxBackoff time.Duration
}

// newDefault 返回填好全部默认值的 Config。
func newDefault() *Config {
	return &Config{
		Common: CommonConfig{
			LogLevel:    "info",
			LogFormat:   "text",
			LogKeepDays: 7,
		},
		Relay: RelayConfig{
			TLS:                 TLSAuto,
			DataTLS:             TLSAuto,
			MaxServicesPerAgent: 64,
			MaxConns:            1024,
			MaxConnsPerIP:       128,
			MaxPendingPairs:     4096,
			PairTimeout:         10 * time.Second,
			IdleTimeout:         300 * time.Second,
			HeartbeatTimeout:    90 * time.Second,
			UDPSessionTimeout:   60 * time.Second,
			TokenTTL:            15 * time.Second,
			UDPDTLS:             true,
			TokenRatePerIP:      10,
			TokenBurstPerIP:     30,
		},
		Server: ServerConfig{
			TLS:                 TLSAuto,
			DataTLS:             TLSAuto,
			Heartbeat:           30 * time.Second,
			DialTimeout:         10 * time.Second,
			UDPSessionTimeout:   60 * time.Second,
			UDPDTLS:             true,
			Reconnect:           true,
			ReconnectMaxBackoff: 60 * time.Second,
			PairTimeout:         10 * time.Second,
		},
		Client: ClientConfig{
			TLS:                 TLSAuto,
			DataTLS:             TLSAuto,
			Heartbeat:           30 * time.Second,
			Subscribe:           true,
			DialTimeout:         10 * time.Second,
			PairTimeout:         10 * time.Second,
			UDPSessionTimeout:   60 * time.Second,
			UDPDTLS:             true,
			Reconnect:           true,
			ReconnectMaxBackoff: 60 * time.Second,
		},
	}
}

// Load 读取并解析 path 指向的 ini 配置文件。
//
// 未知段/未知键会被记录到 Config.Warnings 而不是报错；语法错误、非法 duration、
// 非法服务ID、service.default 冲突会直接返回错误。Load 只做与模式无关的解析，
// 模式相关的必填项检查由 Validate 完成。
func Load(path string) (*Config, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("配置文件路径为空")
	}
	abs, err := filepath.Abs(filepath.FromSlash(path))
	if err != nil {
		return nil, fmt.Errorf("解析配置文件路径 %q: %w", path, err)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件 %q: %w", abs, err)
	}
	f, err := parseINI(data)
	if err != nil {
		return nil, fmt.Errorf("解析配置文件 %q: %w", abs, err)
	}
	cfg := newDefault()
	cfg.Path = abs
	b := &builder{cfg: cfg, file: f, dir: filepath.Dir(abs)}
	if err := b.load(); err != nil {
		return nil, fmt.Errorf("配置文件 %q: %w", abs, err)
	}
	return cfg, nil
}

// builder 把 iniFile 填充进 Config，并收集警告。
type builder struct {
	cfg  *Config
	file *iniFile
	dir  string
}

// warnf 追加一条去重后的警告。
func (b *builder) warnf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	for _, w := range b.cfg.Warnings {
		if w == msg {
			return
		}
	}
	b.cfg.Warnings = append(b.cfg.Warnings, msg)
}

// str 返回指定段/键的字符串值（段或键不存在时返回空串）。
func (b *builder) str(section, key string) string {
	s, ok := b.file.section(section)
	if !ok {
		return ""
	}
	v, ok := s.get(key)
	if !ok {
		return ""
	}
	return v.Value
}

// raw 返回原始 iniValue，用于错误定位。
func (b *builder) raw(section, key string) (iniValue, bool) {
	s, ok := b.file.section(section)
	if !ok {
		return iniValue{}, false
	}
	return s.get(key)
}

// duration 解析 duration 键；键不存在或值为空时返回默认值。
func (b *builder) duration(section, key string, def time.Duration) (time.Duration, error) {
	v, ok := b.raw(section, key)
	if !ok || v.Value == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v.Value)
	if err != nil {
		return 0, fmt.Errorf("[%s] %s = %q 不是合法的 duration（例如 10s / 300s / 5m）", section, key, v.Value)
	}
	if d <= 0 {
		return 0, fmt.Errorf("[%s] %s = %q 必须大于 0", section, key, v.Value)
	}
	return d, nil
}

// boolean 解析布尔键；键不存在或值为空时返回默认值。
func (b *builder) boolean(section, key string, def bool) (bool, error) {
	v, ok := b.raw(section, key)
	if !ok || v.Value == "" {
		return def, nil
	}
	switch strings.ToLower(v.Value) {
	case "true", "1", "yes", "on":
		return true, nil
	case "false", "0", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("[%s] %s = %q 不是合法的布尔值（true/false）", section, key, v.Value)
	}
}

// booleanStrict 解析布尔键，只接受 true / false（大小写不敏感）。
//
// 与 boolean 的差别：不接受 1/0/yes/on 之类别名。udp_dtls 这类"加密开关"
// 的语义必须毫无歧义，写错值应当直接报错，而不是被猜成某个布尔值
// （否则两端取值不一致会退化成明文）。
func (b *builder) booleanStrict(section, key string, def bool) (bool, error) {
	v, ok := b.raw(section, key)
	if !ok || strings.TrimSpace(v.Value) == "" {
		return def, nil
	}
	switch strings.ToLower(strings.TrimSpace(v.Value)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("[%s] %s = %q 非法，只允许 true / false", section, key, v.Value)
	}
}

// integer 解析非负整数键；键不存在或值为空时返回默认值。
// 用于 max_conns / max_conns_per_ip / max_pending_pairs 这类上限（0 = 不限制）。
func (b *builder) integer(section, key string, def int) (int, error) {
	v, ok := b.raw(section, key)
	if !ok || strings.TrimSpace(v.Value) == "" {
		return def, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(v.Value))
	if err != nil {
		return def, fmt.Errorf("[%s] %s = %q 不是合法的整数", section, key, v.Value)
	}
	if n < 0 {
		return def, fmt.Errorf("[%s] %s = %q 不能为负数（0 表示不限制）", section, key, v.Value)
	}
	return n, nil
}

// tlsMode 解析三态 tls 键；键不存在或值为空时返回默认值。
func (b *builder) tlsMode(section, key string, def TLSMode) (TLSMode, error) {
	v, ok := b.raw(section, key)
	if !ok || strings.TrimSpace(v.Value) == "" {
		return def, nil
	}
	m, err := ParseTLSMode(v.Value)
	if err != nil {
		return def, fmt.Errorf("[%s] %s = %q: %w", section, key, v.Value, err)
	}
	return m, nil
}

// splitList 把逗号分隔的配置值拆成去空白、去空项的列表（保留原始顺序）。
//
// 用于 [relay] service_allow 这类白名单；重复项与合法性由 Validate 统一处理。
func splitList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// load 依次解析各段。
func (b *builder) load() error {
	if err := b.loadCommon(); err != nil {
		return err
	}
	if err := b.loadRelay(); err != nil {
		return err
	}
	if err := b.loadServer(); err != nil {
		return err
	}
	if err := b.loadClient(); err != nil {
		return err
	}
	b.warnUnknown()
	return nil
}

// loadCommon 解析 [common] 段。
func (b *builder) loadCommon() error {
	c := &b.cfg.Common
	c.ManageListen = b.str("common", "manage_listen")
	pprof, err := b.boolean("common", "pprof", false)
	if err != nil {
		return err
	}
	c.Pprof = pprof
	if v := b.str("common", "log_level"); v != "" {
		c.LogLevel = strings.ToLower(v)
	}
	if v := b.str("common", "log_format"); v != "" {
		c.LogFormat = strings.ToLower(v)
	}
	keepDays, err := b.integer("common", "log_keep_days", logx.DefaultKeepDays)
	if err != nil {
		return err
	}
	if err := logx.ValidateKeepDays(keepDays); err != nil {
		return err
	}
	c.LogKeepDays = keepDays
	return nil
}

// loadRelay 解析 [relay] 段。
func (b *builder) loadRelay() error {
	r := &b.cfg.Relay
	r.TunnelListen = b.str("relay", "tunnel_listen")
	r.TCPListen = b.str("relay", "tcp_listen")
	r.UDPListen = b.str("relay", "udp_listen")
	r.AuthToken = b.str("relay", "auth_token")
	r.TLSCert = b.absPath(b.str("relay", "tls_cert"))
	r.TLSKey = b.absPath(b.str("relay", "tls_key"))
	r.ServiceAllow = splitList(b.str("relay", "service_allow"))

	var err error
	if r.TLS, err = b.tlsMode("relay", "tls", r.TLS); err != nil {
		return err
	}
	if r.DataTLS, err = b.tlsMode("relay", "data_tls", r.DataTLS); err != nil {
		return err
	}
	if r.MaxServicesPerAgent, err = b.integer("relay", "max_services_per_agent", r.MaxServicesPerAgent); err != nil {
		return err
	}
	if r.MaxConns, err = b.integer("relay", "max_conns", r.MaxConns); err != nil {
		return err
	}
	if r.MaxConnsPerIP, err = b.integer("relay", "max_conns_per_ip", r.MaxConnsPerIP); err != nil {
		return err
	}
	if r.MaxPendingPairs, err = b.integer("relay", "max_pending_pairs", r.MaxPendingPairs); err != nil {
		return err
	}
	if r.PairTimeout, err = b.duration("relay", "pair_timeout", r.PairTimeout); err != nil {
		return err
	}
	if r.IdleTimeout, err = b.duration("relay", "idle_timeout", r.IdleTimeout); err != nil {
		return err
	}
	if r.HeartbeatTimeout, err = b.duration("relay", "heartbeat_timeout", r.HeartbeatTimeout); err != nil {
		return err
	}
	if r.UDPSessionTimeout, err = b.duration("relay", "udp_session_timeout", r.UDPSessionTimeout); err != nil {
		return err
	}
	if r.TokenTTL, err = b.duration("relay", "token_ttl", r.TokenTTL); err != nil {
		return err
	}
	if r.UDPDTLS, err = b.booleanStrict("relay", "udp_dtls", r.UDPDTLS); err != nil {
		return err
	}
	if r.TokenRatePerIP, err = b.integer("relay", "token_rate_per_ip", r.TokenRatePerIP); err != nil {
		return err
	}
	if r.TokenBurstPerIP, err = b.integer("relay", "token_burst_per_ip", r.TokenBurstPerIP); err != nil {
		return err
	}
	return nil
}

// loadServer 解析 [server] 段，包括 service.<ID> 点号键。
func (b *builder) loadServer() error {
	s := &b.cfg.Server
	s.RelayAddr = b.str("server", "relay_addr")
	s.Token = b.str("server", "token")
	s.Protocol = strings.ToLower(b.str("server", "protocol"))
	s.Local = normalizeLocalAddr(b.str("server", "local"))
	s.TLSCA = b.absPath(b.str("server", "tls_ca"))

	var err error
	if s.TLS, err = b.tlsMode("server", "tls", s.TLS); err != nil {
		return err
	}
	if s.DataTLS, err = b.tlsMode("server", "data_tls", s.DataTLS); err != nil {
		return err
	}
	if s.TLSInsecureSkipVerify, err = b.boolean("server", "tls_insecure_skip_verify", false); err != nil {
		return err
	}
	if s.Heartbeat, err = b.duration("server", "heartbeat", s.Heartbeat); err != nil {
		return err
	}
	if s.DialTimeout, err = b.duration("server", "dial_timeout", s.DialTimeout); err != nil {
		return err
	}
	if s.PairTimeout, err = b.duration("server", "pair_timeout", s.PairTimeout); err != nil {
		return err
	}
	if s.UDPSessionTimeout, err = b.duration("server", "udp_session_timeout", s.UDPSessionTimeout); err != nil {
		return err
	}
	if s.UDPDTLS, err = b.booleanStrict("server", "udp_dtls", s.UDPDTLS); err != nil {
		return err
	}
	if s.Reconnect, err = b.boolean("server", "reconnect", s.Reconnect); err != nil {
		return err
	}
	if s.ReconnectMaxBackoff, err = b.duration("server", "reconnect_max_backoff", s.ReconnectMaxBackoff); err != nil {
		return err
	}
	if s.Protocol == "" && s.Local != "" {
		s.Protocol = "tcp"
	}

	sec, ok := b.file.section("server")
	if !ok {
		return nil
	}
	for _, normKey := range sec.order {
		v := sec.values[normKey]
		id, isServiceKey := serviceKeyID(v.Key)
		if !isServiceKey {
			continue
		}
		if strings.EqualFold(id, DefaultServiceID) {
			return fmt.Errorf("[server] 中不允许显式配置 service.default，主服务请用 protocol + local 配置")
		}
		if err := validateServiceID(id); err != nil {
			return fmt.Errorf("[server] 键 %q: %w", v.Key, err)
		}
		proto, addr, err := parseServerServiceValue(v.Value)
		if err != nil {
			return fmt.Errorf("[server] 键 %q = %q: %w", v.Key, v.Value, err)
		}
		s.Services = upsertServerService(s.Services, ServerService{ID: id, Proto: proto, Addr: addr})
	}
	return nil
}

// loadClient 解析 [client] 段，包括 service.<ID> 点号键。
func (b *builder) loadClient() error {
	c := &b.cfg.Client
	c.RelayAddr = b.str("client", "relay_addr")
	c.Token = b.str("client", "token")
	c.TLSCA = b.absPath(b.str("client", "tls_ca"))

	var err error
	if c.TLS, err = b.tlsMode("client", "tls", c.TLS); err != nil {
		return err
	}
	if c.DataTLS, err = b.tlsMode("client", "data_tls", c.DataTLS); err != nil {
		return err
	}
	if c.TLSInsecureSkipVerify, err = b.boolean("client", "tls_insecure_skip_verify", false); err != nil {
		return err
	}
	if c.Heartbeat, err = b.duration("client", "heartbeat", c.Heartbeat); err != nil {
		return err
	}
	if c.Subscribe, err = b.boolean("client", "subscribe", c.Subscribe); err != nil {
		return err
	}
	if c.DialTimeout, err = b.duration("client", "dial_timeout", c.DialTimeout); err != nil {
		return err
	}
	if c.PairTimeout, err = b.duration("client", "pair_timeout", c.PairTimeout); err != nil {
		return err
	}
	if c.UDPSessionTimeout, err = b.duration("client", "udp_session_timeout", c.UDPSessionTimeout); err != nil {
		return err
	}
	if c.UDPDTLS, err = b.booleanStrict("client", "udp_dtls", c.UDPDTLS); err != nil {
		return err
	}
	if c.Reconnect, err = b.boolean("client", "reconnect", c.Reconnect); err != nil {
		return err
	}
	if c.ReconnectMaxBackoff, err = b.duration("client", "reconnect_max_backoff", c.ReconnectMaxBackoff); err != nil {
		return err
	}

	sec, ok := b.file.section("client")
	if !ok {
		return nil
	}
	for _, normKey := range sec.order {
		v := sec.values[normKey]
		id, isServiceKey := serviceKeyID(v.Key)
		if !isServiceKey {
			continue
		}
		if err := validateServiceID(id); err != nil {
			return fmt.Errorf("[client] 键 %q: %w", v.Key, err)
		}
		listen, err := normalizeListenAddr(v.Value)
		if err != nil {
			return fmt.Errorf("[client] 键 %q = %q: %w", v.Key, v.Value, err)
		}
		c.Services = upsertClientService(c.Services, ClientService{ID: id, Listen: listen})
	}
	return nil
}

// serviceKeyID 判断键是否形如 service.<ID>，返回 ID 与判断结果。
func serviceKeyID(rawKey string) (string, bool) {
	lower := strings.ToLower(rawKey)
	const prefix = "service."
	if !strings.HasPrefix(lower, prefix) {
		return "", false
	}
	return rawKey[len(prefix):], true
}

// validateServiceID 校验服务ID 的字符与长度限制。
func validateServiceID(id string) error {
	if id == "" {
		return fmt.Errorf("服务ID 为空")
	}
	if !serviceIDPattern.MatchString(id) {
		return fmt.Errorf("服务ID %q 非法，只允许 a-z0-9-_ 且长度 1..64", id)
	}
	return nil
}

// ValidateServiceID 是 validateServiceID 的公开版本，供管理页表单校验使用。
//
// 规则：非空、只允许 a-z0-9-_、长度 1..64（docs/contract.md §3.10）。
func ValidateServiceID(id string) error { return validateServiceID(id) }

// NormalizeListenAddr 是 normalizeListenAddr 的公开版本，供管理页表单校验使用。
//
// 语义：裸端口 / ":port" 补 127.0.0.1；host:port / [IPv6]:port 原样规范化；
// 端口必须 1..65535。返回值已用 net.JoinHostPort 规范化。
func NormalizeListenAddr(v string) (string, error) { return normalizeListenAddr(v) }

// upsertServerService 以 ID 为键写入服务映射：重复 ID 后者覆盖并留在首次出现位置。
func upsertServerService(list []ServerService, svc ServerService) []ServerService {
	for i := range list {
		if list[i].ID == svc.ID {
			list[i] = svc
			return list
		}
	}
	return append(list, svc)
}

// upsertClientService 以 ID 为键写入客户端监听映射：重复 ID 后者覆盖。
func upsertClientService(list []ClientService, svc ClientService) []ClientService {
	for i := range list {
		if list[i].ID == svc.ID {
			list[i] = svc
			return list
		}
	}
	return append(list, svc)
}

// absPath 把 ini 中的路径（允许 '/' 分隔）转换为绝对路径。
//
// 相对路径相对 ini 文件所在目录解析，避免受进程工作目录影响；
// service 启动时工作目录通常不是 ini 目录，这一点很关键。
func (b *builder) absPath(p string) string {
	p = strings.TrimSpace(filepath.FromSlash(p))
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(b.dir, p)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	return filepath.Clean(abs)
}

// parseServerServiceValue 解析 [server] 的 service.<ID> 值：
// <proto>:<port> 或 <proto>:<host>:<port>，裸端口时 host 为 127.0.0.1。
func parseServerServiceValue(v string) (proto, addr string, err error) {
	v = strings.TrimSpace(v)
	proto, rest, ok := strings.Cut(v, ":")
	if !ok {
		return "", "", fmt.Errorf("值必须形如 <proto>:<port> 或 <proto>:<host>:<port>")
	}
	proto = strings.ToLower(strings.TrimSpace(proto))
	if proto != "tcp" && proto != "udp" {
		return "", "", fmt.Errorf("协议 %q 非法，只允许 tcp / udp", proto)
	}
	rest = strings.TrimSpace(rest)
	norm, err := normalizeListenAddr(rest)
	if err != nil {
		return "", "", err
	}
	return proto, norm, nil
}

// normalizeLocalAddr 规范化本地目标地址：裸端口补 127.0.0.1。
func normalizeLocalAddr(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	norm, err := normalizeListenAddr(v)
	if err != nil {
		// 非法地址留待 Validate 报错，这里只做尽力规范化。
		return v
	}
	return norm
}

// normalizeListenAddr 规范化监听/目标地址：
//   - "8080" 或 ":8080" 补 host 127.0.0.1；
//   - "host:port" 原样返回（host 可为空表示所有网卡）；
//   - IPv6 需写成 "[::1]:8080"。
func normalizeListenAddr(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", fmt.Errorf("地址为空")
	}
	host, port, err := netSplitHostPort(v)
	if err != nil {
		return "", err
	}
	if host == "" {
		host = "127.0.0.1"
	}
	if err := validatePort(port); err != nil {
		return "", err
	}
	return joinHostPort(host, port), nil
}

// netSplitHostPort 是 net.SplitHostPort 的宽松版本：允许裸端口与 ":port"。
func netSplitHostPort(v string) (host, port string, err error) {
	if !strings.Contains(v, ":") {
		if _, convErr := strconv.Atoi(v); convErr != nil {
			return "", "", fmt.Errorf("地址 %q 无法解析为 host:port", v)
		}
		return "", v, nil
	}
	if strings.HasPrefix(v, ":") && strings.Count(v, ":") == 1 {
		return "", v[1:], nil
	}
	return splitHostPort(v)
}

// splitHostPort 包装 net.SplitHostPort 并附带可读错误。
func splitHostPort(v string) (string, string, error) {
	host, port, err := net.SplitHostPort(v)
	if err != nil {
		return "", "", fmt.Errorf("地址 %q 无法解析为 host:port: %w", v, err)
	}
	return host, port, nil
}

// joinHostPort 规范化 host:port 文本。
func joinHostPort(host, port string) string {
	return net.JoinHostPort(host, port)
}

// validatePort 校验端口范围 1..65535。
func validatePort(port string) error {
	if port == "" {
		return fmt.Errorf("端口为空")
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("端口 %q 不是数字", port)
	}
	if n < 1 || n > 65535 {
		return fmt.Errorf("端口 %d 超出范围 1..65535", n)
	}
	return nil
}
