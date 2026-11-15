package logx

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock 是可注入的时钟：单测用它把"跨天"变成确定事件。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// localDay 返回本地时区某天某点的时刻。
func localDay(year int, month time.Month, day, hour int) time.Time {
	return time.Date(year, month, day, hour, 0, 0, 0, time.Local)
}

// openTestLogger 用注入时钟打开日切 logger。
func openTestLogger(t *testing.T, path string, clock *fakeClock, keepDays *int, mode string) (*slog.Logger, io.Closer) {
	t.Helper()
	log, closer, err := NewFileLogger(path, Config{
		Level:    "debug",
		Format:   "text",
		Mode:     mode,
		KeepDays: keepDays,
		now:      clock.now,
	})
	if err != nil {
		t.Fatalf("NewFileLogger(%q) 失败: %v", path, err)
	}
	return log, closer
}

// listNames 返回目录内的文件名（排序后）。
func listNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取目录 %q 失败: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// readLogLines 读取日志文件的非空行。
func readLogLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取日志文件 %q 失败: %v", path, err)
	}
	text := strings.TrimSuffix(string(data), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %q 失败: %v", path, err)
	}
	return st.Size()
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%q 不应存在（err=%v）", path, err)
	}
}

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%q 应存在: %v", path, err)
	}
}

func mustWriteFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("预置文件 %q 失败: %v", path, err)
	}
}

// TestFileLoggerSameDayAppends 覆盖命名规则与"同一天多次写入追加到同一个文件"。
func TestFileLoggerSameDayAppends(t *testing.T) {
	cases := []struct {
		name string
		base string
		want string
	}{
		{"有扩展名", "relay.log", "relay-2026-11-15.log"},
		{"无扩展名", "relay", "relay-2026-11-15"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			clock := newFakeClock(localDay(2026, time.November, 15, 8))
			days := 7
			log, closer := openTestLogger(t, filepath.Join(dir, tc.base), clock, &days, "relay")

			for i := 0; i < 3; i++ {
				log.Info("同一天写入", "i", i)
			}
			if err := closer.Close(); err != nil {
				t.Fatalf("关闭失败: %v", err)
			}

			// 只应有一个日文件：不创建无日期的原始基名文件。
			if got := listNames(t, dir); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("目录内文件 = %v, want [%s]", got, tc.want)
			}
			mustNotExist(t, filepath.Join(dir, tc.base))

			lines := readLogLines(t, filepath.Join(dir, tc.want))
			if len(lines) != 3 {
				t.Fatalf("行数 = %d, want 3: %v", len(lines), lines)
			}
			for i, line := range lines {
				if !strings.HasPrefix(line, "time=") {
					t.Fatalf("第 %d 行应以 time= 开头（追加而非覆盖）: %q", i, line)
				}
				if !strings.Contains(line, fmt.Sprintf("i=%d", i)) {
					t.Fatalf("第 %d 行缺少 i=%d: %q", i, i, line)
				}
			}
		})
	}
}

