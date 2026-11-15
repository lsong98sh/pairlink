package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mustLoad 加载 testdata 下的配置，失败即终止用例。
func mustLoad(t *testing.T, name string) *Config {
	t.Helper()
	cfg, err := Load(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("Load(%s) 失败: %v", name, err)
	}
	return cfg
}

// writeTempINI 把内容写入临时 ini 文件并返回路径。
func writeTempINI(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.ini")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写临时配置失败: %v", err)
	}
	return path
}

// hasWarning 判断配置警告中是否包含给定子串。
func hasWarning(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

func TestLoadValidRelay(t *testing.T) {
	cfg := mustLoad(t, "valid_relay.ini")

	if got, want := cfg.Common.ManageListen, "127.0.0.1:9000"; got != want {
		t.Errorf("manage_listen = %q, want %q", got, want)
	}
	if !cfg.Common.Pprof {
		t.Error("pprof 应为 true")
	}
	if got, want := cfg.Common.LogLevel, "debug"; got != want {
		t.Errorf("log_level = %q, want %q", got, want)
	}
	if got, want := cfg.Common.LogFormat, "json"; got != want {
		t.Errorf("log_format = %q, want %q", got, want)
	}

	r := cfg.Relay
	if got, want := r.TunnelListen, "0.0.0.0:7000"; got != want {
		t.Errorf("tunnel_listen = %q, want %q", got, want)
	}
	if got, want := r.TCPListen, "0.0.0.0:7001"; got != want {
		t.Errorf("tcp_listen = %q, want %q", got, want)
	}
	if got, want := r.UDPListen, "0.0.0.0:7002"; got != want {
		t.Errorf("udp_listen = %q, want %q", got, want)
	}
	if got, want := r.PairTimeout, 12*time.Second; got != want {
		t.Errorf("pair_timeout = %v, want %v", got, want)
	}
	if got, want := r.IdleTimeout, 120*time.Second; got != want {
		t.Errorf("idle_timeout = %v, want %v", got, want)
	}
	if got, want := r.HeartbeatTimeout, 45*time.Second; got != want {
		t.Errorf("heartbeat_timeout = %v, want %v", got, want)
	}
	if got, want := r.UDPSessionTimeout, 30*time.Second; got != want {
		t.Errorf("udp_session_timeout = %v, want %v", got, want)
	}
	if got, want := r.TokenTTL, 20*time.Second; got != want {
		t.Errorf("token_ttl = %v, want %v", got, want)
	}
	if got, want := r.AuthToken, "s3cret-token"; got != want {
		t.Errorf("auth_token = %q, want %q", got, want)
	}

	dir := filepath.Dir(cfg.Path)
	if want := filepath.Join(dir, "certs", "cert.pem"); r.TLSCert != want {
		t.Errorf("tls_cert = %q, want %q（相对路径应相对 ini 目录解析为绝对路径）", r.TLSCert, want)
	}
	if want := filepath.Join(dir, "certs", "key.pem"); r.TLSKey != want {
		t.Errorf("tls_key = %q, want %q", r.TLSKey, want)
	}

	if got, want := len(cfg.Server.Services), 3; got != want {
		t.Fatalf("[server] service.<ID> 数量 = %d, want %d", got, want)
	}
	wantServices := []ServerService{
		{ID: "dsh", Proto: "tcp", Addr: "127.0.0.1:18080"},
		{ID: "rdp", Proto: "tcp", Addr: "10.0.0.5:3389"},
		{ID: "game", Proto: "udp", Addr: "127.0.0.1:19000"},
	}
	for i, want := range wantServices {
		if got := cfg.Server.Services[i]; got != want {
			t.Errorf("Services[%d] = %+v, want %+v", i, got, want)
		}
	}
	if hasWarning(cfg.Warnings, "未知配置") {
		t.Errorf("不应有未知配置警告: %v", cfg.Warnings)
	}

	if err := cfg.Validate(ModeRelay); err != nil {
		t.Fatalf("Validate(relay) 失败: %v", err)
	}
}

func TestLoadValidAgent(t *testing.T) {
	cfg := mustLoad(t, "valid_agent.ini")
	s := cfg.Server
	if got, want := s.RelayAddr, "relay.example.com:7000"; got != want {
		t.Errorf("relay_addr = %q, want %q", got, want)
	}
	if got, want := s.Protocol, "tcp"; got != want {
		t.Errorf("protocol = %q, want %q", got, want)
	}
	if got, want := s.Local, "127.0.0.1:8080"; got != want {
		t.Errorf("local = %q, want %q", got, want)
	}
	if got, want := s.Heartbeat, 15*time.Second; got != want {
		t.Errorf("heartbeat = %v, want %v", got, want)
	}
	if got, want := s.DialTimeout, 5*time.Second; got != want {
		t.Errorf("dial_timeout = %v, want %v", got, want)
	}
	if got, want := s.UDPSessionTimeout, 45*time.Second; got != want {
		t.Errorf("udp_session_timeout = %v, want %v", got, want)
	}
	if want := filepath.Join(filepath.Dir(cfg.Path), "ca.pem"); s.TLSCA != want {
		t.Errorf("tls_ca = %q, want %q", s.TLSCA, want)
	}

	all := s.All()
	if len(all) != 3 {
		t.Fatalf("All() 长度 = %d, want 3: %+v", len(all), all)
	}
	if all[0].ID != DefaultServiceID || all[0].Addr != "127.0.0.1:8080" || all[0].Proto != "tcp" {
		t.Errorf("All()[0] = %+v, want default 主服务", all[0])
	}
	if all[1].ID != "dsh" || all[2].ID != "web" || all[2].Proto != "udp" {
		t.Errorf("附加服务顺序或内容错误: %+v", all[1:])
	}
	if err := cfg.Validate(ModeAgent); err != nil {
		t.Fatalf("Validate(agent) 失败: %v", err)
	}
}

func TestLoadValidClient(t *testing.T) {
	cfg := mustLoad(t, "valid_client.ini")
	c := cfg.Client
	if got, want := c.RelayAddr, "relay.example.com:7000"; got != want {
		t.Errorf("relay_addr = %q, want %q", got, want)
	}
	if c.Subscribe {
		t.Error("subscribe 应为 false")
	}
	if got, want := c.Heartbeat, 20*time.Second; got != want {
		t.Errorf("heartbeat = %v, want %v", got, want)
	}
	if got, want := c.DialTimeout, 3*time.Second; got != want {
		t.Errorf("dial_timeout = %v, want %v", got, want)
	}
	if got, want := c.PairTimeout, 4*time.Second; got != want {
		t.Errorf("pair_timeout = %v, want %v", got, want)
	}
	if got, want := len(c.Services), 3; got != want {
		t.Fatalf("service 数量 = %d, want %d", got, want)
	}
	want := []ClientService{
		{ID: "http-svc", Listen: "127.0.0.1:8080"},
		{ID: "rdp-svc", Listen: "127.0.0.1:13389"},
		{ID: "udp-svc", Listen: "127.0.0.1:15000"},
	}
	for i := range want {
		if c.Services[i] != want[i] {
			t.Errorf("Services[%d] = %+v, want %+v", i, c.Services[i], want[i])
		}
	}
	if err := cfg.Validate(ModeClient); err != nil {
		t.Fatalf("Validate(client) 失败: %v", err)
	}
}

func TestLoadBadDuration(t *testing.T) {
	_, err := Load(filepath.Join("testdata", "bad_duration.ini"))
	if err == nil {
		t.Fatal("缺少 duration 单位的配置应报错")
	}
	if !strings.Contains(err.Error(), "duration") {
		t.Fatalf("错误信息应提示 duration: %v", err)
	}
}

func TestLoadServiceDefaultConflict(t *testing.T) {
	_, err := Load(filepath.Join("testdata", "service_default.ini"))
	if err == nil {
		t.Fatal("显式 service.default 应报错")
	}
	if !strings.Contains(err.Error(), "service.default") {
		t.Fatalf("错误信息应包含 service.default: %v", err)
	}
}

func TestValidateDuplicateClientListen(t *testing.T) {
	cfg := mustLoad(t, "dup_listen.ini")
	err := cfg.Validate(ModeClient)
	if err == nil {
		t.Fatal("重复的客户端监听地址应报错")
	}
	if !strings.Contains(err.Error(), "重复") {
		t.Fatalf("错误信息应提示重复: %v", err)
	}
}

func TestUnknownKeysOnlyWarn(t *testing.T) {
	cfg := mustLoad(t, "unknown_keys.ini")
	if !hasWarning(cfg.Warnings, "未知配置段 [totally-unknown]") {
		t.Errorf("缺少未知段警告: %v", cfg.Warnings)
	}
	if !hasWarning(cfg.Warnings, "unknown_common_key") {
		t.Errorf("缺少未知键警告: %v", cfg.Warnings)
	}
	if !hasWarning(cfg.Warnings, "[relay] unknown_relay_key") {
		t.Errorf("缺少 [relay] 未知键警告: %v", cfg.Warnings)
	}
	if err := cfg.Validate(ModeRelay); err != nil {
		t.Fatalf("未知键不应导致校验失败: %v", err)
	}
}

func TestRelayUDPListenOptional(t *testing.T) {
	// 未配置 udp_listen：合法，UDP 走 TCP 隧道。
	// 显式 tls = false + auth_token：非回环监听必须显式承担明文风险（SEC-T2）
	// 并提供接入密钥（SEC-A1），本用例只关注 udp_listen 的可选性。
	path := writeTempINI(t, `[relay]
tunnel_listen = 0.0.0.0:7000
tcp_listen = 0.0.0.0:7001
tls = false
auth_token = test-token
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if cfg.Relay.UDPListen != "" {
		t.Fatalf("UDPListen 应为空，实际 %q", cfg.Relay.UDPListen)
	}
	if err := cfg.Validate(ModeRelay); err != nil {
		t.Fatalf("未配置 udp_listen 应合法: %v", err)
	}
	if !hasWarning(cfg.Warnings, "udp_mode = tcp_tunnel") {
		t.Errorf("应提示 UDP 走 TCP 隧道: %v", cfg.Warnings)
	}

	// 配置 udp_listen：合法，原生 UDP。
	path = writeTempINI(t, `[relay]
tunnel_listen = 0.0.0.0:7000
tcp_listen = 0.0.0.0:7001
udp_listen = 0.0.0.0:7002
tls = false
auth_token = test-token
`)
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if err := cfg.Validate(ModeRelay); err != nil {
		t.Fatalf("配置 udp_listen 应合法: %v", err)
	}
	if hasWarning(cfg.Warnings, "udp_mode = tcp_tunnel") {
		t.Errorf("配置了 udp_listen 不应提示 TCP 隧道: %v", cfg.Warnings)
	}

	// 非法的 udp_listen 仍然报错。
	path = writeTempINI(t, `[relay]
tunnel_listen = 0.0.0.0:7000
tcp_listen = 0.0.0.0:7001
udp_listen = 0.0.0.0:0
tls = false
auth_token = test-token
`)
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if err := cfg.Validate(ModeRelay); err == nil {
		t.Fatal("非法 udp_listen 应校验失败")
	}
}

func TestDefaults(t *testing.T) {
	// 回环监听 + tls = auto 无证书 + 空 auth_token 仍是合法组合（仅本机调试），
	// 非回环监听则会被 SEC-A1 / SEC-T2 拒绝，见 TestValidateRelayNonLoopbackSecurity。
	path := writeTempINI(t, `[common]
[relay]
tunnel_listen = 127.0.0.1:7000
tcp_listen = 127.0.0.1:7001
udp_listen = 127.0.0.1:7002
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	checks := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"relay.pair_timeout", cfg.Relay.PairTimeout, 10 * time.Second},
		{"relay.idle_timeout", cfg.Relay.IdleTimeout, 300 * time.Second},
		{"relay.heartbeat_timeout", cfg.Relay.HeartbeatTimeout, 90 * time.Second},
		{"relay.udp_session_timeout", cfg.Relay.UDPSessionTimeout, 60 * time.Second},
		{"relay.token_ttl", cfg.Relay.TokenTTL, 15 * time.Second},
		{"server.heartbeat", cfg.Server.Heartbeat, 30 * time.Second},
		{"server.dial_timeout", cfg.Server.DialTimeout, 10 * time.Second},
		{"server.udp_session_timeout", cfg.Server.UDPSessionTimeout, 60 * time.Second},
		{"client.heartbeat", cfg.Client.Heartbeat, 30 * time.Second},
		{"client.dial_timeout", cfg.Client.DialTimeout, 10 * time.Second},
		{"client.pair_timeout", cfg.Client.PairTimeout, 10 * time.Second},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if cfg.Common.LogLevel != "info" || cfg.Common.LogFormat != "text" {
		t.Errorf("common 默认值错误: %+v", cfg.Common)
	}
	if !cfg.Client.Subscribe {
		t.Error("client.subscribe 默认应为 true")
	}
	if err := cfg.Validate(ModeRelay); err != nil {
		t.Fatalf("Validate(relay) 失败: %v", err)
	}
	if !hasWarning(cfg.Warnings, "tls_cert") {
		t.Errorf("未配置 TLS 应有警告: %v", cfg.Warnings)
	}
	if !hasWarning(cfg.Warnings, "auth_token 为空") {
		t.Errorf("空 auth_token 应有警告: %v", cfg.Warnings)
	}
	// SEC-D2 新增上限的默认值必须固定（0 = 不限制，默认值不能退化成 0）。
	if cfg.Relay.MaxConns != 1024 {
		t.Errorf("relay.max_conns 默认 = %d, want 1024", cfg.Relay.MaxConns)
	}
	if cfg.Relay.MaxConnsPerIP != 128 {
		t.Errorf("relay.max_conns_per_ip 默认 = %d, want 128", cfg.Relay.MaxConnsPerIP)
	}
	if cfg.Relay.MaxPendingPairs != 4096 {
		t.Errorf("relay.max_pending_pairs 默认 = %d, want 4096", cfg.Relay.MaxPendingPairs)
	}
}

func TestAddressNormalization(t *testing.T) {
	path := writeTempINI(t, `[server]
relay_addr = relay.example.com:7000
token = t
protocol = tcp
local = 127.0.0.1:8080
service.a = tcp:18080
service.b = udp::19000
service.c = tcp:[::1]:19001

[client]
relay_addr = relay.example.com:7000
token = t
service.x = 8080
service.y = :8081
service.z = 0.0.0.0:8082
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	wantServer := map[string]string{
		"a": "127.0.0.1:18080",
		"b": "127.0.0.1:19000",
		"c": "[::1]:19001",
	}
	for _, svc := range cfg.Server.Services {
		if got, ok := wantServer[svc.ID]; !ok || got != svc.Addr {
			t.Errorf("server service %s = %q, want %q", svc.ID, svc.Addr, wantServer[svc.ID])
		}
	}
	wantClient := map[string]string{
		"x": "127.0.0.1:8080",
		"y": "127.0.0.1:8081",
		"z": "0.0.0.0:8082",
	}
	for _, svc := range cfg.Client.Services {
		if got, ok := wantClient[svc.ID]; !ok || got != svc.Listen {
			t.Errorf("client service %s = %q, want %q", svc.ID, svc.Listen, wantClient[svc.ID])
		}
	}
	if err := cfg.Validate(ModeAgent); err != nil {
		t.Fatalf("Validate(agent) 失败: %v", err)
	}
	if err := cfg.Validate(ModeClient); err != nil {
		t.Fatalf("Validate(client) 失败: %v", err)
	}
}

func TestLoadSyntaxErrors(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name:    "缺少等号",
			content: "[relay]\ntunnel_listen 0.0.0.0:7000\n",
			wantErr: "缺少",
		},
		{
			name:    "键出现在段之前",
			content: "tunnel_listen = 0.0.0.0:7000\n[relay]\n",
			wantErr: "段之前",
		},
		{
			name:    "段头未闭合",
			content: "[relay\ntunnel_listen = 0.0.0.0:7000\n",
			wantErr: "']'",
		},
		{
			name:    "空段名",
			content: "[]\nfoo = bar\n",
			wantErr: "段名为空",
		},
		{
			name:    "非法服务ID",
			content: "[server]\nrelay_addr = r:7000\ntoken = t\nlocal = 127.0.0.1:1\nservice.Bad_ID = tcp:80\n",
			wantErr: "服务ID",
		},
		{
			name:    "非法协议",
			content: "[server]\nrelay_addr = r:7000\ntoken = t\nlocal = 127.0.0.1:1\nservice.a = icmp:80\n",
			wantErr: "协议",
		},
		{
			name:    "非法布尔",
			content: "[common]\npprof = maybe\n",
			wantErr: "布尔",
		},
		{
			name:    "duration 为零",
			content: "[relay]\ntunnel_listen = 0.0.0.0:7000\ntcp_listen = 0.0.0.0:7001\nudp_listen = 0.0.0.0:7002\npair_timeout = 0s\n",
			wantErr: "必须大于 0",
		},
		{
			name:    "客户端监听端口越界",
			content: "[client]\nrelay_addr = relay.example.com:7000\ntoken = t\nservice.a = 127.0.0.1:0\n",
			wantErr: "超出范围",
		},
		{
			name:    "客户端监听地址非法",
			content: "[client]\nrelay_addr = relay.example.com:7000\ntoken = t\nservice.a = 127.0.0.1\n",
			wantErr: "无法解析",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempINI(t, tc.content)
			_, err := Load(path)
			if err == nil {
				t.Fatalf("期望报错，实际成功")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误信息 %q 不包含 %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestValidateErrors(t *testing.T) {
	tests := []struct {
		name    string
		mode    Mode
		content string
		wantErr string
	}{
		{
			name: "relay 缺少 tcp_listen",
			mode: ModeRelay,
			content: `[relay]
tunnel_listen = 0.0.0.0:7000
udp_listen = 0.0.0.0:7002
`,
			wantErr: "tcp_listen 为必填项",
		},
		{
			name: "relay 监听地址重复",
			mode: ModeRelay,
			content: `[relay]
tunnel_listen = 0.0.0.0:7000
tcp_listen = 0.0.0.0:7000
udp_listen = 0.0.0.0:7002
`,
			wantErr: "重复",
		},
		{
			name: "relay 端口越界",
			mode: ModeRelay,
			content: `[relay]
tunnel_listen = 0.0.0.0:70000
tcp_listen = 0.0.0.0:7001
udp_listen = 0.0.0.0:7002
`,
			wantErr: "超出范围",
		},
		{
			name: "agent 缺少 token",
			mode: ModeAgent,
			content: `[server]
relay_addr = relay.example.com:7000
local = 127.0.0.1:8080
`,
			wantErr: "token 为必填项",
		},
		{
			name: "agent 缺少服务",
			mode: ModeAgent,
			content: `[server]
relay_addr = relay.example.com:7000
token = t
`,
			wantErr: "至少要配置一个服务",
		},
		{
			name: "agent 缺少 relay_addr",
			mode: ModeAgent,
			content: `[server]
token = t
local = 127.0.0.1:8080
`,
			wantErr: "relay_addr 为必填项",
		},
		{
			name: "agent 协议非法",
			mode: ModeAgent,
			content: `[server]
relay_addr = relay.example.com:7000
token = t
protocol = http
local = 127.0.0.1:8080
`,
			wantErr: "协议",
		},
		{
			name: "client 缺少 relay_addr",
			mode: ModeClient,
			content: `[client]
token = t
service.a = 127.0.0.1:8080
`,
			wantErr: "relay_addr 为必填项",
		},
		{
			name: "client 重复监听地址",
			mode: ModeClient,
			content: `[client]
relay_addr = relay.example.com:7000
token = t
service.a = 127.0.0.1:8080
service.b = 127.0.0.1:8080
`,
			wantErr: "重复",
		},
		{
			name: "common log_level 非法",
			mode: ModeRelay,
			content: `[common]
log_level = trace
[relay]
tunnel_listen = 0.0.0.0:7000
tcp_listen = 0.0.0.0:7001
udp_listen = 0.0.0.0:7002
`,
			wantErr: "log_level",
		},
		{
			name: "common log_format 非法",
			mode: ModeRelay,
			content: `[common]
log_format = yaml
[relay]
tunnel_listen = 0.0.0.0:7000
tcp_listen = 0.0.0.0:7001
udp_listen = 0.0.0.0:7002
`,
			wantErr: "log_format",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeTempINI(t, tc.content))
			if err != nil {
				t.Fatalf("Load 失败: %v", err)
			}
			err = cfg.Validate(tc.mode)
			if err == nil {
				t.Fatalf("期望校验失败")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误信息 %q 不包含 %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join("testdata", "no_such_file.ini")); err == nil {
		t.Fatal("文件不存在应报错")
	}
	if _, err := Load(""); err == nil {
		t.Fatal("空路径应报错")
	}
}

func TestParseMode(t *testing.T) {
	for _, s := range []string{"relay", "RELAY", " Agent ", "client"} {
		if _, err := ParseMode(s); err != nil {
			t.Errorf("ParseMode(%q) 失败: %v", s, err)
		}
	}
	if _, err := ParseMode("server"); err == nil {
		t.Error("ParseMode(server) 应报错")
	}
	if ModeRelay.String() != "relay" || !ModeClient.Valid() || Mode("x").Valid() {
		t.Error("Mode 基本方法错误")
	}
}
