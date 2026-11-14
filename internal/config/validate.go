package config

import (
	"fmt"
	"net"
	"os"
	"strings"
)

// knownKeys 列出各段已知键，用于未知键警告。
var knownKeys = map[string]map[string]bool{
	"common": {
		"manage_listen": true,
		"pprof":         true,
		"log_level":     true,
		"log_format":    true,
	},
	"relay": {
		"tunnel_listen":       true,
		"tcp_listen":          true,
		"udp_listen":          true,
		"tls_cert":            true,
		"tls_key":             true,
		"auth_token":          true,
		"pair_timeout":        true,
		"idle_timeout":        true,
		"heartbeat_timeout":   true,
		"udp_session_timeout": true,
		"token_ttl":           true,
	},
	"server": {
		"relay_addr":               true,
		"token":                    true,
		"protocol":                 true,
		"local":                    true,
		"tls_ca":                   true,
		"tls_insecure_skip_verify": true,
		"heartbeat":                true,
		"dial_timeout":             true,
		"udp_session_timeout":      true,
		"reconnect":                true,
		"reconnect_max_backoff":    true,
	},
	"client": {
		"relay_addr":               true,
		"token":                    true,
		"tls_ca":                   true,
		"tls_insecure_skip_verify": true,
		"heartbeat":                true,
		"subscribe":                true,
		"dial_timeout":             true,
		"pair_timeout":             true,
		"udp_session_timeout":      true,
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
			b.warnf("未知配置段 [%s]（第 %d 行），已忽略", sec.Name, sec.Line)
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
			b.warnf("未知配置项 [%s] %s（第 %d 行），已忽略", sec.Name, v.Key, v.Line)
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

	hasCert := strings.TrimSpace(r.TLSCert) != ""
	hasKey := strings.TrimSpace(r.TLSKey) != ""
	switch {
	case !hasCert && !hasKey:
		c.addWarning("[relay] 未配置 tls_cert / tls_key，隧道将以明文运行（不建议在公网使用）")
	case hasCert != hasKey:
		c.addWarning("[relay] tls_cert 与 tls_key 必须成对配置，当前按明文隧道运行")
	}
	if hasCert {
		warnFileMissing(c, "tls_cert", r.TLSCert)
	}
	if hasKey {
		warnFileMissing(c, "tls_key", r.TLSKey)
	}
	if strings.TrimSpace(r.AuthToken) == "" {
		c.addWarning("[relay] auth_token 为空，任何 agent/client 都可接入隧道（仅建议内网调试使用）")
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
	if s.TLSCA != "" {
		warnFileMissing(c, "tls_ca", s.TLSCA)
	}
	if s.TLSInsecureSkipVerify {
		c.addWarning("[server] tls_insecure_skip_verify = true，将跳过 relay 证书校验（仅建议测试环境）")
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
	if cl.TLSCA != "" {
		warnFileMissing(c, "tls_ca", cl.TLSCA)
	}
	if cl.TLSInsecureSkipVerify {
		c.addWarning("[client] tls_insecure_skip_verify = true，将跳过 relay 证书校验（仅建议测试环境）")
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

// warnFileMissing 在路径存在但文件缺失时给出警告（不报错，便于先写配置后拷证书）。
func warnFileMissing(c *Config, field, path string) {
	if _, err := os.Stat(path); err != nil {
		c.addWarning("%s 文件 %q 当前不可访问: %v", field, path, err)
	}
}