// TestFileLoggerRollsOverAtDateChange 覆盖跨天切换：日期变化时开新文件、旧文件
// 停止增长、并在切换时清理新窗口之外的日文件。
func TestFileLoggerRollsOverAtDateChange(t *testing.T) {
	dir := t.TempDir()
	// 启动日（11-15）保留窗口 = 11-13 起：11-13 保留，切换日（11-16）窗口变
	// 11-14 起，11-13 应被这次切换清掉。
	mustWriteFile(t, filepath.Join(dir, "relay-2026-11-13.log"), "history 13\n")
	mustWriteFile(t, filepath.Join(dir, "relay-2026-11-12.log"), "history 12\n")

	clock := newFakeClock(localDay(2026, time.November, 15, 23))
	days := 2
	log, closer := openTestLogger(t, filepath.Join(dir, "relay.log"), clock, &days, "relay")
	defer closer.Close()

	day1 := filepath.Join(dir, "relay-2026-11-15.log")
	log.Info("第一天")
	size1 := fileSize(t, day1)
	mustExist(t, filepath.Join(dir, "relay-2026-11-13.log"))
	mustNotExist(t, filepath.Join(dir, "relay-2026-11-12.log"))

	clock.set(localDay(2026, time.November, 16, 0))
	log.Info("第二天")

	day2 := filepath.Join(dir, "relay-2026-11-16.log")
	if got := fileSize(t, day1); got != size1 {
		t.Fatalf("跨天后旧文件仍在增长: %d → %d 字节", size1, got)
	}
	lines1 := readLogLines(t, day1)
	if len(lines1) != 1 || !strings.Contains(lines1[0], "第一天") {
		t.Fatalf("旧文件内容错误: %v", lines1)
	}
	lines2 := readLogLines(t, day2)
	if len(lines2) != 1 || !strings.Contains(lines2[0], "第二天") {
		t.Fatalf("新文件内容错误: %v", lines2)
	}
	// 切换后 11-13 已在新窗口（11-14 起）之外。
	mustNotExist(t, filepath.Join(dir, "relay-2026-11-13.log"))

	// 再写一天：仍在同一个（第二天的）文件里追加。
	log.Info("第二天再写")
	if got := len(readLogLines(t, day2)); got != 2 {
		t.Fatalf("第二天文件行数 = %d, want 2", got)
	}
	if got := fileSize(t, day1); got != size1 {
		t.Fatalf("旧文件又变了: %d → %d 字节", size1, got)
	}
}

// TestFileLoggerCleanupKeepsRetentionWindow 覆盖启动清理：只删"同目录 + 同基名 +
// 严格日期格式 + 早于 today-keepDays"的文件，其它一律不动。
func TestFileLoggerCleanupKeepsRetentionWindow(t *testing.T) {
	dir := t.TempDir()
	// keepDays = 2、today = 11-15 → cutoff = 11-13（早于它才删）。
	remove := []string{
		"relay-2026-11-10.log", // 5 天前
		"relay-2026-11-12.log", // 3 天前
	}
	keep := []string{
		"relay-2026-11-13.log",     // 2 天前：窗口边界，保留
		"relay-2026-11-14.log",     // 1 天前
		"other-2026-11-10.log",     // 不同基名
		"relay.log",                // 无日期的原始文件
		"relay-2026-1-01.log",      // 日期未补零
		"relay-2026-13-01.log",     // 月份非法
		"relay-2026-01-01.log.bak", // 后缀不是 .log
		"relay-2026-01-01-old.log", // 基名主干不严格一致
		"relayx-2026-01-01.log",    // 前缀多一个字符
	}
	for _, name := range append(append([]string{}, remove...), keep...) {
		mustWriteFile(t, filepath.Join(dir, name), "history\n")
	}

	clock := newFakeClock(localDay(2026, time.November, 15, 9))
	days := 2
	log, closer := openTestLogger(t, filepath.Join(dir, "relay.log"), clock, &days, "relay")
	log.Info("启动")
	if err := closer.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}

	for _, name := range remove {
		mustNotExist(t, filepath.Join(dir, name))
	}
	for _, name := range append(keep, "relay-2026-11-15.log") {
		mustExist(t, filepath.Join(dir, name))
	}
	// 新文件应有内容（清理不影响写入）。
	if lines := readLogLines(t, filepath.Join(dir, "relay-2026-11-15.log")); len(lines) != 1 {
		t.Fatalf("当天日志行数 = %d, want 1: %v", len(lines), lines)
	}
}

