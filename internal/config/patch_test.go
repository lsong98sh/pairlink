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

// parsePatched 重新解析写回后的文件。
func parsePatched(t *testing.T, path string) *iniFile {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 ini %s: %v", path, err)
	}
	f, err := parseINI(data)
	if err != nil {
		t.Fatalf("写回后无法解析: %v\n%s", err, data)
	}
	return f
}

// mustValue 断言段内键值。
func mustValue(t *testing.T, f *iniFile, section, key, want string) {
	t.Helper()
	sec, ok := f.section(section)
	if !ok {
		t.Fatalf("缺少 [%s] 段", section)
	}
	v, ok := sec.get(key)
	if !ok {
		t.Fatalf("[%s] 缺少键 %s", section, key)
	}
	if v.Value != want {
		t.Fatalf("[%s] %s = %q, want %q", section, key, v.Value, want)
	}
}

// TestPatchValueUpdatesExistingKey 覆盖改值后其它键/段/注释仍在。
func TestPatchValueUpdatesExistingKey(t *testing.T) {
	original := "; 头部注释\n\n[relay]\n# 隧道监听\ntunnel_listen = 127.0.0.1:7000\npair_timeout = 10s\nidle_timeout = 300s\n\n[server]\ntoken = abc\n"
	path := writePatchINI(t, "relay.ini", original)

	if err := PatchValue(path, "relay", "pair_timeout", "30s"); err != nil {
		t.Fatalf("PatchValue: %v", err)
	}
	f := parsePatched(t, path)
	mustValue(t, f, "relay", "pair_timeout", "30s")
	mustValue(t, f, "relay", "tunnel_listen", "127.0.0.1:7000")
	mustValue(t, f, "relay", "idle_timeout", "300s")
	mustValue(t, f, "server", "token", "abc")

	got := readPatchINI(t, path)
	if strings.Count(got, "pair_timeout") != 1 {
		t.Fatalf("pair_timeout 出现次数异常: %q", got)
	}
	// ini.v1 会把注释挂在段/键上，写回后仍应保留（位置可能变化）。
	if !strings.Contains(got, "; 头部注释") || !strings.Contains(got, "# 隧道监听") {
		t.Fatalf("注释被意外丢弃:\n%s", got)
	}
}

// TestPatchValueKeyCaseInsensitive 覆盖键名大小写不敏感且保留原写法。
func TestPatchValueKeyCaseInsensitive(t *testing.T) {
	path := writePatchINI(t, "relay.ini", "[relay]\nPAIR_TIMEOUT = 10s\n")
	if err := PatchValue(path, "relay", "pair_timeout", "45s"); err != nil {
		t.Fatalf("PatchValue: %v", err)
	}
	mustValue(t, parsePatched(t, path), "relay", "pair_timeout", "45s")
	if got := readPatchINI(t, path); !strings.Contains(got, "PAIR_TIMEOUT = 45s") {
		t.Fatalf("应保留键名原写法，实际: %q", got)
	}
}

// TestPatchValueSectionCaseAndSpacing 覆盖段名大小写/方括号内空白不产生重复段。
func TestPatchValueSectionCaseAndSpacing(t *testing.T) {
	path := writePatchINI(t, "relay.ini", "[ Relay ]\ntunnel_listen = 127.0.0.1:7000\npair_timeout = 10s\n")
	if err := PatchValue(path, "relay", "pair_timeout", "20s"); err != nil {
		t.Fatalf("PatchValue: %v", err)
	}
	f := parsePatched(t, path)
	mustValue(t, f, "relay", "pair_timeout", "20s")
	if len(f.sections) != 1 {
		t.Fatalf("写回后出现重复段: %+v", f.order)
	}
	if got := readPatchINI(t, path); strings.Count(got, "[") != 1 {
		t.Fatalf("段头重复: %q", got)
	}
}

