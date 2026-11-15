// Package udpdtls 是原生 UDP 数据面 DTLS-PSK 的共享参数与密钥派生。
//
// 三端（relay / agent / client）必须使用**完全相同**的参数，否则握手必然失败：
//
//	PSK     = HKDF-SHA256(ikm = auth_token, salt = nil,
//	                      info = InfoPrefix + pairing_key, L = KeyLen)
//	identity = pairing_key（由 client 通过 PSKIdentityHint 发送）
//
// 把参数集中在这里而不是各端各写一份，是为了让"三端一致"成为编译期事实：
// 任何一端改了 info 标签或长度，另外两端会跟着变（或共享包编译失败），
// 而不是等到 e2e 握手失败才发现。
package udpdtls

import (
	"crypto/sha256"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

const (
	// InfoPrefix 是 HKDF info 的域隔离前缀（v1）；后接 pairing_key。
	InfoPrefix = "pairlink-udp-dtls/v1|"
	// KeyLen 是派生出的 PSK 长度。
	//
	// AES-128-GCM 实际只用前 16 字节，但仍按 32 字节派生，给将来换
	// AES-256 留余地（换套件时三端必须一起改）。
	KeyLen = 32
)

// Derive 按 HKDF-SHA256 派生一个配对会话的 DTLS PSK。
//
// authToken 是 [relay] auth_token / [server] token / [client] token；
// pairingKey 是配对键的规范字符串，同时用作 PSK identity。
// 相同入参在任一平台、任一端都必须得到相同 32 字节。
func Derive(authToken, pairingKey string) ([]byte, error) {
	r := hkdf.New(sha256.New, []byte(authToken), nil, []byte(InfoPrefix+pairingKey))
	key := make([]byte, KeyLen)
	if _, err := io.ReadFull(r, key); err != nil {
		return nil, fmt.Errorf("udpdtls: 派生 PSK（配对键 %s）: %w", pairingKey, err)
	}
	return key, nil
}
