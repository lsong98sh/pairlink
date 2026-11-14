// Package protocol 定义 PairLink 隧道信令的线格式与全部消息类型。
//
// 帧格式（大端）：
//
//	type(1 byte) | payload_len(4 bytes big-endian) | payload(payload_len bytes JSON)
//
// payload_len 上限为 MaxPayload（1 MiB），超过上限直接报错，避免内存放大攻击。
// 数据面连接只在建立瞬间发送一帧 MsgBind，之后字节原样透传，不参与本协议解析。
package protocol

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ProtocolVersion 是当前协议版本，Hello 中携带；版本不匹配必须回
// MsgError{code: unsupported_version} 并关闭连接。
const ProtocolVersion = 1

const (
	// MaxPayload 是单帧 payload 上限（1 MiB）。
	MaxPayload = 1 << 20
	// HeaderSize 是帧头长度：1 字节类型 + 4 字节大端长度。
	HeaderSize = 5
)

// UDP over TCP 隧道帧相关上限。
const (
	// MaxUDPDatagram 是单个 UDP 数据报的最大长度（65535 字节）。
	MaxUDPDatagram = 65535
	// MaxSessionIDLen 是 UDP 隧道帧会话标识的最大长度：
	// "serviceID/clientConnID"，两段各最多 64 字节再加 1 个分隔符。
	MaxSessionIDLen = 129
	// MaxUDPTunnelFrameLen 是 UDP 隧道帧的最大长度（4 字节长度前缀之后的部分）。
	MaxUDPTunnelFrameLen = 1 + MaxSessionIDLen + MaxUDPDatagram
)

// 帧与消息相关错误。
var (
	// ErrPayloadTooLarge 表示帧负载超过允许上限。
	ErrPayloadTooLarge = errors.New("protocol: 帧负载超过上限")
	// ErrUnknownType 表示收到未定义的消息类型。
	ErrUnknownType = errors.New("protocol: 未知消息类型")
	// ErrUnsupportedVersion 表示协议版本不匹配。
	ErrUnsupportedVersion = errors.New("protocol: 不支持的协议版本")
	// ErrBadUDPTunnelFrame 表示 UDP over TCP 隧道帧结构非法。
	ErrBadUDPTunnelFrame = errors.New("protocol: UDP 隧道帧非法")
)

// MessageType 是隧道信令的消息类型（帧首字节）。
type MessageType uint8

// 全部消息类型。数值即线格式中的类型字节，发布后不得变更。
const (
	// MsgHello 是 agent/client 接入隧道后的第一条消息。
	MsgHello MessageType = 0x01
	// MsgHelloOK 是 relay 对 MsgHello 的应答。
	MsgHelloOK MessageType = 0x02
	// MsgRegisterService 是 agent 注册服务的请求。
	MsgRegisterService MessageType = 0x03
	// MsgServiceRegistered 是 relay 对注册请求的确认。
	MsgServiceRegistered MessageType = 0x04
	// MsgListServices 是查询服务列表的请求。
	MsgListServices MessageType = 0x05
	// MsgServiceList 是 relay 返回的服务列表。
	MsgServiceList MessageType = 0x06
	// MsgSubscribe 是订阅/取消订阅服务列表变更。
	MsgSubscribe MessageType = 0x07
	// MsgServiceEvent 是服务列表变更推送。
	MsgServiceEvent MessageType = 0x08
	// MsgConnectRequest 是 client 请求接入某个服务。
	MsgConnectRequest MessageType = 0x09
	// MsgPairResult 是 relay 下发给 client 的配对信息。
	MsgPairResult MessageType = 0x0A
	// MsgIncoming 是 relay 下发给 agent 的接入通知。
	MsgIncoming MessageType = 0x0B
	// MsgBind 是数据面连接建立瞬间的报到帧（TCP/UDP 通用）。
	MsgBind MessageType = 0x0C
	// MsgBindOK 是 UDP 数据面报到成功的应答；TCP 数据面不回任何字节。
	MsgBindOK MessageType = 0x0D
	// MsgHeartbeat 是控制面心跳。
	MsgHeartbeat MessageType = 0x0E
	// MsgHeartbeatAck 是心跳应答。
	MsgHeartbeatAck MessageType = 0x0F
	// MsgError 是错误响应。
	MsgError MessageType = 0x10
	// MsgKicked 表示连接被管理页踢出。
	MsgKicked MessageType = 0x11
	// MsgServerShutdown 表示 relay 即将停机。
	MsgServerShutdown MessageType = 0x12
	// MsgUnregisterService 是 agent 主动注销服务的请求（阶段 7 新增）。
	MsgUnregisterService MessageType = 0x13
	// MsgServiceUnregistered 是 relay 对注销请求的确认（阶段 7 新增）。
	MsgServiceUnregistered MessageType = 0x14
)

