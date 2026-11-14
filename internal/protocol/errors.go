package protocol

// 错误码常量（MsgError.Code）。
//
// 错误码是线上协议的一部分，发布后不得改名；新增只能追加。
const (
	// ErrCodeAuthFailed 表示接入密钥错误（Hello.Token 与 [relay] auth_token 不符）。
	ErrCodeAuthFailed = "auth_failed"
	// ErrCodeServiceNotFound 表示服务ID 未注册。
	ErrCodeServiceNotFound = "service_not_found"
	// ErrCodeNoAgentOnline 表示服务存在但所属 agent 当前不在线。
	ErrCodeNoAgentOnline = "no_agent_online"
	// ErrCodePairTimeout 表示配对超时（只到齐了一侧或等待对端报到超时）。
	ErrCodePairTimeout = "pair_timeout"
	// ErrCodeInvalidToken 表示配对令牌错误、过期、已消费或与配对键不匹配。
	ErrCodeInvalidToken = "invalid_token"
	// ErrCodeAlreadyPaired 表示该配对键已完成配对，重复报到被拒绝。
	ErrCodeAlreadyPaired = "already_paired"
	// ErrCodeTooManyPending 表示待配对表已满，relay 拒绝新的接入请求。
	ErrCodeTooManyPending = "too_many_pending"
	// ErrCodeInternal 表示 relay 内部错误。
	ErrCodeInternal = "internal"
	// ErrCodeUnsupportedVersion 表示协议版本不匹配。
	ErrCodeUnsupportedVersion = "unsupported_version"
	// ErrCodeBadRequest 表示报文结构非法（字段缺失、角色非预期等）。
	ErrCodeBadRequest = "bad_request"
)
