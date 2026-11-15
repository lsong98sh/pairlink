package config

import (
	"bytes"
	"fmt"
	"strings"

	ini "gopkg.in/ini.v1"
)

// iniLoadOptions 是 ini.v1 的加载选项，用于复刻 v0.4.0 手写解析器的语义：
//
//   - 不设置 Insensitive：ini.v1 会把段名/键名改成小写，导致 `service.FOO`
//     这类大写服务ID 绕过 validateServiceID 的 `[a-z0-9-_]` 校验。本包改为
//     在 iniFile 中按小写索引、同时保留原始大小写（见 iniSection）；
//   - IgnoreInlineComment：关闭行尾注释，值中的 ';' / '#' 属于值本身；
//   - IgnoreContinuation：关闭反斜杠续行，行尾 '\' 属于值本身；
//   - KeyValueDelimiters：只认 '='，与旧解析器一致（':' 属于值，如 host:port）；
//   - PreserveSurroundedQuote：保留值两侧的引号，旧解析器不做引号处理。
var iniLoadOptions = ini.LoadOptions{
	IgnoreInlineComment:     true,
	IgnoreContinuation:      true,
	KeyValueDelimiters:      "=",
	PreserveSurroundedQuote: true,
}

// iniValue 是一条 ini 键值。
type iniValue struct {
	// Key 是保留原始大小写的键名。
	Key string
	// Value 是裁剪掉两侧空白后的值。
	Value string
}

// iniSection 是一个 ini 段。键按首次出现顺序保存，重复键以最后一次的值为准。
type iniSection struct {
	// Name 是保留原始大小写的段名。
	Name string
	// order 是按首次出现顺序排列的规范化键名（小写）。
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

// ensureSection 返回已有段，不存在时创建。
func (f *iniFile) ensureSection(name string) *iniSection {
	norm := strings.ToLower(name)
	if s, ok := f.sections[norm]; ok {
		return s
	}
	s := &iniSection{Name: name, values: map[string]iniValue{}}
	f.sections[norm] = s
	f.order = append(f.order, norm)
	return s
}

// parseINI 使用 gopkg.in/ini.v1 解析 ini 文本。
//
// 支持的语法：
//   - 空行忽略；
//   - 整行注释以 ';' 或 '#' 开头（允许前置空白）；
//   - 段头 [name]（段名两侧空白被裁剪）；
//   - 键值 key = value，值两侧空白被裁剪，值内部不做转义与引号处理；
//   - 同一段内重复键后者覆盖，重复段名合并。
//
// 不支持行尾注释：值中出现的 ';' 与 '#' 属于值本身（令牌可能包含这些字符）。
func parseINI(data []byte) (*iniFile, error) {
	doc, err := ini.LoadSources(iniLoadOptions, normalizeSectionHeaders(data))
	if err != nil {
		return nil, fmt.Errorf("解析 ini: %w", translateINIError(err))
	}

	f := &iniFile{sections: map[string]*iniSection{}}
	for _, sec := range doc.Sections() {
		name := strings.TrimSpace(sec.Name())
		keys := sec.Keys()
		// ini.v1 总会创建隐式默认段 [DEFAULT]：段前出现的键值都落在这里。
		// 旧解析器要求键必须写在某个 [section] 之内，这里保持报错。
		// 显式写 [DEFAULT] 的文件同样走这条分支（见 docs/contract.md §3.10 的差异说明）。
		if name == ini.DefaultSection {
			if len(keys) > 0 {
				return nil, fmt.Errorf("[%s] %s = %q 出现在任何段之前（缺少 [section] 段头）",
					name, keys[0].Name(), keys[0].Value())
			}
			continue
		}
		s := f.ensureSection(name)
		for _, k := range keys {
			s.set(strings.ToLower(k.Name()), iniValue{Key: k.Name(), Value: k.Value()})
		}
	}
	return f, nil
}

// translateINIError 把 ini.v1 的语法错误转成与旧解析器一致的中文提示。
//
// v0.5.0 起不再有"第 N 行"：错误里回显出错的那一行/段头，便于 grep 定位。
func translateINIError(err error) error {
	if ini.IsErrDelimiterNotFound(err) {
		line := err.Error()
		line = strings.TrimSpace(strings.TrimPrefix(line, "key-value delimiter not found: "))
		return fmt.Errorf("无法解析 %q，键值必须形如 key = value", line)
	}
	if ini.IsErrEmptyKeyName(err) {
		line := err.Error()
		line = strings.TrimSpace(strings.TrimPrefix(line, "empty key name: "))
		return fmt.Errorf("键名为空: %q", line)
	}
	msg := err.Error()
	if rest, ok := strings.CutPrefix(msg, "unclosed section: "); ok {
		return fmt.Errorf("段头 %q 缺少结尾的 ']'", strings.TrimSpace(rest))
	}
	if msg == "empty section name" {
		return fmt.Errorf("段名为空（[] 内不能没有段名）")
	}
	return err
}

// normalizeSectionHeaders 裁剪段头方括号内的空白，使 ini.v1 的段名与旧解析器一致。
//
// ini.v1 不裁剪 "[ Relay ]" 里的空白；不归一化会让 "[ Relay ]" 与 "[relay]"
// 变成两个不同的段，写回时还会在文件末尾重复追加段头。这里只按行改写段头，
// 不解析键值，也不改变行数、换行风格与 BOM。
func normalizeSectionHeaders(data []byte) []byte {
	if !bytes.Contains(data, []byte("[")) {
		return data
	}
	lines := strings.SplitAfter(string(data), "\n")
	changed := false
	for i, raw := range lines {
		line := strings.TrimSuffix(raw, "\n")
		line = strings.TrimSuffix(line, "\r")
		body := strings.TrimLeft(line, " \t\ufeff")
		if !strings.HasPrefix(body, "[") {
			continue
		}
		closeIdx := strings.LastIndexByte(body, ']')
		if closeIdx < 0 {
			continue
		}
		rawName := body[1:closeIdx]
		name := strings.TrimSpace(rawName)
		if name == "" || name == rawName {
			continue
		}
		prefix := line[:len(line)-len(body)]
		lines[i] = prefix + "[" + name + "]" + body[closeIdx+1:] + raw[len(line):]
		changed = true
	}
	if !changed {
		return data
	}
	return []byte(strings.Join(lines, ""))
}
