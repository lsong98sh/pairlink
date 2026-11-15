package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestHelloOKDataTLSWire 覆盖 HelloOK.data_tls 的序列化/反序列化与旧报文兼容。
func TestHelloOKDataTLSWire(t *testing.T) {
	t.Run("true", func(t *testing.T) {
		frame, err := EncodeFrame(MsgHelloOK, &HelloOK{Version: 1, DataTLS: boolPtr(true)})
		if err != nil {
			t.Fatalf("EncodeFrame: %v", err)
		}
		typ, payload, err := ParseFrame(frame)
		if err != nil {
			t.Fatalf("ParseFrame: %v", err)
		}
		if typ != MsgHelloOK {
			t.Fatalf("type = %v", typ)
		}
		decoded, err := Decode(typ, payload)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		ok, isOK := decoded.(*HelloOK)
		if !isOK {
			t.Fatalf("Decode 返回 %T", decoded)
		}
		enabled, advertised := ok.DataTLSEnabled()
		if !advertised || !enabled {
			t.Fatalf("data_tls = %+v，期望已下发且 true", ok.DataTLS)
		}
	})

	t.Run("false_必须显式下发", func(t *testing.T) {
		frame, err := EncodeFrame(MsgHelloOK, &HelloOK{Version: 1, DataTLS: boolPtr(false)})
		if err != nil {
			t.Fatalf("EncodeFrame: %v", err)
		}
		_, payload, err := ParseFrame(frame)
		if err != nil {
			t.Fatalf("ParseFrame: %v", err)
		}
		if !strings.Contains(string(payload), `"data_tls":false`) {
			t.Fatalf("显式 false 必须出现在报文里（否则会被当作旧 relay）：%s", payload)
		}
		decoded, err := Decode(MsgHelloOK, payload)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		ok := decoded.(*HelloOK)
		enabled, advertised := ok.DataTLSEnabled()
		if !advertised || enabled {
			t.Fatalf("data_tls = %+v，期望已下发且 false", ok.DataTLS)
		}
	})

	t.Run("nil_按旧relay处理", func(t *testing.T) {
		frame, err := EncodeFrame(MsgHelloOK, &HelloOK{Version: 1})
		if err != nil {
			t.Fatalf("EncodeFrame: %v", err)
		}
		_, payload, err := ParseFrame(frame)
		if err != nil {
			t.Fatalf("ParseFrame: %v", err)
		}
		if strings.Contains(string(payload), "data_tls") {
			t.Fatalf("nil 不应出现在报文里（omitempty）：%s", payload)
		}
		decoded, err := Decode(MsgHelloOK, payload)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		ok := decoded.(*HelloOK)
		if _, advertised := ok.DataTLSEnabled(); advertised {
			t.Fatal("未下发时 advertised 必须为 false")
		}
	})

	t.Run("旧relay报文", func(t *testing.T) {
		// v0.3.1 relay 的 HelloOK 线格式（没有 data_tls 字段）。
		old := []byte(`{"agent_id":"a-1","version":1,"heartbeat_seconds":30,"udp_mode":"tcp_tunnel","tcp_data_port":7001}`)
		decoded, err := Decode(MsgHelloOK, old)
		if err != nil {
			t.Fatalf("Decode 旧报文: %v", err)
		}
		ok := decoded.(*HelloOK)
		if ok.AgentID != "a-1" || ok.TCPDataPort != 7001 {
			t.Fatalf("旧字段解析异常: %+v", ok)
		}
		enabled, advertised := ok.DataTLSEnabled()
		if advertised || enabled {
			t.Fatalf("旧报文不应被判定为已下发 data_tls: enabled=%v advertised=%v", enabled, advertised)
		}
	})

	t.Run("未知字段被忽略", func(t *testing.T) {
		// 新 relay 给旧 agent 的报文里多出的字段不影响旧端解析。
		var ok HelloOK
		if err := json.Unmarshal([]byte(`{"version":1,"data_tls":true,"future_field":"x"}`), &ok); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if enabled, advertised := ok.DataTLSEnabled(); !enabled || !advertised {
			t.Fatalf("data_tls 解析异常: %+v", ok.DataTLS)
		}
	})
}

// boolPtr 返回 v 的指针（测试辅助）。
func boolPtr(v bool) *bool { return &v }