// messageNames 是消息类型的可读名（用于日志与错误信息）。
var messageNames = map[MessageType]string{
	MsgHello:               "hello",
	MsgHelloOK:             "hello_ok",
	MsgRegisterService:     "register_service",
	MsgServiceRegistered:   "service_registered",
	MsgListServices:        "list_services",
	MsgServiceList:         "service_list",
	MsgSubscribe:           "subscribe",
	MsgServiceEvent:        "service_event",
	MsgConnectRequest:      "connect_request",
	MsgPairResult:          "pair_result",
	MsgIncoming:            "incoming",
	MsgBind:                "bind",
	MsgBindOK:              "bind_ok",
	MsgHeartbeat:           "heartbeat",
	MsgHeartbeatAck:        "heartbeat_ack",
	MsgError:               "error",
	MsgKicked:              "kicked",
	MsgServerShutdown:      "server_shutdown",
	MsgUnregisterService:   "unregister_service",
	MsgServiceUnregistered: "service_unregistered",
}

// String 返回消息类型的可读名；未知类型返回 unknown(0xNN)。
func (t MessageType) String() string {
	if name, ok := messageNames[t]; ok {
		return name
	}
	return fmt.Sprintf("unknown(0x%02x)", uint8(t))
}

// Valid 报告 t 是否是已定义的消息类型。
func (t MessageType) Valid() bool {
	_, ok := messageNames[t]
	return ok
}

// WriteFrame 把 payload 序列化为 JSON 并写成一帧。
//
// payload 为 nil 时发送空负载；负载超过 MaxPayload 返回 ErrPayloadTooLarge。
// 帧内容通过一次 Write 写出，避免头部与负载被拆开导致对端读到半个帧。
func WriteFrame(w io.Writer, typ MessageType, payload any) error {
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("protocol: 序列化 %s 负载: %w", typ, err)
		}
	}
	if len(body) > MaxPayload {
		return fmt.Errorf("%w: %s 负载 %d 字节 > %d", ErrPayloadTooLarge, typ, len(body), MaxPayload)
	}
	buf := make([]byte, HeaderSize+len(body))
	buf[0] = byte(typ)
	binary.BigEndian.PutUint32(buf[1:HeaderSize], uint32(len(body)))
	copy(buf[HeaderSize:], body)
	if _, err := w.Write(buf); err != nil {
		return fmt.Errorf("protocol: 写帧 %s: %w", typ, err)
	}
	return nil
}

// ReadFrame 读取一帧并返回消息类型与原始 payload。
//
// 未知类型不在此处报错（保留前向兼容），由调用方决定如何处理；
// 负载超限返回 ErrPayloadTooLarge；连接中断/截断返回包装后的 io 错误。
func ReadFrame(r io.Reader) (MessageType, []byte, error) {
	return ReadFrameWithLimit(r, MaxPayload)
}

// ReadFrameWithLimit 与 ReadFrame 相同，但使用调用方指定的负载上限。
// limit <= 0 时回退到 MaxPayload。
func ReadFrameWithLimit(r io.Reader, limit int) (MessageType, []byte, error) {
	if limit <= 0 || limit > MaxPayload {
		limit = MaxPayload
	}
	var header [HeaderSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, fmt.Errorf("protocol: 读取帧头: %w", err)
	}
	typ := MessageType(header[0])
	n := int(binary.BigEndian.Uint32(header[1:HeaderSize]))
	if n < 0 || n > limit {
		return typ, nil, fmt.Errorf("%w: %s 声明负载 %d 字节 > %d", ErrPayloadTooLarge, typ, n, limit)
	}
	if n == 0 {
		return typ, nil, nil
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return typ, nil, fmt.Errorf("protocol: 读取 %s 负载: %w", typ, err)
	}
	return typ, body, nil
}

// Decode 把一帧的 payload 解析为对应的消息结构体指针。
//
// 未知类型返回 ErrUnknownType，JSON 非法返回包装后的解析错误。
// 空负载返回对应结构体的零值（适用于心跳等空消息）。
func Decode(typ MessageType, payload []byte) (any, error) {
	var msg any
	switch typ {
	case MsgHello:
		msg = &Hello{}
	case MsgHelloOK:
		msg = &HelloOK{}
	case MsgRegisterService:
		msg = &RegisterService{}
	case MsgServiceRegistered:
		msg = &ServiceRegistered{}
	case MsgListServices:
		msg = &ListServices{}
	case MsgServiceList:
		msg = &ServiceList{}
	case MsgSubscribe:
		msg = &Subscribe{}
	case MsgServiceEvent:
		msg = &ServiceEvent{}
	case MsgConnectRequest:
		msg = &ConnectRequest{}
	case MsgPairResult:
		msg = &PairResult{}
	case MsgIncoming:
		msg = &Incoming{}
	case MsgBind:
		msg = &Bind{}
	case MsgBindOK:
		msg = &BindOK{}
	case MsgHeartbeat:
		msg = &Heartbeat{}
	case MsgHeartbeatAck:
		msg = &HeartbeatAck{}
	case MsgError:
		msg = &Error{}
	case MsgKicked:
		msg = &Kicked{}
	case MsgServerShutdown:
		msg = &ServerShutdown{}
	case MsgUnregisterService:
		msg = &UnregisterService{}
	case MsgServiceUnregistered:
		msg = &ServiceUnregistered{}
	default:
		return nil, fmt.Errorf("%w: 0x%02x", ErrUnknownType, uint8(typ))
	}
	if len(payload) == 0 {
		return msg, nil
	}
	if err := json.Unmarshal(payload, msg); err != nil {
		return nil, fmt.Errorf("protocol: 解析 %s 负载: %w", typ, err)
	}
	return msg, nil
}

