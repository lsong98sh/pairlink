package main

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/version"
)

func TestSelectMode(t *testing.T) {
	tests := []struct {
		name                 string
		relay, agent, client bool
		want                 config.Mode
		wantErr              bool
	}{
		{"relay", true, false, false, config.ModeRelay, false},
		{"agent", false, true, false, config.ModeAgent, false},
		{"client", false, false, true, config.ModeClient, false},
		{"none", false, false, false, "", true},
		{"two", true, true, false, "", true},
		{"three", true, true, true, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectMode(tc.relay, tc.agent, tc.client)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际返回 %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("selectMode 失败: %v", err)
			}
			if got != tc.want {
				t.Fatalf("selectMode = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLoadConfig(t *testing.T) {
	if _, err := loadConfig("", config.ModeRelay); err == nil {
		t.Fatal("空配置路径应报错")
	}
	if _, err := loadConfig(filepath.Join("internal", "config", "testdata", "no_such.ini"), config.ModeRelay); err == nil {
		t.Fatal("不存在的配置应报错")
	}

	cfg, err := loadConfig(filepath.Join("internal", "config", "testdata", "valid_relay.ini"), config.ModeRelay)
	if err != nil {
		t.Fatalf("加载合法 relay 配置失败: %v", err)
	}
	if cfg.Relay.TunnelListen != "0.0.0.0:7000" {
		t.Fatalf("配置内容错误: %+v", cfg.Relay)
	}

	// client 模式校验失败：缺少 relay_addr/token。
	if _, err := loadConfig(filepath.Join("internal", "config", "testdata", "bad_duration.ini"), config.ModeRelay); err == nil {
		t.Fatal("非法 duration 应报错")
	} else if !strings.Contains(err.Error(), "duration") {
		t.Fatalf("错误信息应提示 duration: %v", err)
	}
}

func TestModeTitle(t *testing.T) {
	if got := modeTitle(config.ModeRelay); got != "Relay" {
		t.Errorf("modeTitle(relay) = %q", got)
	}
	if got := modeTitle(config.Mode("unknown")); got != "unknown" {
		t.Errorf("modeTitle(unknown) = %q", got)
	}
}

func TestVersionConstant(t *testing.T) {
	// 单一来源是 internal/version.Version；这里只断言别名一致、格式正确且用法说明引用了它，
	// 避免每次升版本都要改测试里的字面量。
	if Version != version.Version {
		t.Fatalf("main.Version = %q，internal/version.Version = %q", Version, version.Version)
	}
	if !regexp.MustCompile(`^v\d+\.\d+\.\d+$`).MatchString(Version) {
		t.Fatalf("Version = %q，不是 vX.Y.Z 形式", Version)
	}
	if !strings.Contains(usageText(), "-relay") {
		t.Fatal("用法说明缺少 -relay")
	}
	if !strings.Contains(usageText(), Version) {
		t.Fatalf("用法说明未引用版本号 %q", Version)
	}
}
