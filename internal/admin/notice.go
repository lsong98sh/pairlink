package admin

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
)

const (
	// maxNoticeRunes 是提示条文本上限，避免超长消息破坏布局。
	maxNoticeRunes = 300
	// maxAuditValueRunes 是审计日志字段上限，避免把整段表单值写进日志。
	maxAuditValueRunes = 200
)

// notice 是布局顶部提示条的数据模型。
//
// Text 是未经转义的纯文本，由模板（布局或 NoticeBar）自动转义；框架自身
// 不再手工拼接 template.HTML。
type notice struct {
	Show bool
	Err  bool
	Text string
}

// noticeMarkup 是一段提示条的 HTML 片段（模板文本，所有值自动转义）。
const noticeMarkup = `<div class="notice {{if .Err}}err{{else}}ok{{end}}"><strong>{{if .Err}}操作失败：{{else}}操作成功：{{end}}</strong>{{.Text}}</div>`

// noticeTemplate 是预编译的提示条片段。
var noticeTemplate = template.Must(template.New("notice").Parse(noticeMarkup))

// noticeFor 根据 ?err= / ?ok= 装配提示条数据；没有提示时 Show 为 false。
func noticeFor(r *http.Request) notice {
	if r == nil {
		return notice{}
	}
	if msg := r.URL.Query().Get("err"); msg != "" {
		return notice{Show: true, Err: true, Text: truncateRunes(msg, maxNoticeRunes)}
	}
	if msg := r.URL.Query().Get("ok"); msg != "" {
		return notice{Show: true, Text: truncateRunes(msg, maxNoticeRunes)}
	}
	return notice{}
}

// RedirectNotice 以 303 重定向回 path，并附带一次性提示（?ok= / ?err=）。
//
// 写操作成功后调用 key="ok"，失败调用 key="err"；页面由布局自动读取并渲染
// 提示条（见 noticeFor / NoticeBar）。msg 会被 QueryEscape，读取端再由模板
// 自动 HTML 转义。
func RedirectNotice(w http.ResponseWriter, r *http.Request, path string, isErr bool, msg string) {
	key := "ok"
	if isErr {
		key = "err"
	}
	u := url.URL{Path: path, RawQuery: url.Values{key: {truncateRunes(msg, maxNoticeRunes)}}.Encode()}
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

// NoticeBar 根据 ?ok= / ?err= 生成页面顶部的提示条；没有提示时返回空串。
//
// 文本来自查询参数，由模板自动 HTML 转义后再输出。该导出函数保留原签名与
// 行为；布局内部改用 noticeFor + 模板渲染，不再使用 template.HTML。
func NoticeBar(r *http.Request) template.HTML {
	n := noticeFor(r)
	if !n.Show {
		return ""
	}
	var b bytes.Buffer
	if err := noticeTemplate.Execute(&b, n); err != nil {
		return ""
	}
	return template.HTML(b.String())
}

// Audit 记录一次管理页写操作的审计日志。
//
// 字段固定为 event=admin_action、remote、action、target、old、new；
// 调用方不得把令牌/密钥传入 old/new（token 类字段本就不在可改白名单内）。
// 值会被截断并去掉换行，避免日志注入。
func (s *Server) Audit(r *http.Request, action, target, oldValue, newValue string) {
	remote := ""
	if r != nil {
		remote = r.RemoteAddr
	}
	s.log.Info("管理页写操作",
		"event", "admin_action",
		"remote", sanitizeLog(remote),
		"action", sanitizeLog(action),
		"target", sanitizeLog(target),
		"old", sanitizeLog(oldValue),
		"new", sanitizeLog(newValue),
	)
}

// AuditDenied 记录一次被门禁拒绝的写请求。
//
// reason 区分拒绝原因：只读模式（read_only）与 Host/Origin 校验失败
// （host_origin，SEC-M1）。两者都必须留审计痕迹，但日志不能让排障者误判为
// "服务正处于只读模式"。
func (s *Server) AuditDenied(r *http.Request, action, reason string) {
	remote := ""
	if r != nil {
		remote = r.RemoteAddr
	}
	if reason == "" {
		reason = "read_only"
	}
	s.log.Warn("管理页写操作被拒绝",
		"event", "admin_action_denied",
		"remote", sanitizeLog(remote),
		"action", sanitizeLog(action),
		"reason", sanitizeLog(reason),
	)
}

// AuditFailure 记录一次失败的管理页写操作（校验失败/写盘失败/relay 拒绝等）。
func (s *Server) AuditFailure(r *http.Request, action, target string, err error) {
	remote := ""
	if r != nil {
		remote = r.RemoteAddr
	}
	s.log.Warn("管理页写操作失败",
		"event", "admin_action_failed",
		"remote", sanitizeLog(remote),
		"action", sanitizeLog(action),
		"target", sanitizeLog(target),
		"err", sanitizeLog(fmt.Sprint(err)),
	)
}

// truncateRunes 按 rune 截断字符串。
func truncateRunes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}

// sanitizeLog 去掉控制字符并截断，防止日志注入。
func sanitizeLog(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	return truncateRunes(s, maxAuditValueRunes)
}
