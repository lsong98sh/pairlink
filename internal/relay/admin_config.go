package relay

import (
	"fmt"
	"net/http"
	"time"

	"github.com/lsong98sh/pairlink/internal/admin"
	"github.com/lsong98sh/pairlink/internal/config"
)

// relayParam 描述一个可在管理页修改的 relay 运行期参数。
type relayParam struct {
	// Key 是 ini 键名（也是表单的 key 字段）。
	Key string
	// Title 是页面上的中文名。
	Title string
	// Get 返回当前运行期值。
	Get func(s *server) time.Duration
	// Set 更新运行期值；入参已通过 >0 校验。
	Set func(s *server, d time.Duration) error
}

// relayParams 是 relay 可写参数的白名单（顺序即页面展示顺序）。
//
// 其它字段（relay_addr / token / 监听端口 / tls / 证书等）一律不允许页面修改；
// services / agent / 配对 / UDP 会话只能"踢出"，不能编辑（服务由 agent 注册）。
var relayParams = []relayParam{
	{
		Key:   "pair_timeout",
		Title: "配对超时",
		Get:   func(s *server) time.Duration { return s.pairTimeout() },
		Set:   func(s *server, d time.Duration) error { return s.pairs.SetTimeout(d) },
	},
	{
		Key:   "idle_timeout",
		Title: "数据面空闲超时",
		Get:   func(s *server) time.Duration { return s.idleTimeoutValue() },
		Set: func(s *server, d time.Duration) error {
			s.setRuntimeTimeouts(d, s.heartbeatTimeoutValue(), s.udpSessionTimeoutValue())
			return nil
		},
	},
	{
		Key:   "heartbeat_timeout",
		Title: "心跳判死超时",
		Get:   func(s *server) time.Duration { return s.heartbeatTimeoutValue() },
		Set: func(s *server, d time.Duration) error {
			s.setRuntimeTimeouts(s.idleTimeoutValue(), d, s.udpSessionTimeoutValue())
			return nil
		},
	},
	{
		Key:   "udp_session_timeout",
		Title: "UDP 会话空闲超时",
		Get:   func(s *server) time.Duration { return s.udpSessionTimeoutValue() },
		Set: func(s *server, d time.Duration) error {
			s.setRuntimeTimeouts(s.idleTimeoutValue(), s.heartbeatTimeoutValue(), d)
			return nil
		},
	},
	{
		Key:   "token_ttl",
		Title: "配对令牌有效期",
		Get:   func(s *server) time.Duration { return s.tokenTTL() },
		Set:   func(s *server, d time.Duration) error { return s.tokens.SetTTL(d) },
	},
}

// relayParamByKey 按白名单查找参数。
func relayParamByKey(key string) (relayParam, bool) {
	for _, p := range relayParams {
		if p.Key == key {
			return p, true
		}
	}
	return relayParam{}, false
}

// relayConfigParam 是“配置”表格一行的渲染数据。
//
// 全部字段都是普通 string（外部可控值也只以 string 传入），由 html/template
// 在输出时自动转义；这里不携带任何 template.HTML。
type relayConfigParam struct {
	// Key 是 ini 键名，同时是表单的 key 字段。
	Key string
	// Title 是页面上的中文名。
	Title string
	// Current 是当前运行期值（也是修改表单的默认值）。
	Current string
	// Token 是一次性 CSRF 令牌；只读模式下为空（不渲染表单）。
	Token string
}

// relayConfigData 是“配置（可写回 ini）”区块的渲染数据。
type relayConfigData struct {
	// Writable 为 true 时渲染五个可写表单，否则只显示只读占位。
	Writable bool
	// Params 是五个白名单参数，顺序即页面展示顺序。
	Params []relayConfigParam
}

