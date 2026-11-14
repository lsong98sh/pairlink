package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// 包级互斥锁的说明见 patchMu。

// patchMu 串行化同一进程内对 ini 文件的读改写。
//
// 管理页的写操作来自多个 HTTP handler，而 PatchValue/DeleteKey 都是
// "读整份文件 → 改一行 → 原子替换"，并发执行会互相覆盖，因此用包级互斥锁
// 保证进程内串行。跨进程（例如同时用编辑器改同一个 ini）不在保证范围内。
var patchMu sync.Mutex

// PatchValue 把 ini 文件中 [section] 段的 key 值替换为 value。
//
// 语义（阶段 7，docs/contract.md §3.10）：
//   - 只替换 "=" 之后的值部分，保留原行的前导空白、键名大小写与 "=" 前的对齐；
//   - 同一段内重复出现的同名键全部替换（ini 解析器以最后一个为准，只换一个会
//     让新值不生效）；
//   - 段内没有该键时，插到该段最后一个非空行之后（下一个段头之前）；
//   - 段不存在时在文件末尾追加空行、[section]、key = value；
//   - 保留原文件的换行风格（LF / CRLF）、结尾是否有换行、BOM 与权限位；
//   - 写盘使用同目录临时文件 + Rename，失败不会留下半截文件。
//
// 本函数不做任何"重新格式化"：注释、空行、未知键都原样保留。
func PatchValue(path, section, key, value string) error {
	if err := validatePatchArgs(section, key, value); err != nil {
		return err
	}
	patchMu.Lock()
	defer patchMu.Unlock()

	f, err := readPatchFile(path)
	if err != nil {
		return err
	}
	if !f.patchValue(section, key, value) {
		return nil
	}
	return f.write(path)
}

// DeleteKey 删除 ini 文件中 [section] 段的 key 行（含重复键）。
//
// 键或段不存在视为成功；其余行原样保留（连续空行不清理）。
func DeleteKey(path, section, key string) error {
	if err := validatePatchArgs(section, key, ""); err != nil {
		return err
	}
	patchMu.Lock()
	defer patchMu.Unlock()

	f, err := readPatchFile(path)
	if err != nil {
		return err
	}
	if !f.deleteKey(section, key) {
		return nil
	}
	return f.write(path)
}

// validatePatchArgs 校验补丁参数，拒绝会破坏行结构或越界的内容。
func validatePatchArgs(section, key, value string) error {
	if strings.TrimSpace(section) == "" {
		return fmt.Errorf("config: 段名为空")
	}
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("config: 键名为空")
	}
	if strings.ContainsAny(section, "\r\n") || strings.ContainsAny(key, "\r\n") || strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("config: 段名/键名/值不能包含换行")
	}
	if strings.ContainsAny(section, "[]") {
		return fmt.Errorf("config: 段名 %q 不能包含方括号", section)
	}
	if strings.Contains(key, "=") {
		return fmt.Errorf("config: 键名 %q 不能包含 '='", key)
	}
	return nil
}

// patchFile 是保留行结构与换行风格的 ini 文本。
type patchFile struct {
	// lines 是去掉行尾 CR 的各行；不含 '\n'。
	lines []string
	// crlf 表示原文件使用 CRLF 换行。
	crlf bool
	// trailingNewline 表示原文件以换行结尾。
	trailingNewline bool
	// bom 表示原文件带 UTF-8 BOM。
	bom bool
	// mode 是原文件权限位。
	mode os.FileMode
	// path 是原文件路径（写入用）。
	path string
}

// readPatchFile 读取并解析 ini 文本结构。
func readPatchFile(path string) (*patchFile, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("config: 配置文件路径为空")
	}
	abs, err := filepath.Abs(filepath.FromSlash(path))
	if err != nil {
		return nil, fmt.Errorf("config: 解析配置文件路径 %q: %w", path, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("config: 读取配置文件 %q: %w", abs, err)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("config: 读取配置文件 %q: %w", abs, err)
	}

	f := &patchFile{path: abs, mode: info.Mode().Perm()}
	text := string(data)
	if strings.HasPrefix(text, "\ufeff") {
		f.bom = true
		text = strings.TrimPrefix(text, "\ufeff")
	}
	f.crlf = strings.Contains(text, "\r\n")
	f.trailingNewline = strings.HasSuffix(text, "\n")
	if f.trailingNewline {
		text = strings.TrimSuffix(text, "\n")
	}
	if text == "" {
		return f, nil
	}
	raw := strings.Split(text, "\n")
	f.lines = make([]string, len(raw))
	for i, line := range raw {
		f.lines[i] = strings.TrimSuffix(line, "\r")
	}
	return f, nil
}

// eol 返回该文件使用的换行符。
func (f *patchFile) eol() string {
	if f.crlf {
		return "\r\n"
	}
	return "\n"
}

// render 把行结构还原为字节。
func (f *patchFile) render() []byte {
	var b strings.Builder
	if f.bom {
		b.WriteString("\ufeff")
	}
	eol := f.eol()
	b.WriteString(strings.Join(f.lines, eol))
	if f.trailingNewline && len(f.lines) > 0 {
		b.WriteString(eol)
	}
	return []byte(b.String())
}

