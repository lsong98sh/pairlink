package protocol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

// sampleMessages 为每种消息提供一个非零样本，用于编解码往返测试。
func sampleMessages() map[MessageType]any {
	return map[MessageType]any{
		MsgHello:             &Hello{Role: RoleAgent, Token: "t0k", Version: ProtocolVersion, ClientName: "win-box"},
		MsgHelloOK:           &HelloOK{AgentID: "a1", Version: ProtocolVersion, HeartbeatSeconds: 30, UDPMode: UDPModeNative, TCPDataPort: 7001, UDPDataPort: 7002},
		MsgRegisterService:   &RegisterService{ServiceID: "web", Proto: "tcp", LocalHint: "127.0.0.1:8080"},
		MsgServiceRegistered: &ServiceRegistered{ServiceID: "web", Proto: "tcp", UDPMode: UDPModeTCPTunnel},
		MsgListServices:      &ListServices{},
		MsgServiceList: &ServiceList{Services: []ServiceInfo{
			{ServiceID: "web", Proto: "tcp", AgentID: "a1", Online: true},
			{ServiceID: "game", Proto: "udp", AgentID: "a2", Online: false},
		}, Revision: 7, UDPMode: UDPModeTCPTunnel},
		MsgSubscribe:      &Subscribe{Enable: true},
		MsgServiceEvent:   &ServiceEvent{Event: EventAdded, Service: ServiceInfo{ServiceID: "web", Proto: "tcp", AgentID: "a1", Online: true}, Revision: 8},
		MsgConnectRequest: &ConnectRequest{ServiceID: "web", ClientConnID: "c-1"},
		MsgPairResult:     &PairResult{PairingKey: "web/c-1", PairingToken: "abc", Proto: "tcp", Role: RoleClient},
		MsgIncoming:       &Incoming{PairingKey: "web/c-1", PairingToken: "abc", Proto: "udp", Role: RoleAgent, ClientAddr: "1.2.3.4:5678"},
		MsgBind:           &Bind{PairingKey: "web/c-1", PairingToken: "abc", Role: RoleClient, Carrier: CarrierUDPOverTCP},
		MsgBindOK:         &BindOK{PairingKey: "web/c-1"},
		MsgHeartbeat:      &Heartbeat{},
		MsgHeartbeatAck:   &HeartbeatAck{},
		MsgError:          &Error{Code: ErrCodeServiceNotFound, Message: "没有该服务", Detail: "web", ClientConnID: "0123456789abcdef"},
		MsgKicked:         &Kicked{Reason: "管理页踢出"},
		MsgServerShutdown: &ServerShutdown{Reason: "重启"},
	}
}

func TestFrameRoundTrip(t *testing.T) {
	for typ, want := range sampleMessages() {
		t.Run(typ.String(), func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteFrame(&buf, typ, want); err != nil {
				t.Fatalf("WriteFrame 失败: %v", err)
			}
			if got, wantLen := buf.Len(), HeaderSize+len(buf.Bytes())-HeaderSize; got < HeaderSize || wantLen < 0 {
				t.Fatalf("帧长度异常: %d", got)
			}
			gotType, payload, err := ReadFrame(&buf)
			if err != nil {
				t.Fatalf("ReadFrame 失败: %v", err)
			}
			if gotType != typ {
				t.Fatalf("类型 = %v, want %v", gotType, typ)
			}
			got, err := Decode(gotType, payload)
			if err != nil {
				t.Fatalf("Decode 失败: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("消息不一致\n got: %#v\nwant: %#v", got, want)
			}
			if buf.Len() != 0 {
				t.Fatalf("读取后仍有剩余字节: %d", buf.Len())
			}
		})
	}
}

func TestFrameHeaderLayout(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, MsgHello, &Hello{Role: RoleClient, Version: ProtocolVersion}); err != nil {
		t.Fatalf("WriteFrame 失败: %v", err)
	}
	raw := buf.Bytes()
	if MessageType(raw[0]) != MsgHello {
		t.Fatalf("首字节 = 0x%02x, want 0x%02x", raw[0], byte(MsgHello))
	}
	n := binary.BigEndian.Uint32(raw[1:5])
	if int(n) != len(raw)-HeaderSize {
		t.Fatalf("长度字段 = %d, 实际负载 = %d", n, len(raw)-HeaderSize)
	}
	if !strings.HasPrefix(string(raw[5:]), `{"role":"client"`) {
		t.Fatalf("负载不是 JSON: %q", raw[5:])
	}
}

func TestNilPayload(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, MsgHeartbeat, nil); err != nil {
		t.Fatalf("WriteFrame(nil) 失败: %v", err)
	}
	if buf.Len() != HeaderSize {
		t.Fatalf("空负载帧长度 = %d, want %d", buf.Len(), HeaderSize)
	}
	typ, payload, err := ReadFrame(&buf)
	if err != nil || typ != MsgHeartbeat || len(payload) != 0 {
		t.Fatalf("读取空负载帧失败: typ=%v payload=%q err=%v", typ, payload, err)
	}
	msg, err := Decode(typ, payload)
	if err != nil {
		t.Fatalf("Decode 空负载失败: %v", err)
	}
	if _, ok := msg.(*Heartbeat); !ok {
		t.Fatalf("Decode 返回类型错误: %T", msg)
	}
}