// TestFileLoggerConcurrentWritesStayIntact 覆盖 20 goroutine × 50 行并发写入：
// 行数正确、每行都以时间戳开头且完整（无交错/截断）。
func TestFileLoggerConcurrentWritesStayIntact(t *testing.T) {
	dir := t.TempDir()
	clock := newFakeClock(localDay(2026, time.November, 15, 12))
	days := 7
	log, closer := openTestLogger(t, filepath.Join(dir, "relay.log"), clock, &days, "relay")

	const writers, perWriter = 20, 50
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				log.Info("并发写入", "goroutine", w, "i", i)
			}
		}(w)
	}
	wg.Wait()
	if err := closer.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}

	path := filepath.Join(dir, "relay-2026-11-15.log")
	lines := readLogLines(t, path)
	if len(lines) != writers*perWriter {
		t.Fatalf("行数 = %d, want %d", len(lines), writers*perWriter)
	}
	lineRE := regexp.MustCompile(`^time=\S+ level=INFO msg=并发写入 mode=relay goroutine=(\d+) i=(\d+)$`)
	seen := make(map[string]bool, len(lines))
	for i, line := range lines {
		m := lineRE.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("第 %d 行不完整或被交错: %q", i, line)
		}
		key := m[1] + "/" + m[2]
		if seen[key] {
			t.Fatalf("第 %d 行重复（goroutine/i = %s）: %q", i, key, line)
		}
		seen[key] = true
	}
	if len(seen) != writers*perWriter {
		t.Fatalf("唯一记录数 = %d, want %d", len(seen), writers*perWriter)
	}
}

// TestFileLoggerRolloverUnderConcurrency 在写入持续进行时来回拨动时钟（反复跨天
// 切换），断言：一条记录都不丢、每行仍然完整、只产生这两天的文件。
func TestFileLoggerRolloverUnderConcurrency(t *testing.T) {
	dir := t.TempDir()
	clock := newFakeClock(localDay(2026, time.November, 15, 23))
	days := 7
	log, closer := openTestLogger(t, filepath.Join(dir, "relay.log"), clock, &days, "relay")

	const writers, perWriter = 8, 40
	var wg sync.WaitGroup
	stop := make(chan struct{})
	var flipWG sync.WaitGroup
	flipWG.Add(1)
	go func() { // 反复跨天：切换必须安全且不丢行
		defer flipWG.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				clock.set(localDay(2026, time.November, 16, 0))
			} else {
				clock.set(localDay(2026, time.November, 15, 23))
			}
			time.Sleep(time.Millisecond)
		}
	}()
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				log.Info("并发跨天", "goroutine", w, "i", i)
			}
		}(w)
	}
	wg.Wait()
	close(stop)
	flipWG.Wait()
	if err := closer.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}

	lineRE := regexp.MustCompile(`^time=\S+ level=INFO msg=并发跨天 mode=relay goroutine=(\d+) i=(\d+)$`)
	total := 0
	seen := make(map[string]bool, writers*perWriter)
	for _, name := range listNames(t, dir) {
		if !strings.HasPrefix(name, "relay-") {
			t.Fatalf("出现非本基名文件: %q", name)
		}
		for _, line := range readLogLines(t, filepath.Join(dir, name)) {
			m := lineRE.FindStringSubmatch(line)
			if m == nil {
				t.Fatalf("%s 中出现不完整/交错的行: %q", name, line)
			}
			seen[m[1]+"/"+m[2]] = true
			total++
		}
	}
	if total != writers*perWriter {
		t.Fatalf("跨天切换期间总行数 = %d, want %d（有日志丢失）", total, writers*perWriter)
	}
	if len(seen) != writers*perWriter {
		t.Fatalf("唯一记录数 = %d, want %d", len(seen), writers*perWriter)
	}
	// 只应有 11-15 与 11-16 两个日文件。
	for _, name := range listNames(t, dir) {
		if name != "relay-2026-11-15.log" && name != "relay-2026-11-16.log" {
			t.Fatalf("意外的文件: %q", name)
		}
	}
}

// TestFileLoggerKeepDaysZeroKeepsHistory 覆盖 log_keep_days = 0（永久保留）。
func TestFileLoggerKeepDaysZeroKeepsHistory(t *testing.T) {
	dir := t.TempDir()
	history := []string{"relay-2020-01-01.log", "relay-2026-11-10.log", "relay-2026-11-12.log"}
	for _, name := range history {
		mustWriteFile(t, filepath.Join(dir, name), "history\n")
	}
	clock := newFakeClock(localDay(2026, time.November, 15, 9))
	days := 0
	log, closer := openTestLogger(t, filepath.Join(dir, "relay.log"), clock, &days, "relay")
	log.Info("不删历史")
	if err := closer.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	for _, name := range append(history, "relay-2026-11-15.log") {
		mustExist(t, filepath.Join(dir, name))
	}
}

