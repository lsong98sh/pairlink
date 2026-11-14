package config

import (
	"fmt"
	"strings"
)

// iniValue 是一条 ini 键值及其在源文件中的行号（行号用于报错定位）。
type iniValue struct {
	// Key 是保留原始大小写的键名。
	Key string
	// Value 是裁剪掉两侧空白后的值。
	Value string
	// Line 是键值所在行（1 起）。
	Line int
}

// iniSection 是一个 ini 段。键按首次出现顺序保存，重复键以最后一次的值为准。
type iniSection struct {
	// Name 是保留原始大小写的段名。
	Name string
	// Line 是段头所在行（1 起）。
	Line int
	// order 是按首次出现顺序排列的规范化键名。
	order []string
	// values 以规范化键名（小写）索引键值。
	values map[string]iniValue
}

// set 写入一个键值；同一个键重复出现时后者覆盖，但保持首次出现的顺序。
func (s *iniSection) set(normKey string, v iniValue) {
	if _, exists := s.values[normKey]; !exists {
		s.order = append(s.order, normKey)
	}
	s.values[normKey] = v
}

// get 按原始键名（不区分大小写）查询键值。
func (s *iniSection) get(key string) (iniValue, bool) {
	v, ok := s.values[strings.ToLower(key)]
	return v, ok
}

// iniFile 是解析后的 ini 文件，保留段与键的出现顺序以便生成稳定警告。
type iniFile struct {
	// sections 以小写段名索引段。
	sections map[string]*iniSection
	// order 是按首次出现顺序排列的规范化段名。
	order []string
}

// section 返回指定段（段名不区分大小写）。
func (f *iniFile) section(name string) (*iniSection, bool) {
	s, ok := f.sections[strings.ToLower(name)]
	return s, ok
}

// ensureSection 返回已有段，不存在时按给定行号创建。
func (f *iniFile) ensureSection(name string, line int) *iniSection {
	norm := strings.ToLower(name)
	if s, ok := f.sections[norm]; ok {
		return s
	}
	s := &iniSection{Name: name, Line: line, values: map[string]iniValue{}}
	f.sections[norm] = s
	f.order = append(f.order, norm)
	return s
}

// parseINI 解析 ini 文本。
//
// 支持的语法：
//   - 空行忽略；
//   - 整行注释以 ';' 或 '#' 开头（允许前置空白）；
//   - 段头 [name]；
//   - 键值 key = value，值两侧空白被裁剪，值内部不做转义与引号处理；
//   - 同一段内重复键后者覆盖，重复段名合并。
//
// 不支持行尾注释：值中出现的 ';' 与 '#' 属于值本身（令牌可能包含这些字符）。
func parseINI(data []byte) (*iniFile, error) {
	text := strings.TrimPrefix(string(data), "\ufeff")
	lines := strings.Split(text, "\n")

	f := &iniFile{sections: map[string]*iniSection{}}
	var current *iniSection

	for i, raw := range lines {
		lineNo := i + 1
		line := strings.TrimSuffix(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, ";") || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			if !strings.HasSuffix(trimmed, "]") {
				return nil, fmt.Errorf("第 %d 行: 段头 %q 缺少结尾的 ']'", lineNo, trimmed)
			}
			name := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
			if name == "" {
				return nil, fmt.Errorf("第 %d 行: 段名为空", lineNo)
			}
			current = f.ensureSection(name, lineNo)
			continue
		}
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok {
			return nil, fmt.Errorf("第 %d 行: 无法解析 %q，键值必须形如 key = value", lineNo, trimmed)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("第 %d 行: 键名为空", lineNo)
		}
		if current == nil {
			return nil, fmt.Errorf("第 %d 行: 键 %q 出现在任何段之前", lineNo, key)
		}
		current.set(strings.ToLower(key), iniValue{
			Key:   key,
			Value: strings.TrimSpace(value),
			Line:  lineNo,
		})
	}
	return f, nil
}