// CheckVersion 校验对端声明的协议版本。
//
// 版本不匹配时返回包装了 ErrUnsupportedVersion 的错误，调用方应回
// MsgError{code: ErrCodeUnsupportedVersion} 后关闭连接。
func CheckVersion(v int) error {
	if v != ProtocolVersion {
		return fmt.Errorf("%w: 收到 %d，期望 %d", ErrUnsupportedVersion, v, ProtocolVersion)
	}
	return nil
}

// EncodeFrame 把一帧编码为字节切片（不含额外的网络写入）。
//
// 用于：把 MsgBind 帧塞进 UDP 数据报或 UDP-over-TCP 隧道帧。
func EncodeFrame(typ MessageType, payload any) ([]byte, error) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, typ, payload); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ParseFrame 从字节切片解析恰好一帧。
//
// 与 ReadFrame 相同，但要求 data 在帧之后没有多余字节（数据报场景）。
func ParseFrame(data []byte) (MessageType, []byte, error) {
	r := bytes.NewReader(data)
	typ, payload, err := ReadFrame(r)
	if err != nil {
		return 0, nil, err
	}
	if r.Len() != 0 {
		return typ, nil, fmt.Errorf("protocol: %s 帧后还有 %d 字节", typ, r.Len())
	}
	return typ, payload, nil
}

// WriteUDPTunnelFrame 写出一个 UDP over TCP 隧道帧。
//
// 帧格式（承载在 Carrier=udp-over-tcp 的 TCP 数据面连接上）：
//
//	len(4B big-endian) | session_len(1B) | session(session_len B) | payload
//
// 其中 len = 1 + session_len + len(payload)，上限 MaxUDPTunnelFrameLen。
// session 为配对键字符串（"serviceID/clientConnID"）；session 为空表示控制帧，
// 此时 payload 是一个完整的 PairLink 帧（如 MsgError），用于在不中断承载连接的前提下
// 报告单个会话的错误。
//
// 帧内容通过单次 Write 写出，避免多 goroutine 写同一连接到帧交错。
func WriteUDPTunnelFrame(w io.Writer, session string, payload []byte) error {
	if len(session) > MaxSessionIDLen {
		return fmt.Errorf("%w: 会话标识 %d 字节 > %d", ErrBadUDPTunnelFrame, len(session), MaxSessionIDLen)
	}
	if len(payload) > MaxUDPDatagram {
		return fmt.Errorf("%w: UDP 数据报 %d 字节 > %d", ErrPayloadTooLarge, len(payload), MaxUDPDatagram)
	}
	n := 1 + len(session) + len(payload)
	if n > MaxUDPTunnelFrameLen {
		return fmt.Errorf("%w: 隧道帧 %d 字节 > %d", ErrPayloadTooLarge, n, MaxUDPTunnelFrameLen)
	}
	buf := make([]byte, 4+n)
	binary.BigEndian.PutUint32(buf[0:4], uint32(n))
	buf[4] = byte(len(session))
	copy(buf[5:], session)
	copy(buf[5+len(session):], payload)
	if _, err := w.Write(buf); err != nil {
		return fmt.Errorf("protocol: 写 UDP 隧道帧: %w", err)
	}
	return nil
}

// ReadUDPTunnelFrame 读取一个 UDP over TCP 隧道帧。
//
// 返回会话标识（空串表示控制帧）与 payload（数据报或完整 PairLink 帧）。
// 长度前缀超限返回 ErrPayloadTooLarge；结构非法返回 ErrBadUDPTunnelFrame。
func ReadUDPTunnelFrame(r io.Reader) (session string, payload []byte, err error) {
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return "", nil, fmt.Errorf("protocol: 读取 UDP 隧道帧长度: %w", err)
	}
	n := int(binary.BigEndian.Uint32(prefix[:]))
	if n < 1 || n > MaxUDPTunnelFrameLen {
		return "", nil, fmt.Errorf("%w: 声明长度 %d 超出 1..%d", ErrBadUDPTunnelFrame, n, MaxUDPTunnelFrameLen)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return "", nil, fmt.Errorf("protocol: 读取 UDP 隧道帧: %w", err)
	}
	sessionLen := int(body[0])
	if sessionLen > n-1 {
		return "", nil, fmt.Errorf("%w: 会话标识长度 %d 超过帧长 %d", ErrBadUDPTunnelFrame, sessionLen, n)
	}
	session = string(body[1 : 1+sessionLen])
	payload = body[1+sessionLen:]
	return session, payload, nil
}
