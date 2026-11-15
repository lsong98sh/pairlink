package config

import (
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/lsong98sh/pairlink/internal/logx"
)

// knownKeys 列出各段已知键，用于未知键警告。
var knownKeys = map[string]map[string]bool{
	"common": {
		"manage_listen": true,
		"pprof":         true,
		"log_level":     true,
		"log_format":    true,
		"log_keep_days": true,
	},
	"relay": {
		"tunnel_listen":          true,
		"tcp_listen":             true,
		"udp_listen":             true,
		"tls":                    true,
		"data_tls":               true,
		"tls_cert":               true,
		"tls_key":                true,
		"auth_token":             true,
		"service_allow":          true,
		"max_services_per_agent": true,
		"max_conns":              true,
		"max_conns_per_ip":       true,
		"max_pending_pairs":      true,
		"pair_timeout":           true,
		"idle_timeout":           true,
		"heartbeat_timeout":      true,
		"udp_session_timeout":    true,
		"token_ttl":              true,
		"udp_dtls":               true,
		"token_rate_per_ip":      true,
		"token_burst_per_ip":     true,
	},
	"server": {
		"relay_addr":               true,
		"token":                    true,
		"protocol":                 true,
		"local":                    true,
		"tls":                      true,
		"data_tls":                 true,
		"tls_ca":                   true,
		"tls_insecure_skip_verify": true,
		"heartbeat":                true,
		"dial_timeout":             true,
		"pair_timeout":             true,
		"udp_session_timeout":      true,
		"udp_dtls":                 true,
		"reconnect":                true,
		"reconnect_max_backoff":    true,
	},
	"client": {
		"relay_addr":               true,
		"token":                    true,
		"tls":                      true,
		"data_tls":                 true,
		"tls_ca":                   true,
		"tls_insecure_skip_verify": true,
		"heartbeat":                true,
		"subscribe":                true,
		"dial_timeout":             true,
		"pair_timeout":             true,
		"udp_session_timeout":      true,
		"udp_dtls":                 true,
		"reconnect":                true,
		"reconnect_max_backoff":    true,
	},
}

// warnUnknown 为未知段与未知键生成警告（不报错）。
func (b *builder) warnUnknown() {
	for _, name := range b.file.order {
		sec := b.file.sections[name]
		keys, knownSection := knownKeys[name]
		if !knownSection {
			b.warnf("未知配置段 [%s]，已忽略", sec.Name)
			continue
		}
		for _, normKey := range sec.order {
			v := sec.values[normKey]
			if keys[normKey] {
				continue
			}
			if (name == "server" || name == "client") && strings.HasPrefix(normKey, "service.") {
				continue
			}
			b.warnf("未知配置项 [%s] %s，已忽略", sec.Name, v.Key)
		}
	}
}

// Validate 按 mode 校验配置，只检查该模式实际使用的字段。
//
// 返回非 nil 表示配置不可用；非致命问题追加到 Config.Warnings。
func (c *Config) Validate(mode Mode) error {
	if !mode.Valid() {
		return fmt.Errorf("未知模式 %q", mode)
	}
	if err := c.validateCommon(); err != nil {
		return err
	}
	switch mode {
	case ModeRelay:
		return c.validateRelay()
	case ModeAgent:
		return c.validateServer()
	case ModeClient:
		return c.validateClient()
	default:
		return fmt.Errorf("未知模式 %q", mode)
	}
}

// addWarning 追加一条去重后的警告。
func (c *Config) addWarning(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	for _, w := range c.Warnings {
		if w == msg {
			return
		}
	}
	c.Warnings = append(c.Warnings, msg)
}

// checkTLSMode 校验三态 tls 取值并返回归一化后的模式。
func (c *Config) checkTLSMode(section string, m TLSMode) (TLSMode, error) {
	if !m.Valid() {
		return "", fmt.Errorf("%s tls = %q 非法，只允许 auto / true / false", section, string(m))
	}
	return m.normalized(), nil
}

