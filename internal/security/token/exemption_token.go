package token

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// TaintExemptionToken HITL 人工介入放行令牌（M04 §3）。
// field_hash = SHA-256(field 内容)；TTL 到期后自动失效。
type TaintExemptionToken struct {
	FieldHash string    // SHA-256(field 原始内容)
	ExpiresAt time.Time // TTL 到期时间
	Issuer    string    // 审批人标识（审计用）
}

// NewTaintExemptionToken 签发放行令牌，TTL 从签发时起计算。
func NewTaintExemptionToken(fieldContent []byte, ttl time.Duration, issuer string) *TaintExemptionToken {
	h := sha256.Sum256(fieldContent)
	return &TaintExemptionToken{
		FieldHash: hex.EncodeToString(h[:]),
		ExpiresAt: time.Now().Add(ttl),
		Issuer:    issuer,
	}
}

// Valid 验证令牌是否对指定字段内容有效且未过期。
func (t *TaintExemptionToken) Valid(fieldContent []byte) bool {
	if t == nil || time.Now().After(t.ExpiresAt) {
		return false
	}
	h := sha256.Sum256(fieldContent)
	return hex.EncodeToString(h[:]) == t.FieldHash
}

// Summary 返回审计摘要（用于 EventLog）。
func (t *TaintExemptionToken) Summary() string {
	return fmt.Sprintf("exemption: hash=%s issuer=%s expires=%s",
		t.FieldHash[:8], t.Issuer, t.ExpiresAt.Format(time.RFC3339))
}
