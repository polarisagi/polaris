package token_test

import (
	"github.com/polarisagi/polaris/internal/security/token"

	"testing"
	"time"
)

func TestCapabilityTokenExtra(t *testing.T) {
	mgr, err := token.NewTokenManager()
	if err != nil {
		t.Fatal(err)
	}

	tok, err := mgr.Mint("sys", []token.CapabilityType{"capA", "capB"}, 0, time.Hour, 100)
	if err != nil {
		t.Fatal(err)
	}

	// HasCap
	has, err := mgr.HasCap(tok, "capA")
	if !has || err != nil {
		t.Errorf("expected HasCap true, got %v %v", has, err)
	}
	has, err = mgr.HasCap(tok, "capC")
	if has || err != nil {
		t.Errorf("expected HasCap false, got %v %v", has, err)
	}

	// Revoke
	mgr.Revoke(tok.Claims.TokenID)

	// Should be invalid now
	if err := mgr.Verify(tok); err == nil {
		t.Errorf("expected invalid token after revoke, got nil")
	}
}