// validateCommon 校验三种模式共享的 [common] 配置。
func (c *Config) validateCommon() error {
	switch c.Common.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("[common] log_level = %q 非法，只允许 debug / info / warn / error", c.Common.LogLevel)
	}
	switch c.Common.LogFormat {
	case "text", "json":
	default:
		return fmt.Errorf("[common] log_format = %q 非法，只允许 text / json", c.Common.LogFormat)
	}
	if err := logx.ValidateKeepDays(c.Common.LogKeepDays); err != nil {
		return err
	}
	if c.Common.ManageListen != "" {
		if err := checkListenAddr("[common] manage_listen", c.Common.ManageListen); err != nil {
			return err
		}
		if host, _, err := net.SplitHostPort(c.Common.ManageListen); err == nil {
			if host == "" || host == "0.0.0.0" || host == "::" {
				c.addWarning("[common] manage_listen = %q 监听所有网卡，管理页无鉴权，建议改为 127.0.0.1:端口", c.Common.ManageListen)
			}
		}
	}
	return nil
}

// validateRelay 校验 [relay] 段。
//
// tunnel_listen / tcp_listen 必填；udp_listen 可选：
//   - 配置了 udp_listen：启用原生 UDP 数据面（udp_mode = native）；
//   - 未配置 udp_listen：不监听 UDP 端口，UDP 数据面走 TCP 通信端口的封装隧道
//     （udp_mode = tcp_tunnel）。
func (c *Config) validateRelay() error {
	r := &c.Relay
	required := []struct{ name, value string }{
		{"tunnel_listen", r.TunnelListen},
		{"tcp_listen", r.TCPListen},
	}
	for _, f := range required {
		if strings.TrimSpace(f.value) == "" {
			return fmt.Errorf("[relay] %s 为必填项", f.name)
		}
		if err := checkListenAddr("[relay] "+f.name, f.value); err != nil {
			return err
		}
	}
	if strings.TrimSpace(r.UDPListen) != "" {
		if err := checkListenAddr("[relay] udp_listen", r.UDPListen); err != nil {
			return err
		}
		required = append(required, struct{ name, value string }{"udp_listen", r.UDPListen})
	} else {
		c.addWarning("[relay] 未配置 udp_listen，UDP 数据面将通过 TCP 通信端口的封装隧道承载（udp_mode = tcp_tunnel）")
	}

	seen := map[string]string{}
	for _, f := range required {
		key := canonicalAddr(f.value)
		if prev, ok := seen[key]; ok {
			return fmt.Errorf("[relay] %s 与 %s 监听地址重复（%s）", prev, f.name, f.value)
		}
		seen[key] = f.name
	}

	mode, err := c.checkTLSMode("[relay]", r.TLS)
	if err != nil {
		return err
	}
	hasCert := strings.TrimSpace(r.TLSCert) != ""
	hasKey := strings.TrimSpace(r.TLSKey) != ""
	tunnelLoopback := isLoopbackListenAddr(r.TunnelListen)
	switch mode {
	case TLSFalse:
		if hasCert || hasKey {
			c.addWarning("[relay] 已配置 tls_cert / tls_key 但 tls = false，隧道按明文监听（证书被忽略）")
		}
		if !tunnelLoopback {
			c.addWarning("[relay] tls = false 且 tunnel_listen = %q 绑定非回环地址：隧道（含 auth_token 与全部信令）在公网上是明文，请确认已评估风险", r.TunnelListen)
		}
	case TLSTrue:
		if !hasCert || !hasKey {
			return fmt.Errorf("[relay] tls = true 但缺少 tls_cert / tls_key：强制 TLS 必须同时配置证书与私钥路径")
		}
	default: // auto：按证书材料推断；非回环监听不再允许静默降级为明文（SEC-T2）
		switch {
		case !hasCert && !hasKey:
			if !tunnelLoopback {
				return fmt.Errorf("[relay] tls = auto 且未配置 tls_cert / tls_key，但 tunnel_listen = %q 绑定非回环地址：公网监听必须配置证书（tls = true + tls_cert/tls_key），或显式写 tls = false 承担明文风险", r.TunnelListen)
			}
			c.addWarning("[relay] 未配置 tls_cert / tls_key，隧道将以明文运行（当前 tunnel_listen 仅绑定回环，仅供本机调试；公网部署请配置证书并把 tls 设为 true）")
		case hasCert != hasKey:
			if !tunnelLoopback {
				return fmt.Errorf("[relay] tls = auto 但 tls_cert 与 tls_key 未成对配置，且 tunnel_listen = %q 绑定非回环地址：请补齐证书对（tls = true），或显式写 tls = false 承担明文风险", r.TunnelListen)
			}
			c.addWarning("[relay] tls_cert 与 tls_key 必须成对配置，当前按明文隧道运行（tunnel_listen 仅绑定回环）")
		}
	}
	// tls=false 时证书材料被显式忽略，不再检查文件是否存在（避免误导）。
	if mode != TLSFalse {
		if hasCert {
			warnFileMissing(c, "tls_cert", r.TLSCert)
		}
		if hasKey {
			warnFileMissing(c, "tls_key", r.TLSKey)
		}
	}

	// 数据面 TLS（SEC-T1）：auto 跟随隧道判定，true 强制，false 强制明文。
	dataMode, err := c.checkTLSMode("[relay] data_tls", r.DataTLS)
	if err != nil {
		return err
	}
	r.DataTLS = dataMode
	tunnelTLS := mode != TLSFalse && hasCert && hasKey
	if dataMode == TLSTrue && (!hasCert || !hasKey) {
		return fmt.Errorf("[relay] data_tls = true 但缺少 tls_cert / tls_key：数据面 TLS 与隧道共用证书，必须同时配置证书与私钥路径")
	}
	dataTLS := dataMode.RelayDataTLS(tunnelTLS)
	if dataTLS && mode == TLSFalse {
		// tls=false 跳过了上面的文件检查，但数据面 TLS 仍要读证书。
		if hasCert {
			warnFileMissing(c, "tls_cert", r.TLSCert)
		}
		if hasKey {
			warnFileMissing(c, "tls_key", r.TLSKey)
		}
		c.addWarning("[relay] tls = false 但 data_tls = true：隧道（含 auth_token 与信令）仍为明文，只有 TCP 数据面使用 TLS")
	}
	if dataMode == TLSFalse && tunnelTLS {
		c.addWarning("[relay] data_tls = false：TCP 数据面按明文运行（业务数据与 pairing_token 可被链路上的人读取），隧道 TLS 只保护控制面")
	}
	if dataTLS && strings.TrimSpace(r.UDPListen) != "" && !r.UDPDTLS {
		c.addWarning("[relay] 数据面 TLS 只覆盖 TCP 数据面：已配置 udp_listen 且 udp_dtls = false，原生 UDP 数据面仍为明文；如需加密请设置 udp_dtls = true（DTLS-PSK）或移除 udp_listen 改用 udp_mode = tcp_tunnel")
	}
	if strings.TrimSpace(r.UDPListen) != "" && !r.UDPDTLS {
		c.addWarning("[relay] udp_dtls = false：原生 UDP 数据面（udp_listen）按 v0.4.0 明文运行，业务数据与 pairing_token 可被链路上的人读取；仅用于与旧版本混合过渡，生产环境请保持 udp_dtls = true")
	}

	// SEC-A2：服务名白名单与单 agent 服务数上限。
	allow := make([]string, 0, len(r.ServiceAllow))
	seenAllow := make(map[string]struct{}, len(r.ServiceAllow))
	for _, id := range r.ServiceAllow {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if err := validateServiceID(id); err != nil {
			return fmt.Errorf("[relay] service_allow 中的 %q 非法: %w", id, err)
		}
		if _, dup := seenAllow[id]; dup {
			continue
		}
		seenAllow[id] = struct{}{}
		allow = append(allow, id)
	}
	r.ServiceAllow = allow
	if len(allow) == 0 {
		c.addWarning("[relay] 未配置 service_allow：任何持有 auth_token 的接入者都可以注册任意服务名（SEC-A2）；多租户/公网部署建议显式列出允许的服务名")
	}
	if r.MaxServicesPerAgent < 0 {
		return fmt.Errorf("[relay] max_services_per_agent = %d 不能为负数（0 表示不限制）", r.MaxServicesPerAgent)
	}
	// SEC-A1：空 auth_token 等价于"谁都能接入"，只允许配合回环监听做本机调试。
	if strings.TrimSpace(r.AuthToken) == "" {
		if !tunnelLoopback {
			return fmt.Errorf("[relay] auth_token 为空且 tunnel_listen = %q 绑定非回环地址：任何能访问该端口的人都能以 agent/client 身份接入。请配置 auth_token（例如 openssl rand -hex 32），或把 tunnel_listen 绑定到回环地址（127.0.0.1:端口）仅供本机调试", r.TunnelListen)
		}
		c.addWarning("[relay] auth_token 为空：仅允许回环地址（127.0.0.0/8、::1）的 agent/client 接入隧道，非回环来源一律拒绝（仅供本机调试）")
	}
	// SEC-D2：连接/待配对上限（0 表示不限制）。ini 解析已拒绝负数，
	// 这里兜住「绕过 ini 直接构造 Config」的情形。
	limits := []struct {
		name  string
		value int
	}{
		{"max_conns", r.MaxConns},
		{"max_conns_per_ip", r.MaxConnsPerIP},
		{"max_pending_pairs", r.MaxPendingPairs},
	}
	for _, l := range limits {
		if l.value < 0 {
			return fmt.Errorf("[relay] %s = %d 不能为负数（0 表示不限制）", l.name, l.value)
		}
	}
	// v0.5.0：按来源 IP 的令牌签发限速。rate = 0 表示不限速（此时 burst 被忽略）。
	if r.TokenRatePerIP < 0 {
		return fmt.Errorf("[relay] token_rate_per_ip = %d 不能为负数（0 表示不限制）", r.TokenRatePerIP)
	}
	if r.TokenBurstPerIP < 0 {
		return fmt.Errorf("[relay] token_burst_per_ip = %d 不能为负数", r.TokenBurstPerIP)
	}
	if r.TokenRatePerIP > 0 && r.TokenBurstPerIP < 1 {
		return fmt.Errorf("[relay] token_rate_per_ip = %d 时 token_burst_per_ip 必须 >= 1（当前 %d）", r.TokenRatePerIP, r.TokenBurstPerIP)
	}
	if c.Common.ManageListen != "" {
		manageKey := canonicalAddr(c.Common.ManageListen)
		for _, f := range required {
			if canonicalAddr(f.value) == manageKey {
				c.addWarning("[relay] manage_listen 与 %s 端口相同，管理页将无法监听", f.name)
			}
		}
	}
	return nil
}

