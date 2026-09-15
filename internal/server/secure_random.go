package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// secureRandHex 仅用于安全敏感标识（OAuth state/sid/session_id）。
// crypto/rand 失败时必须 fail closed，不能降级为时间戳或伪随机源。
func secureRandHex(n int) (string, error) {
	if n <= 0 {
		return "", fmt.Errorf("secure random hex length must be positive")
	}
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return hex.EncodeToString(b)[:n], nil
}
