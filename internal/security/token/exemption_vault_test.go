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
	got := v.Lookup("agent-1", data)
	if got == nil || !got.Valid(data) {
		t.Fatal("expected stored token to be retrievable and valid")
	}

	if v.Lookup("agent-2", data) != nil {
		t.Error("unrelated agentID should return nil")
	}
	if v.Lookup("agent-1", []byte("other")) != nil {
		t.Error("mismatched content should return nil")
	}
}

// 多节点分别送审：后一次批准不得冲掉前一次（2026-09-25 覆盖写缺陷的回归用例）。
func TestExemptionVault_MultipleTokensPerAgent(t *testing.T) {
	v := token.NewExemptionVault()
	a, b := []byte(`{"path":"a.txt"}`), []byte(`{"path":"b.txt"}`)
	v.Store("agent-1", token.NewTaintExemptionToken(a, time.Minute, "u"))
	v.Store("agent-1", token.NewTaintExemptionToken(b, time.Minute, "u"))

	if !v.IsReviewed("agent-1", a) || !v.IsReviewed("agent-1", b) {
		t.Fatal("both approvals must remain valid")
	}
}

func TestExemptionVault_CapEvictsOldest(t *testing.T) {
	v := token.NewExemptionVault()
	first := []byte("first")
	v.Store("agent-1", token.NewTaintExemptionToken(first, time.Minute, "u"))
	for i := 0; i < 16; i++ {
		v.Store("agent-1", token.NewTaintExemptionToken([]byte{byte(i)}, time.Minute, "u"))
	}
	if v.IsReviewed("agent-1", first) {
		t.Error("oldest token should be evicted once the per-agent cap is exceeded")
	}
}

func TestExemptionVault_Lookup_ExpiredRemoved(t *testing.T) {
	v := token.NewExemptionVault()
	data := []byte("payload")
	tok := token.NewTaintExemptionToken(data, -time.Second, "admin") // 已过期
	v.Store("agent-1", tok)

	if v.Lookup("agent-1", data) != nil {
		t.Error("expired token should not be returned")
	}
	if v.Lookup("agent-1", data) != nil {
		t.Error("expired token entry should have been purged")
	}
}

func TestExemptionVault_StoreIgnoresEmptyAgentIDOrNilToken(t *testing.T) {
	v := token.NewExemptionVault()
	x := []byte("x")
	v.Store("", token.NewTaintExemptionToken(x, time.Minute, "admin"))
	if v.Lookup("", x) != nil {
		t.Error("empty agentID must be ignored on Store")
	}
	v.Store("agent-1", nil)
	if v.Lookup("agent-1", x) != nil {
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
