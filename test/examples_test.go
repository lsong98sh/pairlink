package e2e

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lsong98sh/pairlink/internal/config"
)

// examplesDir 是仓库根目录下的示例配置目录（本测试位于 test/ 子目录）。
const examplesDir = "../examples"

// exampleMode 按文件名前缀推断示例配置对应的模式：
//
//	relay*.ini  -> relay（含 relay-no-udp.ini，故意不配 udp_listen）
//	agent*.ini  -> agent
//	client*.ini -> client（含 client-udp.ini）
//
// 以后新增示例时，文件名必须保持这个前缀约定，否则本测试会直接失败，
// 避免新模板没有被 Load/Validate 覆盖。
func exampleMode(name string) (config.Mode, bool) {
	switch {
	case strings.HasPrefix(name, "relay"):
		return config.ModeRelay, true
	case strings.HasPrefix(name, "agent"):
		return config.ModeAgent, true
	case strings.HasPrefix(name, "client"):
		return config.ModeClient, true
	default:
		return "", false
	}
}

// TestExamples 遍历 examples/ 与 examples/docker/ 下的 ini 模板，保证：
//   - 文件名前缀能映射到合法模式；
//   - config.Load + Validate(mode) 全部通过；
//   - 必填字段非空、token 是明显的占位符（含 CHANGE-ME）而不是可直接投产的密钥；
//   - 没有未知段/未知键（防止模板写错键名被静默忽略）；
//   - 没有行尾注释（ini 解析器不支持，会把注释当值）；
//   - 管理页监听地址符合安全默认：主机侧 127.0.0.1，容器侧 0.0.0.0；
//   - relay-no-udp.ini 确实没有 udp_listen，其余 relay 模板启用了原生 UDP；
//   - 所有模板使用同一个 token 占位符，保证模板之间可直接互通。
func TestExamples(t *testing.T) {
	groups := []struct {
		dir      string
		expected map[string]bool
	}{
		{
			dir: examplesDir,
			expected: map[string]bool{
				"relay.ini":        true,
				"relay-no-udp.ini": true,
				"agent.ini":        true,
				"client.ini":       true,
				"client-udp.ini":   true,
			},
		},
		{
			dir: filepath.Join(examplesDir, "docker"),
			expected: map[string]bool{
				"relay.ini":  true,
				"agent.ini":  true,
				"client.ini": true,
			},
		},
	}

	tokens := map[string]string{}
	for _, g := range groups {
		entries, err := os.ReadDir(g.dir)
		if err != nil {
			t.Fatalf("读取示例目录 %s: %v", g.dir, err)
		}
		seen := map[string]bool{}
		for _, ent := range entries {
			if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".ini") {
				continue
			}
			name := ent.Name()
			seen[name] = true
			if !g.expected[name] {
				t.Errorf("%s: 未预期的示例文件 %q；新增模板必须同步本测试与文件名前缀约定", g.dir, name)
				continue
			}
			path := filepath.Join(g.dir, name)
			mode, ok := exampleMode(name)
			if !ok {
				t.Errorf("%s: 文件名无法映射到模式（应以 relay/agent/client 开头）", path)
				continue
			}
			t.Run(filepath.ToSlash(path), func(t *testing.T) {
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("读取 %s: %v", path, err)
				}
				assertNoInlineComment(t, path, string(raw))

				cfg, err := config.Load(path)
				if err != nil {
					t.Fatalf("config.Load(%s): %v", path, err)
				}
				if err := cfg.Validate(mode); err != nil {
					t.Fatalf("Validate(%s): %v", mode, err)
				}
				for _, w := range cfg.Warnings {
					if strings.Contains(w, "未知配置段") || strings.Contains(w, "未知配置项") {
						t.Errorf("模板使用了未知配置段/键: %s", w)
					}
				}

				if cfg.Common.ManageListen == "" {
					t.Error("[common] manage_listen 不应为空：模板要能直接演示管理页")
				}
				host, _, err := net.SplitHostPort(cfg.Common.ManageListen)
				if err != nil {
					t.Errorf("[common] manage_listen = %q 无法解析: %v", cfg.Common.ManageListen, err)
				} else if g.dir == filepath.Join(examplesDir, "docker") {
					// 容器内必须绑 0.0.0.0，否则宿主机的端口映射访问不到。
					if host != "0.0.0.0" {
						t.Errorf("容器模板 manage_listen 应绑 0.0.0.0（宿主机再用端口映射限制到 127.0.0.1），实际 %q", cfg.Common.ManageListen)
					}
				} else if host != "127.0.0.1" {
					// 管理页无鉴权，主机侧默认只能绑 loopback。
					t.Errorf("主机模板 manage_listen 应绑 127.0.0.1（无鉴权管理页），实际 %q", cfg.Common.ManageListen)
				}

				token := ""
				switch mode {
				case config.ModeRelay:
					if cfg.Relay.TunnelListen == "" || cfg.Relay.TCPListen == "" {
						t.Error("[relay] tunnel_listen / tcp_listen 为必填项")
					}
					if name == "relay-no-udp.ini" {
						if cfg.Relay.UDPListen != "" {
							t.Errorf("relay-no-udp.ini 不应配置 udp_listen，实际 %q", cfg.Relay.UDPListen)
						}
					} else if cfg.Relay.UDPListen == "" {
						t.Errorf("%s 应配置 udp_listen（原生 UDP 示例）", name)
					}
					token = cfg.Relay.AuthToken
				case config.ModeAgent:
					if cfg.Server.RelayAddr == "" || cfg.Server.Token == "" {
						t.Error("[server] relay_addr / token 为必填项")
					}
					if len(cfg.Server.All()) == 0 {
						t.Error("[server] 至少需要一个服务（protocol+local 或 service.<ID>）")
					}
					token = cfg.Server.Token
				case config.ModeClient:
					if cfg.Client.RelayAddr == "" || cfg.Client.Token == "" {
						t.Error("[client] relay_addr / token 为必填项")
					}
					if len(cfg.Client.Services) == 0 {
						t.Error("[client] 模板应至少映射一个 service.<ID>，否则无法演示")
					}
					token = cfg.Client.Token
				}

				if strings.TrimSpace(token) == "" {
					// relay 的 auth_token 允许为空，但示例模板必须显式给出占位符，
					// 以便用户一眼看到"这里要改"。
					t.Errorf("%s: token 为空，模板必须放 CHANGE-ME 占位符", name)
				} else if !strings.Contains(token, "CHANGE-ME") {
					t.Errorf("%s: token %q 不是 CHANGE-ME 占位符，示例模板不应携带可投产的密钥", name, token)
				}
				// 记录出现过的 token 种类：所有模板共用一个占位符才能本地互通。
				if _, ok := tokens[token]; !ok {
					tokens[token] = name
				}
			})
		}
		for name := range g.expected {
			if !seen[name] {
				t.Errorf("%s: 缺少示例文件 %q", g.dir, name)
			}
		}
	}

	if len(tokens) > 1 {
		t.Errorf("示例模板出现了多个不同 token（%v）；三份模板应使用同一个 CHANGE-ME 占位符以便本地互通", tokens)
	}
}

// assertNoInlineComment 检查模板没有使用行尾注释。
//
// internal/config 的 ini 解析器只支持以 ';' 或 '#' 开头的整行注释；值里的 ';'/'#'
// 属于值本身。模板若写成 "key = value ; 说明"，说明会被当成值的一部分。
func assertNoInlineComment(t *testing.T, path, raw string) {
	t.Helper()
	for i, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, ";") || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "[") {
			continue
		}
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok {
			t.Errorf("%s:%d: 无法解析的行 %q", path, i+1, trimmed)
			continue
		}
		if strings.ContainsAny(value, ";#") {
			t.Errorf("%s:%d: [%s] 的值 %q 含 ';' 或 '#'；ini 不支持行尾注释，注释会被当成值",
				path, i+1, strings.TrimSpace(key), strings.TrimSpace(value))
		}
	}
}
