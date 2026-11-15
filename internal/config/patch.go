package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	ini "gopkg.in/ini.v1"
)

// patchMu 串行化同一进程内对 ini 文件的读改写。
//
// 管理页的写操作来自多个 HTTP handler，而 PatchValue/DeleteKey 都是
// "读整份文件 → 改一项 → 原子替换"，并发执行会互相覆盖，因此用包级互斥锁
// 保证进程内串行。跨进程（例如同时用编辑器改同一个 ini）不在保证范围内。
var patchMu sync.Mutex

// PatchValue 把 ini 文件中 [section] 段的 key 值替换为 value。
//
// 语义（v0.5.0 起改用 gopkg.in/ini.v1 读写，docs/contract.md §3.10）：
//   - 用 ini.v1 重新生成整份文件：注释会尽量保留，但空行、键顺序、对齐、
//     换行风格（LF/CRLF）、键名大小写、重复键/重复段的写法都可能被归一；
//   - 段名/键名大小写不敏感；同名段会被合并成一段（解析器语义）；
//   - 键不存在时追加到该段末尾；段不存在时在文件末尾追加 [section]；
//   - 保留原文件的 BOM 与权限位；写盘使用同目录临时文件 + Rename + 父目录 fsync；
//   - 进程内用包级互斥锁串行化。
//
// 值中的换行、段名中的方括号、键名中的 '=' 一律拒绝（防 ini 行注入）。
func PatchValue(path, section, key, value string) error {
	if err := validatePatchArgs(section, key, value); err != nil {
		return err
	}
	// 与读取语义一致：值两侧空白在解析时本来就会被裁剪，这里先归一，
	// 避免 ini.v1 把带首尾空白的值写成带引号的形式。
	section = strings.TrimSpace(section)
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)

	patchMu.Lock()
	defer patchMu.Unlock()

	doc, info, err := loadPatchDoc(path)
	if err != nil {
		return err
	}
	keys := matchingKeys(doc, section, key)
	if len(keys) == 0 {
		sec := matchingSection(doc, section)
		if sec == nil {
			sec, err = doc.NewSection(section)
			if err != nil {
				return fmt.Errorf("config: 新增段 [%s]: %w", section, err)
			}
		}
		if _, err := sec.NewKey(key, value); err != nil {
			return fmt.Errorf("config: 写入 [%s] %s: %w", section, key, err)
		}
	}
	for _, k := range keys {
		k.SetValue(value)
	}
	return writePatchDoc(info, doc)
}