// validateServer 校验 [server] 段。
func (c *Config) validateServer() error {
	s := &c.Server
	if strings.TrimSpace(s.RelayAddr) == "" {
		return fmt.Errorf("[server] relay_addr 为必填项")
	}
	if err := checkDialAddr("[server] relay_addr", s.RelayAddr); err != nil {
		return err
	}
	if strings.TrimSpace(s.Token) == "" {
		return fmt.Errorf("[server] token 为必填项")
	}
	services := s.All()
	if len(services) == 0 {
		return fmt.Errorf("[server] 至少要配置一个服务：主服务（protocol + local）或 service.<ID>")
	}
	ids := map[string]bool{}
	for _, svc := range services {
		if ids[svc.ID] {
			return fmt.Errorf("[server] 服务ID %q 重复", svc.ID)
		}
		ids[svc.ID] = true
		if svc.Proto != "tcp" && svc.Proto != "udp" {
			return fmt.Errorf("[server] 服务 %q 的协议 %q 非法，只允许 tcp / udp", svc.ID, svc.Proto)
		}
		if svc.Addr == "" {
			return fmt.Errorf("[server] 服务 %q 缺少本地目标地址", svc.ID)
		}
		if err := checkDialAddr("[server] 服务 "+svc.ID+" 的本地地址", svc.Addr); err != nil {
			return err
		}
	}
	mode, err := c.checkTLSMode("[server]", s.TLS)
	if err != nil {
		return err
	}
	dataMode, err := c.checkTLSMode("[server] data_tls", s.DataTLS)
	if err != nil {
		return err
	}
	s.DataTLS = dataMode
	if mode == TLSFalse {
		if s.TLSCA != "" || s.TLSInsecureSkipVerify {
			c.addWarning("[server] 已配置 tls_ca / tls_insecure_skip_verify 但 tls = false，控制面按明文拨号（上述配置被忽略；数据面是否 TLS 由 data_tls 决定，若判定为 TLS 仍会使用上述材料）")
		}
	} else {
		if s.TLSCA != "" {
			warnFileMissing(c, "tls_ca", s.TLSCA)
		}
		if s.TLSInsecureSkipVerify {
			if mode == TLSTrue {
				c.addWarning("[server] tls = true 且 tls_insecure_skip_verify = true：该开关多余但会跳过 relay 证书校验（仅建议测试环境）")
			} else {
				c.addWarning("[server] tls_insecure_skip_verify = true，将跳过 relay 证书校验（仅建议测试环境）")
			}
		}
	}
	switch dataMode {
	case TLSTrue:
		if s.TLSCA == "" && !s.TLSInsecureSkipVerify {
			c.addWarning("[server] data_tls = true 且未配置 tls_ca：数据面 TLS 将用系统根证书校验 relay 证书（与隧道一致）")
		}
	case TLSFalse:
		c.addWarning("[server] data_tls = false：数据面按明文运行；若 relay 要求数据面 TLS，拨号会明确失败而不是静默降级")
	}
	// duration 解析阶段已拒绝 <= 0；这里兜住「绕过 ini 直接构造 Config」的情形。
	if s.ReconnectMaxBackoff < 0 {
		return fmt.Errorf("[server] reconnect_max_backoff = %s 必须大于 0", s.ReconnectMaxBackoff)
	}
	return nil
}

