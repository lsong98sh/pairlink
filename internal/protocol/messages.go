package protocol

// 角色取值（Hello.Role / Bind.Role / PairResult.Role / Incoming.Role）。
const (
	// RoleAgent 表示服务提供者侧。
	RoleAgent = "agent"
	// RoleClient 表示访问侧。
	RoleClient = "client"
)

// UDP 数据面承载模式（udp_mode），由 relay 在 HelloOK / ServiceRegistered /
// ServiceList 中下发，agent/client 据此选择 UDP 数据面接入方式。
const (
	// UDPModeNative 表示 relay 配置了 udp_listen，UDP 数据面直接走原生 UDP 端口。
	UDPModeNative = "native"
	// UDPModeTCPTunnel 表示 relay 未配置 udp_listen，UDP 数据面封装在 TCP 数据面
	// 连接中（UDP over TCP，见 UDP 隧道帧）。
	UDPModeTCPTunnel = "tcp_tunnel"
)

// Bind.Carrier 取值：决定 TCP 数据面连接在报到后进入纯透传还是帧解析模式。
const (
	// CarrierTCP 是默认值：报到后连接进入纯字节透传（TCP 服务）。
	CarrierTCP = "tcp"
	// CarrierUDPOverTCP 表示该连接作为 UDP over TCP 承载连接，relay 切换为
	// UDP 隧道帧解析模式。
	CarrierUDPOverTCP = "udp-over-tcp"
)

// 服务列表变更事件类型（ServiceEvent.Event）。
const (
	// EventAdded 表示服务上线。
	EventAdded = "added"
	// EventRemoved 表示服务下线。
	EventRemoved = "removed"
	// EventUpdated 表示服务信息变更（如 agent 重连后地址变化）。
	EventUpdated = "updated"
)

// Hello 是 agent/client 接入隧道后的第一条消息（0x01）。
type Hello struct {
	// Role 是角色："agent" 或 "client"。
	Role string `json:"role"`
	// Token 是接入密钥，对应 [relay] auth_token。
	Token string `json:"token"`
	// Version 是协议版本，应为 ProtocolVersion。
	Version int `json:"version"`
	// ClientName 是客户端自报名字，仅用于日志与展示，可空。
	ClientName string `json:"client_name,omitempty"`
}

// HelloOK 是 relay 对 Hello 的应答（0x02）。
type HelloOK struct {
	// AgentID 是 relay 分配给 agent 的会话 ID（role=agent 时非空）。
	AgentID string `json:"agent_id,omitempty"`
	// ClientID 是 relay 分配给 client 的会话 ID（role=client 时非空）。
	ClientID string `json:"client_id,omitempty"`
	// Version 是 relay 的协议版本。
	Version int `json:"version"`
	// HeartbeatSeconds 是 relay 建议的心跳间隔（秒）。
	HeartbeatSeconds int `json:"heartbeat_seconds,omitempty"`
	// UDPMode 是 UDP 数据面承载模式："native" 或 "tcp_tunnel"。
	UDPMode string `json:"udp_mode,omitempty"`
	// TCPDataPort 是 relay TCP 数据面（tcp_listen）端口。
	// agent/client 用 relay_addr 的主机名 + 该端口推导数据面拨号地址，
	// 避免各自的 ini 必须重复写 [relay] 段。
	TCPDataPort int `json:"tcp_data_port,omitempty"`
	// UDPDataPort 是 relay 原生 UDP 数据面（udp_listen）端口；
	// 0 表示未启用原生 UDP（此时 udp_mode 应为 tcp_tunnel）。
	UDPDataPort int `json:"udp_data_port,omitempty"`
}

// RegisterService 是 agent 注册服务的请求（0x03）。
type RegisterService struct {
	// ServiceID 是服务ID，1..64 字节，字符集 [a-z0-9-_]。
	ServiceID string `json:"service_id"`
	// Proto 是协议类型："tcp" 或 "udp"。
	Proto string `json:"proto"`
	// LocalHint 是本地目标地址，仅用于管理页与日志展示，不参与转发决策。
	LocalHint string `json:"local_hint,omitempty"`
}

// ServiceRegistered 是注册成功的确认（0x04）。
type ServiceRegistered struct {
	// ServiceID 是注册成功的服务ID。
	ServiceID string `json:"service_id"`
	// Proto 是协议类型。
	Proto string `json:"proto"`
	// UDPMode 是 UDP 数据面承载模式："native" 或 "tcp_tunnel"。
	// agent 据此决定 UDP 服务的数据面接入方式。
	UDPMode string `json:"udp_mode,omitempty"`
}

// ListServices 是查询服务列表的请求（0x05），无字段。
type ListServices struct{}

// ServiceInfo 是服务列表中的一项。
type ServiceInfo struct {
	// ServiceID 是服务ID。
	ServiceID string `json:"service_id"`
	// Proto 是协议类型 tcp / udp。
	Proto string `json:"proto"`
	// AgentID 是提供该服务的 agent 会话 ID。
	AgentID string `json:"agent_id"`
	// Online 表示当前是否可接入。
	Online bool `json:"online"`
}

// ServiceList 是服务列表响应（0x06）。
type ServiceList struct {
	// Services 按 service_id 升序排列。
	Services []ServiceInfo `json:"services"`
	// Revision 是注册表版本号，单调自增，用于订阅推送去重。
	Revision uint64 `json:"revision"`
	// UDPMode 是 UDP 数据面承载模式："native" 或 "tcp_tunnel"。
	// client 据此决定 UDP 服务的数据面接入方式。
	UDPMode string `json:"udp_mode,omitempty"`
}

