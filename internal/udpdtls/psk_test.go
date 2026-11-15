package udpdtls

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"testing"

	"golang.org/x/crypto/hkdf"

	"github.com/lsong98sh/pairlink/internal/pairing"
)

// TestDeriveVector 固定一组输入/输出，防止有人"顺手"改了 info 标签或长度
// 导致三端在线上握不上手（该向量同时是三端一致的契约）。
func TestDeriveVector(t *testing.T) {
	key, err := Derive("token-abc", "PK-ABC123")
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if len(key) != KeyLen {
		t.Fatalf("长度 = %d，期望 %d", len(key), KeyLen)
	}
	// 与 Derive 内部实现无关的独立计算，避免"用实现测实现"。
	r := hkdf.New(sha256.New, []byte("token-abc"), nil, []byte("pairlink-udp-dtls/v1|PK-ABC123"))
	want := make([]byte, KeyLen)
	if _, err := io.ReadFull(r, want); err != nil {
		t.Fatalf("参考 HKDF: %v", err)
	}
	if !bytes.Equal(key, want) {
		t.Fatalf("派生结果不匹配：%s != %s", hex.EncodeToString(key), hex.EncodeToString(want))
	}
}

// TestDeriveDistinctByInput 确认 auth_token 与 pairing_key 都参与派生：
// 任一变化都必须改变结果（否则会话之间/集群之间会共用同一 PSK）。
func TestDeriveDistinctByInput(t *testing.T) {
	a, err := Derive("token-1", "PK-A")
	if err != nil {
		t.Fatalf("Derive A: %v", err)
	}
	b, err := Derive("token-2", "PK-A")
	if err != nil {
		t.Fatalf("Derive B: %v", err)
	}
	c, err := Derive("token-1", "PK-B")
	if err != nil {
		t.Fatalf("Derive C: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("不同 auth_token 派生出相同 PSK")
	}
	if bytes.Equal(a, c) {
		t.Fatal("不同 pairing_key 派生出相同 PSK")
	}
}

// TestDeriveStable 确认同一输入可重复得到同一结果（无随机盐）。
func TestDeriveStable(t *testing.T) {
	first, err := Derive("token-x", "PK-1")
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	second, err := Derive("token-x", "PK-1")
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("同一输入两次派生结果不一致")
	}
}

// TestDeriveAcceptsCanonicalPairingKey 确认配对键的规范字符串可直接作为
// PSK identity 使用（relay 侧会对 identity 调 ParsePairingKey 校验）。
func TestDeriveAcceptsCanonicalPairingKey(t *testing.T) {
	raw := "web/c-0000000000000001"
	parsed, err := pairing.ParsePairingKey(raw)
	if err != nil {
		t.Fatalf("ParsePairingKey(%q): %v", raw, err)
	}
	psk, err := Derive("token", parsed.String())
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if len(psk) != KeyLen {
		t.Fatalf("长度 = %d，期望 %d", len(psk), KeyLen)
	}
}