// TestKeepDaysNegativeRejected 覆盖 log_keep_days < 0 的校验失败。
func TestKeepDaysNegativeRejected(t *testing.T) {
	if err := ValidateKeepDays(-1); err == nil {
		t.Fatal("ValidateKeepDays(-1) 应报错")
	}
	if err := ValidateKeepDays(0); err != nil {
		t.Fatalf("ValidateKeepDays(0) 不应报错: %v", err)
	}

	dir := t.TempDir()
	days := -1
	log, closer, err := NewFileLogger(filepath.Join(dir, "logs", "relay.log"),
		Config{KeepDays: &days})
	if err == nil {
		_ = closer.Close()
		t.Fatalf("负数保留天数应在构造时报错，got logger=%v", log)
	}
	if !strings.Contains(err.Error(), "log_keep_days") {
		t.Fatalf("错误信息应点名 log_keep_days: %v", err)
	}
	// 校验失败发生在创建目录之前：不应留下任何文件/目录。
	mustNotExist(t, filepath.Join(dir, "logs"))
}

// TestFileLoggerCreatesMissingDirs 保持"目录不存在时自动创建"的既有行为。
func TestFileLoggerCreatesMissingDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "c", "relay.log")
	clock := newFakeClock(localDay(2026, time.November, 15, 9))
	days := 7
	log, closer := openTestLogger(t, path, clock, &days, "relay")
	log.Info("目录自动创建")
	if err := closer.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	mustExist(t, filepath.Join(dir, "a", "b", "c", "relay-2026-11-15.log"))
}

// TestFileLoggerEmptyPathUsesDefaultOutput 覆盖 log_path 为空 → 默认输出（stderr）。
func TestFileLoggerEmptyPathUsesDefaultOutput(t *testing.T) {
	var buf bytes.Buffer
	SetDefaultOutput(&buf)
	defer SetDefaultOutput(nil)

	log, closer, err := NewFileLogger("   ", Config{Mode: "relay", Format: "text"})
	if err != nil {
		t.Fatalf("空路径应退回默认输出而不报错: %v", err)
	}
	log.Info("写 stderr")
	if err := closer.Close(); err != nil {
		t.Fatalf("空路径的 Closer 应可关闭: %v", err)
	}
	if !strings.Contains(buf.String(), "写 stderr") || !strings.Contains(buf.String(), "mode=relay") {
		t.Fatalf("空路径应写入默认输出: %q", buf.String())
	}
}

// TestCleanupIgnoresDirectories 确认同名目录不会被当作日文件删除。
func TestCleanupIgnoresDirectories(t *testing.T) {
	dir := t.TempDir()
	squatter := filepath.Join(dir, "relay-2020-01-01.log")
	if err := os.Mkdir(squatter, 0o755); err != nil {
		t.Fatalf("预置目录失败: %v", err)
	}
	mustWriteFile(t, filepath.Join(squatter, "keep.txt"), "x\n")

	clock := newFakeClock(localDay(2026, time.November, 15, 9))
	days := 2
	log, closer := openTestLogger(t, filepath.Join(dir, "relay.log"), clock, &days, "relay")
	log.Info("目录不删")
	if err := closer.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	mustExist(t, filepath.Join(squatter, "keep.txt"))
}