// Subscribe 是订阅/取消订阅服务列表变更（0x07）。
type Subscribe struct {
	// Enable 为 true 表示订阅，false 表示取消订阅。
	Enable bool `json:"enable"`
}

// ServiceEvent 是服务列表变更推送（0x08）。
type ServiceEvent struct {
	// Event 取值为 "added" / "removed" / "updated"。
	Event string `json:"event"`
	// Service 是发生变更的服务。
	Service ServiceInfo `json:"service"`
	// Revision 是变更后的注册表版本号。
	Revision uint64 `json:"revision"`
}

// ConnectRequest 是 client 请求接入服务（0x09）。
type ConnectRequest struct {
	// ServiceID 是目标服务ID。
	ServiceID string `json:"service_id"`
	// ClientConnID 是 client 为本次本地连接生成的 ID（1..64 字节，字符集 [a-z0-9-_]）。
	ClientConnID string `json:"client_conn_id"`
}

// PairResult 是 relay 下发给 client 的配对信息（0x0A）。
type PairResult struct {
	// PairingKey 是配对键，格式 "serviceID/clientConnID"。
	PairingKey string `json:"pairing_key"`
	// PairingToken 是一次性配对令牌（base64url，无填充）。
	PairingToken string `json:"pairing_token"`
	// Proto 是数据面协议 tcp / udp。
	Proto string `json:"proto"`
	// Role 固定为 "client"。
	Role string `json:"role"`
}

// Incoming 是 relay 下发给 agent 的接入通知（0x0B）。
type Incoming struct {
	// PairingKey 是配对键。
	PairingKey string `json:"pairing_key"`
	// PairingToken 是一次性配对令牌。
	PairingToken string `json:"pairing_token"`
	// Proto 是数据面协议 tcp / udp。
	Proto string `json:"proto"`
	// Role 固定为 "agent"。
	Role string `json:"role"`
	// ClientAddr 是 client 的公网地址，仅用于日志展示，可空。
	ClientAddr string `json:"client_addr,omitempty"`
}

// Bind 是数据面报到帧（0x0C），只在连接建立瞬间发送一次。
//
// TCP：agent/client 与 relay 建立连接后立即发送该帧，之后字节原样透传；
// UDP（原生）：agent/client 向 UDP 数据面端口发送一个携带该帧的数据报完成会话登记；
// UDP over TCP：TCP 数据面连接的首帧携带 Carrier=CarrierUDPOverTCP，relay 校验后
// 把该连接切换为 UDP 隧道帧解析模式（见 WriteUDPTunnelFrame）。
type Bind struct {
	// PairingKey 是配对键。
	PairingKey string `json:"pairing_key"`
	// PairingToken 是一次性配对令牌。
	PairingToken string `json:"pairing_token"`
	// Role 是报到方角色："agent" 或 "client"。
	Role string `json:"role"`
	// Carrier 是数据面承载方式："tcp"（默认，纯透传）或 "udp-over-tcp"
	// （UDP over TCP 承载连接，relay 切换为帧解析）。
	Carrier string `json:"carrier,omitempty"`
}

// BindOK 是 UDP 报到成功的应答（0x0D）；TCP 数据面不回任何字节。
type BindOK struct {
	// PairingKey 是完成报到的配对键。
	PairingKey string `json:"pairing_key"`
}

// Heartbeat 是控制面心跳（0x0E），无字段。
type Heartbeat struct{}

// HeartbeatAck 是心跳应答（0x0F），无字段。
type HeartbeatAck struct{}

// Error 是错误响应（0x10），Code 取 ErrCode* 常量。
type Error struct {
	// Code 是机器可读错误码。
	Code string `json:"code"`
	// Message 是人类可读错误信息。
	Message string `json:"message"`
	// Detail 是可选的补充信息。约定（docs/protocol.md §4.12）：
	//   - UDP over TCP 承载连接上的会话级错误：Detail 是配对键字符串；
	//   - 其它场景不保证稳定，不应被程序解析。
	Detail string `json:"detail,omitempty"`
	// ClientConnID 是可选关联 ID：relay 针对某个 MsgConnectRequest 回错误时
	// 填入该请求的 client_conn_id，client 据此精确关闭对应的本地连接。
	//
	// 这是**新增的可选字段**（向后兼容，不提升 ProtocolVersion）：
	// 旧 relay 不填 → client 回退到"最早等待的本地连接"近似匹配；
	// 旧 client 忽略未知字段 → 行为与之前一致。
	ClientConnID string `json:"client_conn_id,omitempty"`
}

// Error 实现 error 接口，便于在日志中直接使用。
func (e *Error) Error() string {
	if e == nil {
		return "protocol: <nil error>"
	}
	if e.Message == "" {
		return "protocol: " + e.Code
	}
	return "protocol: " + e.Code + ": " + e.Message
}

// Kicked 表示本连接被管理页踢出（0x11）。
type Kicked struct {
	// Reason 是踢出原因，仅用于日志与展示。
	Reason string `json:"reason"`
}

// ServerShutdown 表示 relay 即将停机（0x12）。
type ServerShutdown struct {
	// Reason 是停机原因，仅用于日志与展示。
	Reason string `json:"reason"`
}