// DeleteKey 删除 ini 文件中 [section] 段的 key（同名键大小写不敏感，全部删除）。
//
// 键或段不存在视为成功（且不写盘，文件逐字节不变）。
func DeleteKey(path, section, key string) error {
	if err := validatePatchArgs(section, key, ""); err != nil {
		return err
	}
	section = strings.TrimSpace(section)
	key = strings.TrimSpace(key)

	patchMu.Lock()
	defer patchMu.Unlock()

	doc, info, err := loadPatchDoc(path)
	if err != nil {
		return err
	}
	changed := false
	for _, sec := range matchingSections(doc, section) {
		for _, k := range sec.Keys() {
			if !strings.EqualFold(k.Name(), key) {
				continue
			}
			// 先按精确键名删除自己，避免大小写变体互相干扰。
			sec.DeleteKey(k.Name())
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return writePatchDoc(info, doc)
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
	if strings.EqualFold(strings.TrimSpace(section), ini.DefaultSection) {
		return fmt.Errorf("config: 段名 %q 是 ini 保留段名，不能作为 [section] 写入", section)
	}
	if strings.Contains(key, "=") {
		return fmt.Errorf("config: 键名 %q 不能包含 '='", key)
	}
	return nil
}

// patchFileInfo 是写回时需要保留的文件元信息。
type patchFileInfo struct {
	// path 是配置文件的绝对路径。
	path string
	// mode 是原文件权限位。
	mode os.FileMode
	// bom 表示原文件带 UTF-8 BOM（写回时保留）。
	bom bool
}

// loadPatchDoc 读取并解析 ini 文件，同时记录待保留的元信息。
func loadPatchDoc(path string) (*ini.File, patchFileInfo, error) {
	if strings.TrimSpace(path) == "" {
		return nil, patchFileInfo{}, fmt.Errorf("config: 配置文件路径为空")
	}
	abs, err := filepath.Abs(filepath.FromSlash(path))
	if err != nil {
		return nil, patchFileInfo{}, fmt.Errorf("config: 解析配置文件路径 %q: %w", path, err)
	}
	stat, err := os.Stat(abs)
	if err != nil {
		return nil, patchFileInfo{}, fmt.Errorf("config: 读取配置文件 %q: %w", abs, err)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, patchFileInfo{}, fmt.Errorf("config: 读取配置文件 %q: %w", abs, err)
	}
	doc, err := ini.LoadSources(iniLoadOptions, normalizeSectionHeaders(data))
	if err != nil {
		return nil, patchFileInfo{}, fmt.Errorf("config: 解析配置文件 %q: %w", abs, translateINIError(err))
	}
	// 段前键值在 Load 阶段会被拒绝；写回也不能把它留在文件里。
	if def, derr := doc.GetSection(ini.DefaultSection); derr == nil && len(def.Keys()) > 0 {
		return nil, patchFileInfo{}, fmt.Errorf("config: 配置文件 %q 的键值出现在任何段之前（缺少 [section] 段头）", abs)
	}
	// ini.v1 写回时会把"第一个、大小写不敏感等于 DEFAULT 的段"当作隐式默认段
	// 并省略段头（[default] 也会中招），写回后再 Load 会变成"段前键值"。
	// 这种段名是保留名，直接拒绝写回，避免把文件写坏。
	for _, sec := range doc.Sections() {
		if sec.Name() != ini.DefaultSection && strings.EqualFold(sec.Name(), ini.DefaultSection) {
			return nil, patchFileInfo{}, fmt.Errorf("config: 配置文件 %q 包含 ini 保留段名 [%s]，拒绝写回", abs, sec.Name())
		}
	}
	info := patchFileInfo{
		path: abs,
		mode: stat.Mode().Perm(),
		bom:  bytes.HasPrefix(data, []byte("\ufeff")),
	}
	return doc, info, nil
}

// matchingSections 返回段名不区分大小写匹配的全部段（不含隐式默认段）。
func matchingSections(doc *ini.File, name string) []*ini.Section {
	var out []*ini.Section
	for _, sec := range doc.Sections() {
		if sec.Name() == ini.DefaultSection {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(sec.Name()), name) {
			out = append(out, sec)
		}
	}
	return out
}

// matchingSection 返回第一个匹配的段，不存在时返回 nil。
func matchingSection(doc *ini.File, name string) *ini.Section {
	secs := matchingSections(doc, name)
	if len(secs) == 0 {
		return nil
	}
	return secs[0]
}

// matchingKeys 返回所有匹配段中键名不区分大小写匹配的键。
//
// 同一段内大小写不同的重复键在 ini.v1 里是不同条目（不设置 Insensitive 时），
// 这里全部返回，保证写回后读取仍拿到新值（与旧实现"重复键全部替换"一致）。
func matchingKeys(doc *ini.File, section, key string) []*ini.Key {
	var out []*ini.Key
	for _, sec := range matchingSections(doc, section) {
		for _, k := range sec.Keys() {
			if strings.EqualFold(k.Name(), key) {
				out = append(out, k)
			}
		}
	}
	return out
}

// writePatchDoc 把 ini.v1 文档渲染为字节并原子写回。
func writePatchDoc(info patchFileInfo, doc *ini.File) error {
	var buf bytes.Buffer
	if info.bom {
		buf.WriteString("\ufeff")
	}

	// ini.v1 的排版参数是包级变量：默认 PrettyFormat=true 会把同一段内的
	// "=" 对齐到最长键，换行符在 Windows 上还会变成 CRLF。这里在写回期间
	// 临时改成统一的 "key = value" + LF（patchMu 保证串行），写回后再恢复，
	// 避免把依赖库的全局状态永久改掉。
	prettyFormat, prettyEqual, lineBreak := ini.PrettyFormat, ini.PrettyEqual, ini.LineBreak
	ini.PrettyFormat, ini.PrettyEqual, ini.LineBreak = false, true, "\n"
	defer func() {
		ini.PrettyFormat, ini.PrettyEqual, ini.LineBreak = prettyFormat, prettyEqual, lineBreak
	}()
	if _, err := doc.WriteTo(&buf); err != nil {
		return fmt.Errorf("config: 生成配置文本: %w", err)
	}
	return writeFileAtomic(info.path, buf.Bytes(), info.mode)
}

// writeFileAtomic 通过同目录临时文件 + Rename + 父目录 fsync 原子替换目标文件。
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
	// Rename 只改目录项：不 fsync 父目录时，崩溃/掉电后 rename 可能没有落盘，
	// 文件会回到旧内容甚至消失。Windows 没有等价的目录 fsync，见 syncDir 的分平台实现。
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("config: 同步目录 %s: %w", dir, err)
	}
	return nil
}