// TestCleanupFailureOnlyWarns 覆盖"过期日文件删不掉"（Windows 文件被占用、
// 权限不足等）：只记 WARN，不返回错误、不影响后续写入。
//
// 删除失败无法跨平台可靠构造（Windows 上 Go 的 os.Remove 会先清只读属性，
// Unix 的删除只受目录权限约束），因此注入一个总是失败的 remove，
// 走的仍是 cleanOldLocked → Write → warn 的真实路径。
func TestCleanupFailureOnlyWarns(t *testing.T) {
	dir := t.TempDir()
	clock := newFakeClock(localDay(2026, time.November, 15, 9))
	days := 2
	log, closer := openTestLogger(t, filepath.Join(dir, "relay.log"), clock, &days, "relay")
	defer closer.Close()

	// 模拟"停机期间跨了一天"：11-16 的保留窗口从 11-14 起，11-13 已过期。
	hist := filepath.Join(dir, "relay-2026-11-13.log")
	mustWriteFile(t, hist, "history\n")
	w := closer.(*dateWriter)
	w.mu.Lock()
	w.remove = func(string) error { return os.ErrPermission }
	w.mu.Unlock()

	clock.set(localDay(2026, time.November, 16, 9))
	log.Info("清理失败仍继续写")

	mustExist(t, hist) // 删不掉就保留，绝不因为删除失败而中断
	day2 := filepath.Join(dir, "relay-2026-11-16.log")
	data, err := os.ReadFile(day2)
	if err != nil {
		t.Fatalf("读取 %q 失败: %v", day2, err)
	}
	if !strings.Contains(string(data), "log_cleanup_fail") {
		t.Fatalf("删除失败应记 WARN（event=log_cleanup_fail）: %q", string(data))
	}
	if !strings.Contains(string(data), "清理失败仍继续写") {
		t.Fatalf("清理失败后写入应继续: %q", string(data))
	}
	log.Info("第二条")
	if lines := readLogLines(t, day2); len(lines) != 3 { // 业务 2 条 + WARN 1 条
		t.Fatalf("%q 行数 = %d, want 3: %v", day2, len(lines), lines)
	}
}

// TestWarnCleanupFailure 与平台无关地覆盖 WARN 出口本身。
func TestWarnCleanupFailure(t *testing.T) {
	dir := t.TempDir()
	clock := newFakeClock(localDay(2026, time.November, 15, 9))
	days := 2
	_, closer := openTestLogger(t, filepath.Join(dir, "relay.log"), clock, &days, "relay")
	defer closer.Close()

	w := closer.(*dateWriter)
	w.warn([]cleanupFailure{{path: filepath.Join(dir, "relay-2020-01-01.log"), err: os.ErrPermission}})

	data, err := os.ReadFile(filepath.Join(dir, "relay-2026-11-15.log"))
	if err != nil {
		t.Fatalf("读取当天日志失败: %v", err)
	}
	if !strings.Contains(string(data), "level=WARN") || !strings.Contains(string(data), "log_cleanup_fail") {
		t.Fatalf("warn 应发出 WARN 级事件: %q", string(data))
	}
}

// TestDatedPath 覆盖服务模式把裸 stderr 落到当天文件所用的路径计算：
// 命名规则必须与写入器完全一致（有/无扩展名两种情况）。
func TestDatedPath(t *testing.T) {
	day := localDay(2026, time.November, 15, 9)
	cases := []struct{ base, want string }{
		{filepath.Join("logs", "relay.log"), filepath.Join("logs", "relay-2026-11-15.log")},
		{filepath.Join("logs", "relay"), filepath.Join("logs", "relay-2026-11-15")},
	}
	for _, c := range cases {
		got, err := DatedPath(c.base, day)
		if err != nil {
			t.Fatalf("DatedPath(%q): %v", c.base, err)
		}
		if got != c.want {
			t.Fatalf("DatedPath(%q) = %q, want %q", c.base, got, c.want)
		}
	}
	if _, err := DatedPath("", day); err == nil {
		t.Fatal("空路径应报错")
	}
}

// TestDatedPathMatchesWriter 保证 DatedPath 与写入器实际创建的文件同名。
func TestDatedPathMatchesWriter(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "relay.log")
	clock := newFakeClock(localDay(2026, time.November, 15, 9))
	days := 7
	log, closer := openTestLogger(t, base, clock, &days, "relay")
	log.Info("路径一致性")
	if err := closer.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	want, err := DatedPath(base, localDay(2026, time.November, 15, 9))
	if err != nil {
		t.Fatalf("DatedPath: %v", err)
	}
	mustExist(t, want)
}
