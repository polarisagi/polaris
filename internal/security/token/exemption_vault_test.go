package token_test

import (
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/security/token"
)

func TestExemptionVault_StoreLookup(t *testing.T) {
	v := token.NewExemptionVault()
	data := []byte("payload")
	tok := token.NewTaintExemptionToken(data, time.Minute, "admin")

	v.Store("agent-1", tok)
	got := v.Lookup("agent-1")
	if got == nil || !got.Valid(data) {
		t.Fatal("expected stored token to be retrievable and valid")
	}

	if v.Lookup("agent-2") != nil {
		t.Error("unrelated agentID should return nil")
	}
}

func TestExemptionVault_Lookup_ExpiredRemoved(t *testing.T) {
	v := token.NewExemptionVault()
	data := []byte("payload")
	tok := token.NewTaintExemptionToken(data, -time.Second, "admin") // 已过期
	v.Store("agent-1", tok)

	if v.Lookup("agent-1") != nil {
		t.Error("expired token should not be returned")
	}
	// 惰性清理：过期后再次查询应仍为 nil（未 panic，内部条目已被删除）。
	if v.Lookup("agent-1") != nil {
		t.Error("expired token entry should have been purged")
	}
}

func TestExemptionVault_StoreIgnoresEmptyAgentIDOrNilToken(t *testing.T) {
	v := token.NewExemptionVault()
	v.Store("", token.NewTaintExemptionToken([]byte("x"), time.Minute, "admin"))
	if v.Lookup("") != nil {
		t.Error("empty agentID must be ignored on Store")
	}
	v.Store("agent-1", nil)
	if v.Lookup("agent-1") != nil {
		t.Error("nil token must be ignored on Store")
	}
}

func TestExemptionVault_IsReviewed(t *testing.T) {
	v := token.NewExemptionVault()
	data := []byte("reviewed content")
	tok := token.NewTaintExemptionToken(data, time.Minute, "reviewer-1")
	v.Store("agent-1", tok)

	if !v.IsReviewed("agent-1", data) {
		t.Error("expected IsReviewed to be true for matching agentID+content")
	}
	if v.IsReviewed("agent-1", []byte("different content")) {
		t.Error("mismatched content must not be considered reviewed")
	}
	if v.IsReviewed("agent-2", data) {
		t.Error("unrelated agentID must not be considered reviewed")
	}
	if v.IsReviewed("", data) {
		t.Error("empty agentID must not be considered reviewed")
	}
}

func TestExemptionVault_IsReviewed_ExpiredTokenNotReviewed(t *testing.T) {
	v := token.NewExemptionVault()
	data := []byte("reviewed content")
	tok := token.NewTaintExemptionToken(data, -time.Second, "reviewer-1")
	v.Store("agent-1", tok)

	if v.IsReviewed("agent-1", data) {
		t.Error("expired exemption token must not count as reviewed")
	}
}
