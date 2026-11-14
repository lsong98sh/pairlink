package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// writePatchINI 在临时目录写一个 ini 文件并返回路径。
func writePatchINI(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写 ini %s: %v", path, err)
	}
	return path
}

// readPatchINI 读取文件文本。
func readPatchINI(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 ini %s: %v", path, err)
	}
	return string(data)
}

// TestPatchValueReplaceKeepsFormat 覆盖「只替换值、保留对齐/注释/空行」。
func TestPatchValueReplaceKeepsFormat(t *testing.T) {
	original := "; PairLink relay 配置\n" +
		"\n" +
		"[relay]\n" +
		"# 隧道监听\n" +
		"tunnel_listen = 127.0.0.1:7000\n" +
		"pair_timeout      = 10s\n" +
		"idle_timeout=300s   \n" +
		"\n" +
		"; 下面的段跟 relay 无关\n" +
		"[server]\n" +
		"token = abc\n"
	path := writePatchINI(t, "relay.ini", original)

	if err := PatchValue(path, "relay", "pair_timeout", "30s"); err != nil {
		t.Fatalf("PatchValue: %v", err)
	}
	got := readPatchINI(t, path)
	want := strings.Replace(original, "pair_timeout      = 10s", "pair_timeout      = 30s", 1)
	if got != want {
		t.Fatalf("文件内容被意外改写:\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
	// 只改了一行，其它内容逐字节一致。
	if strings.Count(got, "pair_timeout") != 1 {
		t.Fatalf("pair_timeout 出现次数异常: %q", got)
	}
	if !strings.Contains(got, "idle_timeout=300s   \n") {
		t.Fatalf("未修改行的行尾空白应保留: %q", got)
	}
}

// TestPatchValueKeyCaseInsensitive 覆盖键名大小写不敏感且保留原写法。
func TestPatchValueKeyCaseInsensitive(t *testing.T) {
	path := writePatchINI(t, "relay.ini", "[relay]\nPAIR_TIMEOUT = 10s\n")
	if err := PatchValue(path, "relay", "pair_timeout", "45s"); err != nil {
		t.Fatalf("PatchValue: %v", err)
	}
	if got := readPatchINI(t, path); !strings.Contains(got, "PAIR_TIMEOUT = 45s") {
		t.Fatalf("应保留键名原写法，实际: %q", got)
	}
}

// TestPatchValueAppendKey 覆盖段内追加不存在的键。
func TestPatchValueAppendKey(t *testing.T) {
	original := "[common]\nlog_level = info\n\n[relay]\ntunnel_listen = 127.0.0.1:1\n\n; relay 段结束\n\n[server]\ntoken = abc\n"
	path := writePatchINI(t, "relay.ini", original)
	if err := PatchValue(path, "relay", "pair_timeout", "20s"); err != nil {
		t.Fatalf("PatchValue: %v", err)
	}
	got := readPatchINI(t, path)
	want := "[common]\nlog_level = info\n\n[relay]\ntunnel_listen = 127.0.0.1:1\n\n; relay 段结束\npair_timeout = 20s\n\n[server]\ntoken = abc\n"
	if got != want {
		t.Fatalf("追加位置错误:\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseINI(data); err != nil {
		t.Fatalf("追加后无法解析: %v", err)
	}
}

// TestPatchValueAppendSection 覆盖段不存在时在文件末尾追加。
func TestPatchValueAppendSection(t *testing.T) {
	original := "[common]\nlog_level = info\n"
	path := writePatchINI(t, "x.ini", original)
	if err := PatchValue(path, "client", "service.web", "127.0.0.1:8080"); err != nil {
		t.Fatalf("PatchValue: %v", err)
	}
	got := readPatchINI(t, path)
	want := "[common]\nlog_level = info\n\n[client]\nservice.web = 127.0.0.1:8080\n"
	if got != want {
		t.Fatalf("段追加结果错误:\n got %q\nwant %q", got, want)
	}
}

// TestPatchValueAppendSectionNoTrailingNewline 覆盖原文件无结尾换行时追加段。
func TestPatchValueAppendSectionNoTrailingNewline(t *testing.T) {
	path := writePatchINI(t, "x.ini", "[common]\nlog_level = info")
	if err := PatchValue(path, "relay", "token_ttl", "20s"); err != nil {
		t.Fatalf("PatchValue: %v", err)
	}
	got := readPatchINI(t, path)
	if !strings.Contains(got, "\n\n[relay]\ntoken_ttl = 20s") {
		t.Fatalf("追加段格式错误: %q", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Fatalf("新增内容后文件应以换行结尾: %q", got)
	}
}

// TestPatchValueDuplicateKeys 覆盖重复键全部替换。
func TestPatchValueDuplicateKeys(t *testing.T) {
	path := writePatchINI(t, "x.ini", "[relay]\npair_timeout = 1s\npair_timeout = 2s\n")
	if err := PatchValue(path, "relay", "pair_timeout", "3s"); err != nil {
		t.Fatalf("PatchValue: %v", err)
	}
	got := readPatchINI(t, path)
	if got != "[relay]\npair_timeout = 3s\npair_timeout = 3s\n" {
		t.Fatalf("重复键应全部替换: %q", got)
	}
}

// TestDeleteKey 覆盖删除存在的键、只删目标行。
func TestDeleteKey(t *testing.T) {
	original := "[client]\nrelay_addr = 127.0.0.1:7000\nservice.web = 127.0.0.1:8080\nservice.db = 127.0.0.1:9090\n\n[common]\n log_level = info\n"
	path := writePatchINI(t, "client.ini", original)
	if err := DeleteKey(path, "client", "service.web"); err != nil {
		t.Fatalf("DeleteKey: %v", err)
	}
	got := readPatchINI(t, path)
	want := "[client]\nrelay_addr = 127.0.0.1:7000\nservice.db = 127.0.0.1:9090\n\n[common]\n log_level = info\n"
	if got != want {
		t.Fatalf("删除结果错误:\n got %q\nwant %q", got, want)
	}
	if got := readPatchINI(t, path); strings.Contains(got, "service.web") {
		t.Fatalf("目标键仍存在: %q", got)
	}
}

// TestDeleteKeyMissing 覆盖删除不存在的键/段视为成功。
func TestDeleteKeyMissing(t *testing.T) {
	original := "[client]\nrelay_addr = 127.0.0.1:7000\n"
	path := writePatchINI(t, "client.ini", original)
	if err := DeleteKey(path, "client", "service.nope"); err != nil {
		t.Fatalf("删除不存在的键应成功: %v", err)
	}
	if err := DeleteKey(path, "nope", "x"); err != nil {
		t.Fatalf("删除不存在的段应成功: %v", err)
	}
	if got := readPatchINI(t, path); got != original {
		t.Fatalf("文件不应改变: %q", got)
	}
}

// TestPatchCRLFRoundTrip 覆盖 CRLF 换行风格与结尾换行保留。
func TestPatchCRLFRoundTrip(t *testing.T) {
	original := "; 注释\r\n[relay]\r\npair_timeout = 10s\r\n\r\n[server]\r\ntoken = abc\r\n"
	path := writePatchINI(t, "crlf.ini", original)
	if err := PatchValue(path, "relay", "pair_timeout", "30s"); err != nil {
		t.Fatalf("PatchValue: %v", err)
	}
	got := readPatchINI(t, path)
	want := strings.Replace(original, "pair_timeout = 10s", "pair_timeout = 30s", 1)
	if got != want {
		t.Fatalf("CRLF 文件被改写:\n got %q\nwant %q", got, want)
	}
	if strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") {
		t.Fatalf("CRLF 文件中出现了裸 LF: %q", got)
	}
}

// TestPatchNoTrailingNewline 覆盖无结尾换行的文件。
func TestPatchNoTrailingNewline(t *testing.T) {
	path := writePatchINI(t, "x.ini", "[relay]\npair_timeout = 10s")
	if err := PatchValue(path, "relay", "pair_timeout", "12s"); err != nil {
		t.Fatalf("PatchValue: %v", err)
	}
	got := readPatchINI(t, path)
	if got != "[relay]\npair_timeout = 12s" {
		t.Fatalf("结尾换行状态未保持: %q", got)
	}
}

// TestPatchPreservesCommentsAndBlankLines 覆盖注释/空行/未知键原样保留。
func TestPatchPreservesCommentsAndBlankLines(t *testing.T) {
	original := "# top\n\n[relay]\n; pair_timeout 说明\nunknown_key = keep-me\n\npair_timeout = 10s\n\n# tail\n"
	path := writePatchINI(t, "x.ini", original)
	if err := PatchValue(path, "relay", "pair_timeout", "11s"); err != nil {
		t.Fatalf("PatchValue: %v", err)
	}
	want := strings.Replace(original, "pair_timeout = 10s", "pair_timeout = 11s", 1)
	if got := readPatchINI(t, path); got != want {
		t.Fatalf("注释/空行/未知键应原样保留:\n got %q\nwant %q", got, want)
	}
}

// TestPatchPreservesFileMode 覆盖权限位保留（Windows 只校验可读）。
func TestPatchPreservesFileMode(t *testing.T) {
	path := writePatchINI(t, "x.ini", "[relay]\npair_timeout = 10s\n")
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := PatchValue(path, "relay", "pair_timeout", "20s"); err != nil {
		t.Fatalf("PatchValue: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("权限位未保留: %v", info.Mode().Perm())
	}
}

// TestPatchReadOnlyDir 覆盖只读目录写入失败且原文件不被破坏。
//
// Windows 的目录只读属性不阻止在其中创建文件，因此该用例只在类 Unix 上执行；
// WSL2 的 `go test -race ./internal/...` 会覆盖到它。
func TestPatchReadOnlyDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 目录只读属性不阻止创建临时文件，改用 WSL2 覆盖")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "x.ini")
	original := "[relay]\npair_timeout = 10s\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if err := PatchValue(path, "relay", "pair_timeout", "20s"); err == nil {
		t.Fatal("只读目录下 PatchValue 应报错")
	}
	if got := readPatchINI(t, path); got != original {
		t.Fatalf("失败时原文件被破坏: %q", got)
	}
}

// TestPatchValueConcurrent 覆盖 10 个 goroutine 交替 Patch 后文件仍可解析。
func TestPatchValueConcurrent(t *testing.T) {
	path := writePatchINI(t, "x.ini", "; 头部\n[relay]\npair_timeout = 1s\nidle_timeout = 2s\n\n[server]\ntoken = x\n")
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				value := fmt.Sprintf("%ds", i*100+j+1)
				if err := PatchValue(path, "relay", "pair_timeout", value); err != nil {
					t.Errorf("并发 PatchValue: %v", err)
					return
				}
				if err := PatchValue(path, "relay", "idle_timeout", value); err != nil {
					t.Errorf("并发 PatchValue: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseINI(data)
	if err != nil {
		t.Fatalf("并发写后文件无法解析: %v\n%s", err, data)
	}
	relay, ok := parsed.section("relay")
	if !ok {
		t.Fatal("并发写后 [relay] 段丢失")
	}
	for _, key := range []string{"pair_timeout", "idle_timeout"} {
		v, exists := relay.get(key)
		if !exists {
			t.Fatalf("并发写后键 %s 丢失", key)
		}
		if !strings.HasSuffix(v.Value, "s") {
			t.Fatalf("键 %s 的值非法: %q", key, v.Value)
		}
	}
	if !strings.Contains(string(data), "; 头部") || !strings.Contains(string(data), "[server]") {
		t.Fatalf("并发写破坏了注释或其它段:\n%s", data)
	}
}

// TestPatchRejectsInjection 覆盖拒绝换行/非法段名键名。
func TestPatchRejectsInjection(t *testing.T) {
	original := "[relay]\npair_timeout = 10s\n"
	path := writePatchINI(t, "x.ini", original)

	cases := []struct {
		name    string
		section string
		key     string
		value   string
		// deleteAlso 表示 DeleteKey 使用同样的 section/key 也应被拒绝；
		// 仅 value 非法时 DeleteKey 不涉及 value，不应断言失败。
		deleteAlso bool
	}{
		{"换行注入值", "relay", "pair_timeout", "10s\n[evil]", false},
		{"换行注入键", "relay", "x\ny", "1", true},
		{"方括号段名", "re[lay", "x", "1", true},
		{"等号键名", "relay", "a=b", "1", true},
		{"空段名", " ", "x", "1", true},
		{"空键名", "relay", " ", "1", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := PatchValue(path, tc.section, tc.key, tc.value); err == nil {
				t.Fatal("应拒绝非法参数")
			}
			if tc.deleteAlso {
				if err := DeleteKey(path, tc.section, tc.key); err == nil {
					t.Fatal("DeleteKey 应拒绝非法参数")
				}
			}
			if got := readPatchINI(t, path); got != original {
				t.Fatalf("非法参数不应改动文件: %q", got)
			}
		})
	}
}

// TestPatchMissingFile 覆盖文件不存在时报错。
func TestPatchMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.ini")
	if err := PatchValue(path, "relay", "pair_timeout", "1s"); err == nil {
		t.Fatal("文件不存在时应报错")
	}
	if err := DeleteKey(path, "relay", "pair_timeout"); err == nil {
		t.Fatal("文件不存在时应报错")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("失败时不应创建文件: %v", err)
	}
}

// TestPatchValueDuplicateSections 覆盖同名段重复出现时两个段都更新。
func TestPatchValueDuplicateSections(t *testing.T) {
	original := "[relay]\npair_timeout = 1s\n\n[server]\ntoken = x\n\n[relay]\npair_timeout = 2s\n"
	path := writePatchINI(t, "x.ini", original)
	if err := PatchValue(path, "relay", "pair_timeout", "3s"); err != nil {
		t.Fatalf("PatchValue: %v", err)
	}
	want := "[relay]\npair_timeout = 3s\n\n[server]\ntoken = x\n\n[relay]\npair_timeout = 3s\n"
	if got := readPatchINI(t, path); got != want {
		t.Fatalf("重复段应全部替换:\n got %q\nwant %q", got, want)
	}
	if err := DeleteKey(path, "relay", "pair_timeout"); err != nil {
		t.Fatalf("DeleteKey: %v", err)
	}
	want = "[relay]\n\n[server]\ntoken = x\n\n[relay]\n"
	if got := readPatchINI(t, path); got != want {
		t.Fatalf("重复段删除结果错误:\n got %q\nwant %q", got, want)
	}
}

// TestPatchValueOnlyValueChanges 覆盖值中包含 '=' / ';' / '#' 时的替换。
func TestPatchValueOnlyValueChanges(t *testing.T) {
	path := writePatchINI(t, "x.ini", "[relay]\ntoken = old=value;keep#x\n")
	if err := PatchValue(path, "relay", "token", "new=value;keep#y"); err != nil {
		t.Fatalf("PatchValue: %v", err)
	}
	if got := readPatchINI(t, path); got != "[relay]\ntoken = new=value;keep#y\n" {
		t.Fatalf("值替换错误: %q", got)
	}
}