// validateClient 校验 [client] 段。
func (c *Config) validateClient() error {
	cl := &c.Client
	if strings.TrimSpace(cl.RelayAddr) == "" {
		return fmt.Errorf("[client] relay_addr 为必填项")
	}
	if err := checkDialAddr("[client] relay_addr", cl.RelayAddr); err != nil {
		return err
	}
	if strings.TrimSpace(cl.Token) == "" {
		return fmt.Errorf("[client] token 为必填项")
	}
	if len(cl.Services) == 0 {
		c.addWarning("[client] 未配置 service.<ID>，client 只会拉取服务列表，不会开启本地监听")
	}
	ids := map[string]bool{}
	seen := map[string]string{}
	for _, svc := range cl.Services {
		if ids[svc.ID] {
			return fmt.Errorf("[client] 服务ID %q 重复", svc.ID)
		}
		ids[svc.ID] = true
		if err := checkListenAddr("[client] 服务 "+svc.ID+" 的本地监听", svc.Listen); err != nil {
			return err
		}
		key := canonicalAddr(svc.Listen)
		if prev, ok := seen[key]; ok {
			return fmt.Errorf("[client] 服务 %q 与 %q 的本地监听地址重复（%s）", prev, svc.ID, svc.Listen)
		}
		seen[key] = svc.ID
	}
	// 通配地址与具体地址同端口在绑定时会冲突，给出警告。
	for key, id := range seen {
		host, port, err := net.SplitHostPort(key)
		if err != nil {
			continue
		}
		if host != "0.0.0.0" && host != "::" {
			continue
		}
		for otherKey, otherID := range seen {
			if otherKey == key {
				continue
			}
			otherHost, otherPort, err := net.SplitHostPort(otherKey)
			if err != nil || otherPort != port || otherHost == host {
				continue
			}
			c.addWarning("[client] 服务 %q（%s）与服务 %q（%s）监听同一端口，可能绑定冲突", id, key, otherID, otherKey)
		}
	}
	mode, err := c.checkTLSMode("[client]", cl.TLS)
	if err != nil {
		return err
	}
	dataMode, err := c.checkTLSMode("[client] data_tls", cl.DataTLS)
	if err != nil {
		return err
	}
	cl.DataTLS = dataMode
	if mode == TLSFalse {
		if cl.TLSCA != "" || cl.TLSInsecureSkipVerify {
			c.addWarning("[client] 已配置 tls_ca / tls_insecure_skip_verify 但 tls = false，控制面按明文拨号（上述配置被忽略；数据面是否 TLS 由 data_tls 决定，若判定为 TLS 仍会使用上述材料）")
		}
	} else {
		if cl.TLSCA != "" {
			warnFileMissing(c, "tls_ca", cl.TLSCA)
		}
		if cl.TLSInsecureSkipVerify {
			if mode == TLSTrue {
				c.addWarning("[client] tls = true 且 tls_insecure_skip_verify = true：该开关多余但会跳过 relay 证书校验（仅建议测试环境）")
			} else {
				c.addWarning("[client] tls_insecure_skip_verify = true，将跳过 relay 证书校验（仅建议测试环境）")
			}
		}
	}
	switch dataMode {
	case TLSTrue:
		if cl.TLSCA == "" && !cl.TLSInsecureSkipVerify {
			c.addWarning("[client] data_tls = true 且未配置 tls_ca：数据面 TLS 将用系统根证书校验 relay 证书（与隧道一致）")
		}
	case TLSFalse:
		c.addWarning("[client] data_tls = false：数据面按明文运行；若 relay 要求数据面 TLS，拨号会明确失败而不是静默降级")
	}
	// duration 解析阶段已拒绝 <= 0 的值；这里兜住"绕过 ini 直接构造 Config"的情形，
	// 保证 client 侧 UDP 空闲清理不会退化成"立刻清理"或"永不清理"。
	if cl.UDPSessionTimeout < 0 {
		return fmt.Errorf("[client] udp_session_timeout = %s 必须大于 0", cl.UDPSessionTimeout)
	}
	if cl.ReconnectMaxBackoff < 0 {
		return fmt.Errorf("[client] reconnect_max_backoff = %s 必须大于 0", cl.ReconnectMaxBackoff)
	}
	return nil
}

