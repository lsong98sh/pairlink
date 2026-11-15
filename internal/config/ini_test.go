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
	if v, ok := common.get("LOG_LEVEL"); !ok || v.Value != "debug" {
		t.Errorf("重复键应后者覆盖且键名大小写不敏感: %+v ok=%v", v, ok)
	}
	if len(common.order) != 2 {
		t.Errorf("重复键不应重复计入顺序: %v", common.order)
	}
	if common.Name != "common" {
		t.Errorf("段名应保留首次出现的大小写: %q", common.Name)
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
	// "[ Relay ]" 的段内空白被归一化，段名保留首次出现的大小写 "Relay"。
	if relay.Name != "Relay" {
		t.Errorf("段名空白未裁剪: %q", relay.Name)
	}
	if len(f.order) != 2 || f.order[0] != "common" || f.order[1] != "relay" {
		t.Errorf("段顺序错误: %v", f.order)
	}
}

func TestParseINIValues(t *testing.T) {
	longValue := strings.Repeat("x", 128*1024)
	content := "[values]\n" +
		"empty =\n" +
		"spaces =   padded value   \n" +
		"quoted = \" keep quotes \"\n" +
		"semicolon = value;still-value\n" +
		"hash = value#still-value\n" +
		"equals = a=b=c\n" +
		"colon = 127.0.0.1:9000\n" +
		"backslash = C:\\dir\\\n" +
		"long = " + longValue + "\n"
	f, err := parseINI([]byte(content))
	if err != nil {
		t.Fatalf("parseINI 失败: %v", err)
	}
	sec, ok := f.section("values")
	if !ok {
		t.Fatal("缺少 [values] 段")
	}
	cases := map[string]string{
		"empty":     "",
		"spaces":    "padded value",
		"quoted":    `" keep quotes "`,
		"semicolon": "value;still-value",
		"hash":      "value#still-value",
		"equals":    "a=b=c",
		"colon":     "127.0.0.1:9000",
		"backslash": `C:\dir\`,
		"long":      longValue,
	}
	for key, want := range cases {
		v, exists := sec.get(key)
		if !exists {
			t.Errorf("缺少键 %s", key)
			continue
		}
		if v.Value != want {
			t.Errorf("%s = %q, want %q", key, v.Value, want)
		}
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
		{"显式 DEFAULT 段", "[DEFAULT]\nkey = value\n", "DEFAULT"},
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
			if strings.Contains(err.Error(), "行") {
				t.Fatalf("错误信息不应再带行号: %q", err.Error())
			}
		})
	}
}

func TestParseINIEmpty(t *testing.T) {
	for name, data := range map[string][]byte{
		"nil":     nil,
		"空字节":     {},
		"仅有注释和空行": []byte("; c\n# d\n\n"),
		"仅 BOM":   []byte("\ufeff"),
	} {
		t.Run(name, func(t *testing.T) {
			f, err := parseINI(data)
			if err != nil {
				t.Fatalf("空文件不应报错: %v", err)
			}
			if len(f.sections) != 0 || len(f.order) != 0 {
				t.Errorf("空文件应没有段: %+v", f)
			}
		})
	}
}

func TestNormalizeSectionHeaders(t *testing.T) {
	in := "  [ Relay ]  \r\nkey = v\r\n[client] ; 注释\r\n[a]b]\n"
	got := string(normalizeSectionHeaders([]byte(in)))
	want := "  [Relay]  \r\nkey = v\r\n[client] ; 注释\r\n[a]b]\n"
	if got != want {
		t.Fatalf("段头归一化结果错误:\n got %q\nwant %q", got, want)
	}
	// BOM 之后的段头也要归一化，且 BOM 必须保留在行首。
	bomIn := "\ufeff[ Relay ]\nkey = v\n"
	if got := string(normalizeSectionHeaders([]byte(bomIn))); got != "\ufeff[Relay]\nkey = v\n" {
		t.Fatalf("BOM 段头归一化错误: %q", got)
	}
	// 没有段头的输入逐字节返回（避免无谓拷贝）。
	data := []byte("; x\nkey = v\n")
	if string(normalizeSectionHeaders(data)) != string(data) {
		t.Fatal("无段头时应原样返回")
	}
}
