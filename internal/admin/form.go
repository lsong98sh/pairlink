package admin

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// OnlyPostFields 校验表单字段是否只包含 allowed 中的键（csrf_token 总是允许）。
//
// 阶段 7 的写 Action 必须在读取字段前调用：任何未在白名单中的字段名都视为
// 非法请求（返回 false，调用方应回 400），避免把意外字段名带进 ini 或日志。
func OnlyPostFields(r *http.Request, allowed ...string) bool {
	if r == nil {
		return false
	}
	if err := r.ParseForm(); err != nil {
		return false
	}
	ok := make(map[string]bool, len(allowed)+1)
	ok["csrf_token"] = true
	for _, name := range allowed {
		ok[name] = true
	}
	for name := range r.PostForm {
		if !ok[name] {
			return false
		}
	}
	return true
}

// ParsePositiveDuration 解析一个必须 > 0 的 duration 字符串。
//
// 长度上限 64 字符（超长直接拒绝），只接受 Go duration 语法（如 10s / 5m）。
func ParsePositiveDuration(raw string) (time.Duration, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, fmt.Errorf("duration 不能为空")
	}
	if len(s) > 64 {
		return 0, fmt.Errorf("duration 过长（上限 64 字节）")
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("不是合法的 duration（例如 10s / 300s / 5m）: %w", err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("duration 必须大于 0")
	}
	return d, nil
}