func TestWriteFrameTooLarge(t *testing.T) {
	big := strings.Repeat("x", MaxPayload+1)
	var buf bytes.Buffer
	err := WriteFrame(&buf, MsgHello, &Hello{ClientName: big})
	if !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("超长写入应返回 ErrPayloadTooLarge，实际: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("超长帧不应写出任何字节，实际 %d", buf.Len())
	}
}

func TestWriteFrameMarshalError(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, MsgHello, make(chan int)); err == nil {
		t.Fatal("不可序列化的负载应报错")
	}
}

func TestReadFrameTooLarge(t *testing.T) {
	header := make([]byte, HeaderSize)
	header[0] = byte(MsgHello)
	binary.BigEndian.PutUint32(header[1:], MaxPayload+1)
	_, _, err := ReadFrame(bytes.NewReader(header))
	if !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("声明超长负载应返回 ErrPayloadTooLarge，实际: %v", err)
	}
}

func TestReadFrameWithLimit(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, MsgHello, &Hello{Role: RoleAgent, ClientName: strings.Repeat("y", 100)}); err != nil {
		t.Fatalf("WriteFrame 失败: %v", err)
	}
	if _, _, err := ReadFrameWithLimit(&buf, 16); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("超过自定义上限应报错，实际: %v", err)
	}

	buf.Reset()
	if err := WriteFrame(&buf, MsgHello, &Hello{Role: RoleAgent}); err != nil {
		t.Fatalf("WriteFrame 失败: %v", err)
	}
	if _, _, err := ReadFrameWithLimit(&buf, 0); err != nil {
		t.Fatalf("limit<=0 应回退到 MaxPayload: %v", err)
	}
}

func TestReadFrameTruncated(t *testing.T) {
	// 只有半个帧头。
	if _, _, err := ReadFrame(bytes.NewReader([]byte{0x01, 0x00})); err == nil {
		t.Fatal("截断帧头应报错")
	}
	// 帧头完整但负载不足。
	var buf bytes.Buffer
	if err := WriteFrame(&buf, MsgHello, &Hello{Role: RoleAgent}); err != nil {
		t.Fatalf("WriteFrame 失败: %v", err)
	}
	raw := buf.Bytes()
	part := raw[:len(raw)-3]
	_, _, err := ReadFrame(bytes.NewReader(part))
	if err == nil {
		t.Fatal("截断负载应报错")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("错误应包装 io.ErrUnexpectedEOF，实际: %v", err)
	}
	// 空 reader。
	if _, _, err := ReadFrame(bytes.NewReader(nil)); err == nil {
		t.Fatal("空 reader 应报错")
	}
}

func TestDecodeInvalidJSON(t *testing.T) {
	if _, err := Decode(MsgHello, []byte("{not json")); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
	if _, err := Decode(MsgHello, nil); err != nil {
		t.Fatalf("空负载应解码为零值消息: %v", err)
	}
}

func TestDecodeUnknownType(t *testing.T) {
	typ := MessageType(0x99)
	if _, _, err := readFrameOfType(typ, []byte(`{}`)); err != nil {
		t.Fatalf("未知类型不应在帧层报错: %v", err)
	}
	if _, err := Decode(typ, []byte(`{}`)); !errors.Is(err, ErrUnknownType) {
		t.Fatalf("Decode 未知类型应返回 ErrUnknownType，实际: %v", err)
	}
	if typ.Valid() {
		t.Error("未知类型 Valid() 应为 false")
	}
	if !strings.HasPrefix(typ.String(), "unknown(") {
		t.Errorf("未知类型 String() = %q", typ.String())
	}
}

// readFrameOfType 手工拼一个指定类型的帧并读取。
func readFrameOfType(typ MessageType, payload []byte) (MessageType, []byte, error) {
	buf := make([]byte, HeaderSize+len(payload))
	buf[0] = byte(typ)
	binary.BigEndian.PutUint32(buf[1:HeaderSize], uint32(len(payload)))
	copy(buf[HeaderSize:], payload)
	return ReadFrame(bytes.NewReader(buf))
}

