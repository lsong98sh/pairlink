package protocol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"runtime"
	"testing"
	"time"
)

// writeHeaderOnly 只写 5 字节帧头（type + 声明的 payload 长度），不发 payload。
//
// 这是 P0-1/SEC-D1 的攻击形态：对端声明 1 MiB，等待本端按声明值预分配内存。
func writeHeaderOnly(t *testing.T, conn net.Conn, typ MessageType, declared uint32) {
	t.Helper()
	var hdr [HeaderSize]byte
	hdr[0] = byte(typ)
	binary.BigEndian.PutUint32(hdr[1:], declared)
	if _, err := conn.Write(hdr[:]); err != nil {
		t.Fatalf("写帧头失败: %v", err)
	}
}

// TestReadFrameWithLimitRejectsOversizedHeader 是 P0-1 的回归测试：
// 对端只发 5 字节帧头声明 1 MiB，ReadFrameWithLimit(64 KiB) 必须立即返回
// ErrPayloadTooLarge（不读 payload），且整个调用新增分配 < 256 KiB。
func TestReadFrameWithLimitRejectsOversizedHeader(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer ln.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		writeHeaderOnly(t, conn, MsgBind, MaxPayload)
		// 故意不关闭连接、不发 payload：模拟慢速/半帧攻击，直到测试结束。
		<-time.After(5 * time.Second)
		_ = conn.Close()
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	defer conn.Close()

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	typ, payload, err := ReadFrameWithLimit(conn, NetworkPayloadLimit)
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)

	if !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("声明 %d 字节应返回 ErrPayloadTooLarge，实际 typ=%v payload=%d err=%v",
			MaxPayload, typ, len(payload), err)
	}
	if payload != nil {
		t.Fatalf("拒绝时不应返回 payload，实际 %d 字节", len(payload))
	}
	if elapsed > 2*time.Second {
		t.Fatalf("应在读到帧头后立即返回，实际耗时 %v", elapsed)
	}
	delta := int64(after.TotalAlloc - before.TotalAlloc)
	if delta >= 256<<10 {
		t.Fatalf("单次请求新增分配 %d 字节，应 < 256 KiB（不能按声明长度预分配）", delta)
	}
	t.Logf("拒绝 1 MiB 声明帧：耗时 %v，新增分配 %d 字节（上限 256 KiB）", elapsed, delta)
}

// TestParseFrameLimit 覆盖 P0-1 的 UDP/内存入口：声明长度必须等于实际长度。
func TestParseFrameLimit(t *testing.T) {
	frame, err := EncodeFrame(MsgBind, &Bind{PairingKey: "web/c-1", PairingToken: "abc"})
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}

	typ, payload, err := ParseFrameLimit(frame, NetworkPayloadLimit)
	if err != nil || typ != MsgBind || !bytes.Equal(payload, frame[HeaderSize:]) {
		t.Fatalf("完整帧应解析成功: typ=%v payload=%d err=%v", typ, len(payload), err)
	}
	// 返回的是原切片的子切片（零拷贝，不额外分配）。
	if len(payload) > 0 && &payload[0] != &frame[HeaderSize] {
		t.Error("ParseFrameLimit 应返回原数据的子切片（不做拷贝/分配）")
	}

	// 声明长度 > 实际数据：长度不一致（不是"读到一半"）。
	short := append([]byte(nil), frame...)
	short = short[:len(short)-1]
	if _, _, err := ParseFrameLimit(short, NetworkPayloadLimit); !errors.Is(err, ErrFrameLengthMismatch) {
		t.Fatalf("声明长度大于实际数据应返回 ErrFrameLengthMismatch，实际 %v", err)
	}

	// 声明长度 < 实际数据：同样不一致。
	long := append(append([]byte(nil), frame...), 0x00)
	if _, _, err := ParseFrameLimit(long, NetworkPayloadLimit); !errors.Is(err, ErrFrameLengthMismatch) {
		t.Fatalf("实际数据多于声明长度应返回 ErrFrameLengthMismatch，实际 %v", err)
	}

	// 声明长度超限：ErrPayloadTooLarge，且不分配 declared 大小的内存。
	huge := make([]byte, HeaderSize+8)
	huge[0] = byte(MsgBind)
	binary.BigEndian.PutUint32(huge[1:], MaxPayload)
	if _, _, err := ParseFrameLimit(huge, NetworkPayloadLimit); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("声明 1 MiB 应返回 ErrPayloadTooLarge，实际 %v", err)
	}

	// 不足一个帧头：拒绝（ErrFrameLengthMismatch）。
	if _, _, err := ParseFrameLimit(frame[:HeaderSize-1], NetworkPayloadLimit); !errors.Is(err, ErrFrameLengthMismatch) {
		t.Fatalf("不足 5 字节应返回 ErrFrameLengthMismatch，实际 %v", err)
	}

	// 默认 ParseFrame 等价于 MaxPayload 上限，行为不变。
	if _, _, err := ParseFrame(frame); err != nil {
		t.Fatalf("ParseFrame 对完整帧应成功: %v", err)
	}
}

// TestReadUDPTunnelFrameRejectsLongSession 覆盖 P2-6：读侧也必须限制会话标识长度。
func TestReadUDPTunnelFrameRejectsLongSession(t *testing.T) {
	var buf bytes.Buffer
	session := bytes.Repeat([]byte("s"), MaxSessionIDLen+1)
	payload := []byte("data")
	n := 1 + len(session) + len(payload)
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(n))
	buf.Write(prefix[:])
	buf.WriteByte(byte(len(session)))
	buf.Write(session)
	buf.Write(payload)

	if _, _, err := ReadUDPTunnelFrame(&buf); !errors.Is(err, ErrBadUDPTunnelFrame) {
		t.Fatalf("会话标识 %d 字节应被拒绝，实际 %v", len(session), err)
	}
}

// TestEncodeFrameStillAllowsMaxPayload 断言"内存上限"没有被误降级：
// EncodeFrame/ParseFrame 仍按 MaxPayload(1 MiB) 工作（网络入口才用 64 KiB）。
func TestEncodeFrameStillAllowsMaxPayload(t *testing.T) {
	if NetworkPayloadLimit >= MaxPayload {
		t.Fatalf("NetworkPayloadLimit(%d) 必须小于 MaxPayload(%d)", NetworkPayloadLimit, MaxPayload)
	}
	// 构造一个接近 1 MiB 的合法帧（payload 内容无需是合法 JSON：
	// 本用例只验证 帧头/长度 的上限语义）。
	n := MaxPayload - 128
	frame := make([]byte, HeaderSize+n)
	frame[0] = byte(MsgBind)
	binary.BigEndian.PutUint32(frame[1:], uint32(n))

	typ, raw, err := ParseFrame(frame)
	if err != nil || typ != MsgBind || len(raw) != n {
		t.Fatalf("ParseFrame 应接受接近 1 MiB 的负载: typ=%v len=%d err=%v", typ, len(raw), err)
	}
	// 但网络入口只放行 64 KiB。
	if _, _, err := ParseFrameLimit(frame, NetworkPayloadLimit); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("网络入口应拒绝 1 MiB 声明: %v", err)
	}
}
