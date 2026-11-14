package config

import (
	"strings"
	"testing"
)

func TestParseINI(t *testing.T) {
	content := "\ufeff; 顶部注释\r\n" +
		"# 另一种注释\r\n" +
		"\r\n" +
		"[common]\r\n" +
		"  manage_listen   =   127.0.0.1:9000   \r\n" +
		"log_level = info ; 不是行尾注释，属于值\r\n" +
		"log_level = debug\r\n" +
		"\r\n" +
		"[ Relay ]\r\n" +
		"tunnel_listen = 0.0.0.0:7000\r\n" +
		"token = a#b;c\r\n" +
		"[relay]\r\n" +
		"tcp_listen = 0.0.0.0:7001\r\n"

	f, err := parseINI([]byte(content))
	if err != nil {
		t.Fatalf("parseINI 失败: %v", err)
	}

	common, ok := f.section("COMMON")
	if !ok {
		t.Fatal("缺少 [common] 段（段名应大小写不敏感）")
	}
	if v, ok := common.get("manage_listen"); !ok || v.Value != "127.0.0.1:9000" {
		t.Errorf("manage_listen 解析错误: %+v ok=%v", v, ok)
	}
	if v, ok := common.get("log_level"); !ok || v.Value != "debug" {
		t.Errorf("重复键应后者覆盖: %+v ok=%v", v, ok)
	}
	if len(common.order) != 2 {
		t.Errorf("重复键不应重复计入顺序: %v", common.order)
	}

	relay, ok := f.section("relay")
	if !ok {
		t.Fatal("缺少合并后的 [relay] 段")
	}
	if v, _ := relay.get("tunnel_listen"); v.Value != "0.0.0.0:7000" {
		t.Errorf("tunnel_listen = %q", v.Value)
	}
	if v, _ := relay.get("tcp_listen"); v.Value != "0.0.0.0:7001" {
		t.Errorf("重复段名应合并: tcp_listen = %q", v.Value)
	}
	if v, _ := relay.get("token"); v.Value != "a#b;c" {
		t.Errorf("值中的 # 与 ; 应保留: %q", v.Value)
	}
	if len(f.order) != 2 || f.order[0] != "common" || f.order[1] != "relay" {
		t.Errorf("段顺序错误: %v", f.order)
	}
}

func TestParseINIErrors(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{"缺少等号", "[a]\nkey\n", "无法解析"},
		{"空键", "[a]\n = value\n", "键名为空"},
		{"段前有键", "key = value\n[a]\n", "段之前"},
		{"段头未闭合", "[a\nkey = value\n", "']'"},
		{"空段名", "[]\nkey = value\n", "段名为空"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseINI([]byte(tc.content))
			if err == nil {
				t.Fatal("期望报错")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误信息 %q 不包含 %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestParseINIEmpty(t *testing.T) {
	f, err := parseINI(nil)
	if err != nil {
		t.Fatalf("空文件不应报错: %v", err)
	}
	if len(f.sections) != 0 || len(f.order) != 0 {
		t.Errorf("空文件应没有段: %+v", f)
	}
}