// TestPatchValueAppendKey 覆盖段内追加不存在的键。
func TestPatchValueAppendKey(t *testing.T) {
	original := "[common]\nlog_level = info\n\n[relay]\ntunnel_listen = 127.0.0.1:1\n\n; relay 段结束\n\n[server]\ntoken = abc\n"
	path := writePatchINI(t, "relay.ini", original)
	if err := PatchValue(path, "relay", "pair_timeout", "20s"); err != nil {
		t.Fatalf("PatchValue: %v", err)
	}
	f := parsePatched(t, path)
	mustValue(t, f, "relay", "pair_timeout", "20s")
	mustValue(t, f, "relay", "tunnel_listen", "127.0.0.1:1")
	mustValue(t, f, "server", "token", "abc")
}

// TestPatchValueAppendSection 覆盖段不存在时在文件末尾追加。
func TestPatchValueAppendSection(t *testing.T) {
	path := writePatchINI(t, "x.ini", "[common]\nlog_level = info\n")
	if err := PatchValue(path, "client", "service.web", "127.0.0.1:8080"); err != nil {
		t.Fatalf("PatchValue: %v", err)
	}
	f := parsePatched(t, path)
	mustValue(t, f, "client", "service.web", "127.0.0.1:8080")
	mustValue(t, f, "common", "log_level", "info")
	if got := readPatchINI(t, path); !strings.HasSuffix(got, "\n") {
		t.Fatalf("写回后文件应以换行结尾: %q", got)
	}
}

// TestPatchValueAppendSectionNoTrailingNewline 覆盖原文件无结尾换行时追加段。
func TestPatchValueAppendSectionNoTrailingNewline(t *testing.T) {
	path := writePatchINI(t, "x.ini", "[common]\nlog_level = info")
	if err := PatchValue(path, "relay", "token_ttl", "20s"); err != nil {
		t.Fatalf("PatchValue: %v", err)
	}
	mustValue(t, parsePatched(t, path), "relay", "token_ttl", "20s")
	if got := readPatchINI(t, path); !strings.HasSuffix(got, "\n") {
		t.Fatalf("新增内容后文件应以换行结尾: %q", got)
	}
}

// TestPatchValueDuplicateKeys 覆盖重复键：解析器语义为"后者覆盖"，写回后归一为一行。
func TestPatchValueDuplicateKeys(t *testing.T) {
	path := writePatchINI(t, "x.ini", "[relay]\npair_timeout = 1s\npair_timeout = 2s\n")
	if err := PatchValue(path, "relay", "pair_timeout", "3s"); err != nil {
		t.Fatalf("PatchValue: %v", err)
	}
	mustValue(t, parsePatched(t, path), "relay", "pair_timeout", "3s")
	got := readPatchINI(t, path)
	if strings.Count(got, "pair_timeout") != 1 {
		t.Fatalf("重复键应归一为一行: %q", got)
	}
}

// TestDeleteKey 覆盖删除存在的键、只删目标键。
func TestDeleteKey(t *testing.T) {
	original := "[client]\nrelay_addr = 127.0.0.1:7000\nservice.web = 127.0.0.1:8080\nservice.db = 127.0.0.1:9090\n\n[common]\nlog_level = info\n"
	path := writePatchINI(t, "client.ini", original)
	if err := DeleteKey(path, "client", "service.web"); err != nil {
		t.Fatalf("DeleteKey: %v", err)
	}
	f := parsePatched(t, path)
	sec, ok := f.section("client")
	if !ok {
		t.Fatal("[client] 段丢失")
	}
	if _, exists := sec.get("service.web"); exists {
		t.Fatalf("目标键仍存在: %q", readPatchINI(t, path))
	}
	mustValue(t, f, "client", "service.db", "127.0.0.1:9090")
	mustValue(t, f, "client", "relay_addr", "127.0.0.1:7000")
	mustValue(t, f, "common", "log_level", "info")
}

// TestDeleteKeyMissing 覆盖删除不存在的键/段视为成功且文件逐字节不变。
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