// write 原子写回文件。
func (f *patchFile) write(path string) error {
	return writeFileAtomic(path, f.render(), f.mode)
}

// sectionRanges 返回 [section] 段头行号与下一个段头行号（不含）。
//
// 同名段重复出现时返回多个区间（ini 解析器会合并同名段，补丁必须覆盖所有出现）。
// 段不存在时返回空切片。
func (f *patchFile) sectionRanges(section string) [][2]int {
	var out [][2]int
	start := -1
	for i, line := range f.lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "[") || !strings.HasSuffix(trimmed, "]") {
			continue
		}
		if start >= 0 {
			out = append(out, [2]int{start, i})
			start = -1
		}
		name := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
		if strings.EqualFold(name, section) {
			start = i
		}
	}
	if start >= 0 {
		out = append(out, [2]int{start, len(f.lines)})
	}
	return out
}

// keyLineEq 判断一行是否是目标键；返回 '=' 的位置。
func keyLineEq(line, key string) (int, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, ";") || strings.HasPrefix(trimmed, "#") ||
		strings.HasPrefix(trimmed, "[") {
		return 0, false
	}
	eq := strings.Index(line, "=")
	if eq < 0 {
		return 0, false
	}
	if !strings.EqualFold(strings.TrimSpace(line[:eq]), key) {
		return 0, false
	}
	return eq, true
}

// patchValue 替换或插入键值；返回是否有改动。
func (f *patchFile) patchValue(section, key, value string) bool {
	ranges := f.sectionRanges(section)
	if len(ranges) == 0 {
		f.appendSection(section, key, value)
		return true
	}
	replaced := false
	for _, r := range ranges {
		for i := r[0] + 1; i < r[1]; i++ {
			eq, isKey := keyLineEq(f.lines[i], key)
			if !isKey {
				continue
			}
			f.lines[i] = replaceValuePart(f.lines[i], eq, value)
			replaced = true
		}
	}
	if replaced {
		return true
	}

	// 键不存在：插到最后一个同名段内最后一个非空行之后；段内全空则紧跟段头。
	last := ranges[len(ranges)-1]
	insertAt := last[0] + 1
	for i := last[0] + 1; i < last[1]; i++ {
		if strings.TrimSpace(f.lines[i]) != "" {
			insertAt = i + 1
		}
	}
	f.insertLine(insertAt, key+" = "+value)
	return true
}

// deleteKey 删除键行；返回是否有改动。
func (f *patchFile) deleteKey(section, key string) bool {
	ranges := f.sectionRanges(section)
	if len(ranges) == 0 {
		return false
	}
	remove := make(map[int]bool, len(f.lines))
	for _, r := range ranges {
		for i := r[0] + 1; i < r[1]; i++ {
			if _, isKey := keyLineEq(f.lines[i], key); isKey {
				remove[i] = true
			}
		}
	}
	if len(remove) == 0 {
		return false
	}
	out := make([]string, 0, len(f.lines)-len(remove))
	for i, line := range f.lines {
		if remove[i] {
			continue
		}
		out = append(out, line)
	}
	f.lines = out
	return true
}

// appendSection 在文件末尾追加空行、段头与键值。
func (f *patchFile) appendSection(section, key, value string) {
	if len(f.lines) > 0 {
		// 文件末尾补一个空行；若已有尾部空行则不重复添加。
		if strings.TrimSpace(f.lines[len(f.lines)-1]) != "" {
			f.lines = append(f.lines, "")
		}
	}
	f.lines = append(f.lines, "["+section+"]", key+" = "+value)
	f.trailingNewline = true
}

// insertLine 在指定位置插入一行。
func (f *patchFile) insertLine(at int, line string) {
	if at < 0 {
		at = 0
	}
	if at > len(f.lines) {
		at = len(f.lines)
	}
	f.lines = append(f.lines, "")
	copy(f.lines[at+1:], f.lines[at:])
	f.lines[at] = line
}

// replaceValuePart 只替换 "=" 之后的值，保留 "=" 前的内容、值前空白与行尾空白。
func replaceValuePart(line string, eq int, value string) string {
	if eq < 0 || eq >= len(line) {
		return line
	}
	rest := line[eq+1:]
	lead := 0
	for lead < len(rest) && (rest[lead] == ' ' || rest[lead] == '\t') {
		lead++
	}
	end := len(rest)
	for end > lead && (rest[end-1] == ' ' || rest[end-1] == '\t') {
		end--
	}
	return line[:eq+1] + rest[:lead] + value + rest[end:]
}

// writeFileAtomic 通过同目录临时文件 + Rename 原子替换目标文件。
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".pairlink-ini-*.tmp")
	if err != nil {
		return fmt.Errorf("config: 创建临时文件（目录 %s）: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("config: 写临时文件 %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("config: 同步临时文件 %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		cleanup()
		return fmt.Errorf("config: 设置临时文件权限 %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("config: 关闭临时文件 %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("config: 替换配置文件 %s: %w", path, err)
	}
	return nil
}
