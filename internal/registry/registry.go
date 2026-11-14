// Package registry 维护 relay 的连接注册表：服务注册表、agent 注册表与 UDP 会话表。
//
// 三个结构各自用一把锁保护，互不嵌套加锁：
//   - ServiceRegistry 用 sync.RWMutex，读多写少；
//   - AgentRegistry 用 sync.RWMutex；
//   - UDPTable 用 sync.Mutex（需要同时维护 key → 会话与 endpoint → key 两个索引）。
//
// 所有 Get/List/Snapshot 返回副本，调用方不得依赖内部指针；订阅回调在锁外执行。
package registry

import (
	"errors"
	"net"
	"time"
)

// 注册表相关哨兵错误，可用 errors.Is 判别。
var (
	// ErrServiceNotFound 表示服务未注册（或不属于指定 agent）。
	ErrServiceNotFound = errors.New("registry: 服务未注册")
	// ErrServiceOwnerMismatch 表示服务存在但所属 agent 不匹配。
	ErrServiceOwnerMismatch = errors.New("registry: 服务所属 agent 不匹配")
	// ErrAgentExists 表示 agent ID 已存在。
	ErrAgentExists = errors.New("registry: agent 已存在")
	// ErrAgentNotFound 表示 agent 不存在。
	ErrAgentNotFound = errors.New("registry: agent 不存在")
	// ErrUDPEndpointInvalid 表示 UDP 端点非法（IP 或端口为空）。
	ErrUDPEndpointInvalid = errors.New("registry: UDP 端点非法")
	// ErrUDPEndpointConflict 表示 UDP 端点已被其它配对键占用，或同一侧换地址。
	ErrUDPEndpointConflict = errors.New("registry: UDP 端点冲突")
)

// Conn 是与 net.Conn 解耦的连接接口，便于测试伪造。
type Conn interface {
	// Close 关闭连接。
	Close() error
	// RemoteAddr 返回对端地址。
	RemoteAddr() net.Addr
}

// Agent 是一个在线 agent 的注册信息。
type Agent struct {
	// ID 是 relay 分配的 agent 会话 ID。
	ID string
	// RemoteAddr 是控制连接的对端地址（字符串形式，便于展示）。
	RemoteAddr string
	// ConnectedAt 是控制连接建立时间。
	ConnectedAt time.Time
	// LastSeen 是最后一次收到控制面消息的时间。
	LastSeen time.Time
	// Services 是该 agent 当前注册的服务ID 列表。
	Services []string
	// Control 是控制连接，用于踢出等操作；可为 nil（测试场景）。
	Control Conn
}