// TestDeleteKeyLastKeyKeepsSection 覆盖删掉段内最后一个键后段头仍在、文件仍可解析。
func TestDeleteKeyLastKeyKeepsSection(t *testing.T) {
	path := writePatchINI(t, "x.ini", "[client]\nservice.a = 127.0.0.1:8080\n\n[common]\nlog_level = info\n")
	if err := DeleteKey(path, "client", "service.a"); err != nil {
		t.Fatalf("DeleteKey: %v", err)
	}
	f := parsePatched(t, path)
	sec, ok := f.section("client")
	if !ok {
		t.Fatal("空段头不应被删除")
	}
	if len(sec.order) != 0 {
		t.Fatalf("段内键应已删除: %v", sec.order)
	}
	mustValue(t, f, "common", "log_level", "info")
}

// TestPatchCRLFReload 覆盖 CRLF 文件写回后可重新解析（不要求保持 CRLF）。
func TestPatchCRLFReload(t *testing.T) {
	original := "; 注释\r\n[relay]\r\npair_timeout = 10s\r\n\r\n[server]\r\ntoken = abc\r\n"
	path := writePatchINI(t, "crlf.ini", original)
	if err := PatchValue(path, "relay", "pair_timeout", "30s"); err != nil {
		t.Fatalf("PatchValue: %v", err)
	}
	mustValue(t, parsePatched(t, path), "relay", "pair_timeout", "30s")
	got := readPatchINI(t, path)
	if strings.Contains(got, "\r\n") {
		t.Fatalf("换行应统一为 LF（v0.5.0 起不再承诺保留 CRLF）: %q", got)
	}
}

// TestPatchPreservesBOM 覆盖 BOM 保留且写回后可解析。
func TestPatchPreservesBOM(t *testing.T) {
	path := writePatchINI(t, "bom.ini", "\ufeff[relay]\npair_timeout = 10s\n")
	if err := PatchValue(path, "relay", "pair_timeout", "11s"); err != nil {
		t.Fatalf("PatchValue: %v", err)
	}
	got := readPatchINI(t, path)
	if !strings.HasPrefix(got, "\ufeff") {
		t.Fatalf("BOM 丢失: %q", got)
	}
	mustValue(t, parsePatched(t, path), "relay", "pair_timeout", "11s")
}

