package token_test

import (
	"errors"

	"github.com/polarisagi/polaris/internal/security/token"

	"github.com/polarisagi/polaris/internal/security/policy"

	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

func TestTaintExemptionToken_Valid(t *testing.T) {
	data := []byte("secret data")
	tok1 := token.NewTaintExemptionToken(data, 1*time.Second, "admin")

	if !tok1.Valid(data) {
		t.Errorf("Expected token to be valid for correct data")
	}

	if tok1.Valid([]byte("wrong data")) {
		t.Errorf("Expected token to be invalid for incorrect data")
	}

	// Test expiration
	expiredToken := token.NewTaintExemptionToken(data, -1*time.Second, "admin")
	if expiredToken.Valid(data) {
		t.Errorf("Expected token to be invalid after expiration")
	}

	var nilToken *token.TaintExemptionToken
	if nilToken.Valid(data) {
		t.Errorf("Expected nil token to be invalid")
	}
}

func TestGate_CheckEgressWithExemption(t *testing.T) {
	gate := policy.NewGate(nil)
	data := []byte("sensitive")
	tok := token.NewTaintExemptionToken(data, 1*time.Minute, "admin")

	// 1. Taint low -> pass
	if err := gate.CheckEgressWithExemption(data, types.TaintLow, nil); err != nil {
		t.Errorf("Expected nil error for TaintLow, got %v", err)
	}

	// 2. Taint medium without token -> policy.ErrTaintBlockedEgress（透过
	// *TaintEgressBlockedError 的 Unwrap() 链，errors.Is 而非严格等值比较——
	// 2026-07-14 起该方法拦截时返回携带被拦截数据的 *TaintEgressBlockedError，
	// 供 M04 §3 转义路径铸造 TaintExemptionToken 时使用原始字节而非人类可读摘要）。
	err := gate.CheckEgressWithExemption(data, types.TaintMedium, nil)
	if !errors.Is(err, policy.ErrTaintBlockedEgress) {
		t.Errorf("Expected policy.ErrTaintBlockedEgress, got %v", err)
	}
	var blockedErr *policy.TaintEgressBlockedError
	if !errors.As(err, &blockedErr) || string(blockedErr.Data) != string(data) {
		t.Errorf("Expected *TaintEgressBlockedError carrying original data, got %v", err)
	}

	// 3. Taint medium with valid token -> pass
	if err := gate.CheckEgressWithExemption(data, types.TaintMedium, tok); err != nil {
		t.Errorf("Expected nil error for valid token, got %v", err)
	}

	// 4. Taint medium with invalid token -> policy.ErrTaintBlockedEgress
	invalidTok := token.NewTaintExemptionToken([]byte("other data"), 1*time.Minute, "admin")
	if err := gate.CheckEgressWithExemption(data, types.TaintMedium, invalidTok); !errors.Is(err, policy.ErrTaintBlockedEgress) {
		t.Errorf("Expected policy.ErrTaintBlockedEgress, got %v", err)
	}
}