// relayConfigMarkup 是配置区块的主体模板（define 片段，由 relay 页面模板引用）。
//
// 表单的 method/action、字段名（key/value/csrf_token）与默认值都与旧输出逐字
// 一致，避免破坏既有 e2e 的表单解析。
const relayConfigMarkup = `{{define "relay-config"}}<section><h2>配置（可写回 ini）</h2>
{{- if not .Writable}}
<p class="dim">只读模式下配置区不可编辑；请将 manage_listen 绑定到 127.0.0.1 后重启。</p>
{{- end}}
<table><thead><tr><th>参数</th><th>当前值</th><th>修改</th></tr></thead><tbody>
{{- range .Params}}
<tr><td class="mono">{{.Key}}（{{.Title}}）</td><td class="mono">{{.Current}}</td><td>{{if $.Writable}}<form class="form-inline" method="post" action="/action/relay-config"><input type="hidden" name="csrf_token" value="{{.Token}}"><input type="hidden" name="key" value="{{.Key}}"><input type="text" name="value" value="{{.Current}}" size="12"><button type="submit">修改</button></form>{{else}}<span class="dim">只读</span>{{end}}</td></tr>
{{- end}}
</tbody></table>
<p class="dim">写入顺序：校验 → 写回 ini → 更新运行时；写 ini 失败时运行时不改变。其它字段请编辑 ini 后重启。</p>
</section>{{end}}`

// relayConfigView 从运行期状态装配配置区块数据。
//
// 只在可写模式下生成 CSRF 令牌：非回环绑定既不渲染表单，也不下发令牌。
func (s *server) relayConfigView() relayConfigData {
	writable := s.adminWritable()
	data := relayConfigData{Writable: writable, Params: make([]relayConfigParam, 0, len(relayParams))}
	for _, p := range relayParams {
		row := relayConfigParam{Key: p.Key, Title: p.Title, Current: p.Get(s).String()}
		if writable {
			row.Token = s.admin.CSRFToken()
		}
		data.Params = append(data.Params, row)
	}
	return data
}

// handleConfigAction 是管理页的 relay 参数修改 Action（POST + CSRF + 回环门禁）。
func (s *server) handleConfigAction(w http.ResponseWriter, r *http.Request) {
	if !admin.OnlyPostFields(r, "key", "value") {
		http.Error(w, "400 表单字段非法", http.StatusBadRequest)
		return
	}
	key := r.PostFormValue("key")
	param, ok := relayParamByKey(key)
	if !ok {
		http.Error(w, "400 配置项不在可改白名单内", http.StatusBadRequest)
		return
	}
	value, err := admin.ParsePositiveDuration(r.PostFormValue("value"))
	if err != nil {
		admin.RedirectNotice(w, r, "/relay", true, fmt.Sprintf("%s: %v", key, err))
		return
	}

	old := param.Get(s).String()
	newText := value.String()

	// 顺序（docs/contract.md §3.10）：校验通过后先写 ini，成功才改运行时。
	// 这样写盘失败时运行期配置不会被"只改了一半"。
	// 写回会用 ini 库重写该 ini（注释/排版可能归一），原子替换且权限不变。
	if err := config.PatchValue(s.cfg.Path, "relay", key, newText); err != nil {
		admin.RedirectNotice(w, r, "/relay", true,
			fmt.Sprintf("%s 写回 ini 失败（运行时未改变）: %v", key, err))
		return
	}
	// PARSE 已保证 value > 0，param.Set 的唯一错误条件是 <= 0，因此本分支
	// 实际不可达（P3-10）；保留它是为了防御未来 Set 语义变化导致"ini 已写、
	// 运行时没改"的部分成功状态，不要删掉。
	if err := param.Set(s, value); err != nil {
		admin.RedirectNotice(w, r, "/relay", true,
			fmt.Sprintf("%s 已写回 ini，但更新运行时失败: %v", key, err))
		return
	}

	s.admin.Audit(r, "relay-config", "[relay] "+key, old, newText)
	admin.RedirectNotice(w, r, "/relay", false, fmt.Sprintf("[relay] %s 已更新为 %s", key, newText))
}