// TestPatchValueSpecialCharacters 覆盖值里的 '=' / ';' / '#' / 反引号 / 引号往返一致。
func TestPatchValueSpecialCharacters(t *testing.T) {
	path := writePatchINI(t, "x.ini", "[relay]\ntoken = old\n")
	for _, value := range []string{
		"new=value;keep#y",
		"has`backtick",
		`"quoted"`,
		`C:\dir\`,
		strings.Repeat("v", 16*1024),
	} {
		if err := PatchValue(path, "relay", "token", value); err != nil {
			t.Fatalf("PatchValue(%q): %v", value, err)
		}
		mustValue(t, parsePatched(t, path), "relay", "token", value)
	}
}

// TestPatchValueTrimsWhitespace 覆盖值两侧空白与读取语义一致地被裁剪。
func TestPatchValueTrimsWhitespace(t *testing.T) {
	path := writePatchINI(t, "x.ini", "[relay]\npair_timeout = 10s\n")
	if err := PatchValue(path, "relay", "pair_timeout", "  30s  "); err != nil {
		t.Fatalf("PatchValue: %v", err)
	}
	mustValue(t, parsePatched(t, path), "relay", "pair_timeout", "30s")
	if got := readPatchINI(t, path); !strings.Contains(got, "pair_timeout = 30s\n") {
		t.Fatalf("带首尾空白的值不应被写成引号形式: %q", got)
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

// TestPatchValueConcurrent 覆盖 10 个 goroutine 交替 Patch 后文件仍完整可解析。
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

	f := parsePatched(t, path)
	mustValue(t, f, "server", "token", "x")
	sec, ok := f.section("relay")
	if !ok {
		t.Fatal("并发写后 [relay] 段丢失")
	}
	for _, key := range []string{"pair_timeout", "idle_timeout"} {
		v, exists := sec.get(key)
		if !exists {
			t.Fatalf("并发写后键 %s 丢失", key)
		}
		if !strings.HasSuffix(v.Value, "s") {
			t.Fatalf("键 %s 的值非法: %q", key, v.Value)
		}
	}
	data := readPatchINI(t, path)
	if strings.Count(data, "[relay]") != 1 {
		t.Fatalf("并发写后段头重复:\n%s", data)
	}
	if !strings.Contains(data, "; 头部") {
		t.Fatalf("并发写破坏了注释:\n%s", data)
	}
	// 临时文件必须全部清理。
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".pairlink-ini-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("残留临时文件: %v", matches)
	}
}

// TestPatchRejectsInjection 覆盖拒绝换行/非法段名键名与保留段名。
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
		{"回车注入值", "relay", "pair_timeout", "10s\r[evil]", false},
		{"换行注入键", "relay", "x\ny", "1", true},
		{"方括号段名", "re[lay", "x", "1", true},
		{"等号键名", "relay", "a=b", "1", true},
		{"空段名", " ", "x", "1", true},
		{"空键名", "relay", " ", "1", true},
		{"保留段名", " DEFAULT ", "x", "1", true},
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

// TestPatchMissingFile 覆盖文件不存在时报错且不创建文件。
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

// TestPatchRejectsKeyBeforeSection 覆盖段前键值的文件不参与写回（Load 也会拒绝）。
func TestPatchRejectsKeyBeforeSection(t *testing.T) {
	original := "pair_timeout = 10s\n[relay]\ntunnel_listen = 127.0.0.1:7000\n"
	path := writePatchINI(t, "x.ini", original)
	if err := PatchValue(path, "relay", "pair_timeout", "20s"); err == nil {
		t.Fatal("段前键值应拒绝写回")
	}
	if got := readPatchINI(t, path); got != original {
		t.Fatalf("拒绝时文件不应改变: %q", got)
	}
}

// TestPatchEmptyFile 覆盖空文件可以直接新增段。
func TestPatchEmptyFile(t *testing.T) {
	path := writePatchINI(t, "empty.ini", "")
	if err := PatchValue(path, "relay", "token_ttl", "7s"); err != nil {
		t.Fatalf("PatchValue: %v", err)
	}
	mustValue(t, parsePatched(t, path), "relay", "token_ttl", "7s")
}

// TestPatchDuplicateSectionsMerged 覆盖同名段重复出现时写回归并为一段。
func TestPatchDuplicateSectionsMerged(t *testing.T) {
	original := "[relay]\npair_timeout = 1s\n\n[server]\ntoken = x\n\n[relay]\npair_timeout = 2s\n"
	path := writePatchINI(t, "x.ini", original)
	if err := PatchValue(path, "relay", "pair_timeout", "3s"); err != nil {
		t.Fatalf("PatchValue: %v", err)
	}
	f := parsePatched(t, path)
	mustValue(t, f, "relay", "pair_timeout", "3s")
	mustValue(t, f, "server", "token", "x")
	if got := readPatchINI(t, path); strings.Count(got, "[relay]") != 1 {
		t.Fatalf("同名段应合并为一段: %q", got)
	}
}

// TestPatchRejectsReservedDefaultSection 覆盖 [default] 保留段名拒绝写回。
//
// ini.v1 写回时会把首个大小写不敏感等于 DEFAULT 的段当成隐式默认段并省略段头，
// 写回后文件会变成"段前键值"而无法 Load，因此在写回前拒绝。
func TestPatchRejectsReservedDefaultSection(t *testing.T) {
	original := "[default]\nkey = v\n[relay]\npair_timeout = 10s\n"
	path := writePatchINI(t, "x.ini", original)
	if err := PatchValue(path, "relay", "pair_timeout", "20s"); err == nil {
		t.Fatal("包含 [default] 段应拒绝写回")
	}
	if got := readPatchINI(t, path); got != original {
		t.Fatalf("拒绝时文件不应改变: %q", got)
	}
}

// TestSyncDir 覆盖目录 fsync 分平台实现（Windows 为空操作，Linux 真实调用）。
func TestSyncDir(t *testing.T) {
	dir := t.TempDir()
	if err := syncDir(dir); err != nil {
		t.Fatalf("syncDir(%q): %v", dir, err)
	}
	if runtime.GOOS != "windows" {
		if err := syncDir(filepath.Join(dir, "missing")); err == nil {
			t.Fatal("不存在的目录应报错")
		}
	}
}