func TestMessageTypeNames(t *testing.T) {
	all := []MessageType{
		MsgHello, MsgHelloOK, MsgRegisterService, MsgServiceRegistered, MsgListServices,
		MsgServiceList, MsgSubscribe, MsgServiceEvent, MsgConnectRequest, MsgPairResult,
		MsgIncoming, MsgBind, MsgBindOK, MsgHeartbeat, MsgHeartbeatAck, MsgError,
		MsgKicked, MsgServerShutdown,
	}
	seen := map[MessageType]bool{}
	for _, typ := range all {
		if !typ.Valid() {
			t.Errorf("类型 0x%02x 应为已定义", uint8(typ))
		}
		if seen[typ] {
			t.Errorf("类型 0x%02x 重复", uint8(typ))
		}
		seen[typ] = true
		if name := typ.String(); name == "" || strings.HasPrefix(name, "unknown(") {
			t.Errorf("类型 0x%02x 名称错误: %q", uint8(typ), name)
		}
	}
}

func TestCheckVersion(t *testing.T) {
	if err := CheckVersion(ProtocolVersion); err != nil {
		t.Fatalf("当前版本应通过: %v", err)
	}
	err := CheckVersion(ProtocolVersion + 1)
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("版本不匹配应返回 ErrUnsupportedVersion，实际: %v", err)
	}
}

func TestErrorInterface(t *testing.T) {
	var err error = &Error{Code: ErrCodeInvalidToken, Message: "令牌无效"}
	if !strings.Contains(err.Error(), ErrCodeInvalidToken) {
		t.Errorf("Error() = %q", err.Error())
	}
	var nilErr *Error
	if nilErr.Error() == "" {
		t.Error("nil *Error 的 Error() 不应为空")
	}
}

func TestEncodeParseFrame(t *testing.T) {
	raw, err := EncodeFrame(MsgBind, &Bind{PairingKey: "web/c-1", PairingToken: "tok", Role: RoleAgent, Carrier: CarrierUDPOverTCP})
	if err != nil {
		t.Fatalf("EncodeFrame 失败: %v", err)
	}
	typ, payload, err := ParseFrame(raw)
	if err != nil {
		t.Fatalf("ParseFrame 失败: %v", err)
	}
	if typ != MsgBind {
		t.Fatalf("类型 = %v, want %v", typ, MsgBind)
	}
	msg, err := Decode(typ, payload)
	if err != nil {
		t.Fatalf("Decode 失败: %v", err)
	}
	bind, ok := msg.(*Bind)
	if !ok || bind.Carrier != CarrierUDPOverTCP || bind.PairingKey != "web/c-1" {
		t.Fatalf("解析结果错误: %+v", msg)
	}
	// 帧后有多余字节必须报错。
	if _, _, err := ParseFrame(append(raw, 0x00)); err == nil {
		t.Fatal("帧后多余字节应报错")
	}
}

func TestUDPTunnelFrameRoundTrip(t *testing.T) {
	payload := []byte{0x01, 0x02, 0x03, 0xff}
	for _, session := range []string{"web/c-1", ""} {
		var buf bytes.Buffer
		if err := WriteUDPTunnelFrame(&buf, session, payload); err != nil {
			t.Fatalf("WriteUDPTunnelFrame(%q) 失败: %v", session, err)
		}
		gotSession, gotPayload, err := ReadUDPTunnelFrame(&buf)
		if err != nil {
			t.Fatalf("ReadUDPTunnelFrame(%q) 失败: %v", session, err)
		}
		if gotSession != session {
			t.Fatalf("会话标识 = %q, want %q", gotSession, session)
		}
		if !bytes.Equal(gotPayload, payload) {
			t.Fatalf("payload = %v, want %v", gotPayload, payload)
		}
		if buf.Len() != 0 {
			t.Fatalf("读取后仍有剩余 %d 字节", buf.Len())
		}
	}
}

func TestUDPTunnelFrameLimits(t *testing.T) {
	var buf bytes.Buffer
	// 会话标识超长。
	longSession := strings.Repeat("a", MaxSessionIDLen+1)
	if err := WriteUDPTunnelFrame(&buf, longSession, nil); !errors.Is(err, ErrBadUDPTunnelFrame) {
		t.Fatalf("超长会话标识应报错，实际: %v", err)
	}
	// 数据报超长。
	big := make([]byte, MaxUDPDatagram+1)
	if err := WriteUDPTunnelFrame(&buf, "web/c-1", big); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("超长数据报应报错，实际: %v", err)
	}
	// 声明长度超限。
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, MaxUDPTunnelFrameLen+1)
	if _, _, err := ReadUDPTunnelFrame(bytes.NewReader(header)); !errors.Is(err, ErrBadUDPTunnelFrame) {
		t.Fatalf("超限长度应报错，实际: %v", err)
	}
	// 会话长度大于帧长度。
	bad := []byte{0, 0, 0, 2, 5, 'x'}
	if _, _, err := ReadUDPTunnelFrame(bytes.NewReader(bad)); !errors.Is(err, ErrBadUDPTunnelFrame) {
		t.Fatalf("会话长度越界应报错，实际: %v", err)
	}
	// 截断。
	if _, _, err := ReadUDPTunnelFrame(bytes.NewReader([]byte{0, 0})); err == nil {
		t.Fatal("截断长度前缀应报错")
	}
}