// canonicalAddr 把地址规范化为可比较的 host:port 形式（端口保留，host 小写）。
func canonicalAddr(addr string) string {
	host, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return strings.ToLower(strings.TrimSpace(addr))
	}
	return strings.ToLower(net.JoinHostPort(host, port))
}

// checkListenAddr 校验监听地址：可被 SplitHostPort 解析、端口 1..65535。
// host 允许为空（表示所有网卡）。
func checkListenAddr(field, value string) error {
	host, port, err := net.SplitHostPort(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("%s = %q 不是合法的 host:port: %w", field, value, err)
	}
	_ = host
	if err := validatePort(port); err != nil {
		return fmt.Errorf("%s = %q: %w", field, value, err)
	}
	return nil
}

// checkDialAddr 校验拨号目标地址：必须带主机名且端口合法。
func checkDialAddr(field, value string) error {
	host, port, err := net.SplitHostPort(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("%s = %q 不是合法的 host:port: %w", field, value, err)
	}
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("%s = %q 缺少主机名", field, value)
	}
	if err := validatePort(port); err != nil {
		return fmt.Errorf("%s = %q: %w", field, value, err)
	}
	return nil
}

// isLoopbackListenAddr 报告监听地址是否只绑定回环接口（127.0.0.0/8、::1、localhost）。
//
// 用于两条安全校验（SEC-A1 / SEC-T2）：空 auth_token 与 tls=auto 无证书时，
// 只允许回环监听；0.0.0.0 / :: / 具体网卡 IP / 空 host 一律按"非回环"处理
// （宁可拒绝）。与 internal/admin.IsLoopbackListen 的判定保持一致。
func isLoopbackListenAddr(addr string) bool {
	s := strings.TrimSpace(addr)
	if s == "" {
		return false
	}
	host, _, err := net.SplitHostPort(s)
	if err != nil {
		// 容忍不带端口的写法（校验会另行拒绝，这里不能误判为可写）。
		host = s
	}
	return isLoopbackHost(host)
}

// isLoopbackHost 报告 host 是否是回环主机名或回环 IP。
func isLoopbackHost(host string) bool {
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

// warnFileMissing 在路径存在但文件缺失时给出警告（不报错，便于先写配置后拷证书）。
func warnFileMissing(c *Config, field, path string) {
	if _, err := os.Stat(path); err != nil {
		c.addWarning("%s 文件 %q 当前不可访问: %v", field, path, err)
	}
}
