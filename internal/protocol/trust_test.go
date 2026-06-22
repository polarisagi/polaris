package protocol

import (
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

func TestTrustFromSignatureValid(t *testing.T) {
	tests := []struct {
		valid    bool
		expected types.TrustTier
	}{
		{true, types.TrustCommunity},
		{false, types.TrustUntrusted},
	}

	for _, tc := range tests {
		if got := TrustFromSignatureValid(tc.valid); got != tc.expected {
			t.Errorf("TrustFromSignatureValid(%v) = %v, expected %v", tc.valid, got, tc.expected)
		}
	}
}

func TestTrustTier_MaxSandboxTier(t *testing.T) {
	tests := []struct {
		tier     types.TrustTier
		expected int
	}{
		{types.TrustSystem, 3},
		{types.TrustOfficial, 2},
		{types.TrustCommunity, 1},
		{types.TrustLocal, 1},
		{types.TrustUntrusted, 1},
	}

	for _, tc := range tests {
		if got := tc.tier.MaxSandboxTier(); got != tc.expected {
			t.Errorf("MaxSandboxTier(%v) = %v, expected %v", tc.tier, got, tc.expected)
		}
	}
}

func TestTrustTier_TaintLevel(t *testing.T) {
	tests := []struct {
		tier     types.TrustTier
		expected int
	}{
		{types.TrustSystem, 0},
		{types.TrustOfficial, 1},
		{types.TrustCommunity, 2},
		{types.TrustLocal, 2},
		{types.TrustUntrusted, 2},
	}

	for _, tc := range tests {
		if got := tc.tier.TaintLevel(); got != tc.expected {
			t.Errorf("TaintLevel(%v) = %v, expected %v", tc.tier, got, tc.expected)
		}
	}
}

func TestTrustTier_ApprovalRequired(t *testing.T) {
	tests := []struct {
		tier     types.TrustTier
		expected bool
	}{
		{types.TrustSystem, false},
		{types.TrustOfficial, false},
		{types.TrustCommunity, true},
		{types.TrustLocal, true},
		{types.TrustUntrusted, true},
	}

	for _, tc := range tests {
		if got := tc.tier.ApprovalRequired(); got != tc.expected {
			t.Errorf("ApprovalRequired(%v) = %v, expected %v", tc.tier, got, tc.expected)
		}
	}
}

func TestTrustTier_MCPApprovalMode(t *testing.T) {
	tests := []struct {
		tier     types.TrustTier
		expected string
	}{
		{types.TrustSystem, "auto"},
		{types.TrustOfficial, "auto"},
		{types.TrustCommunity, "prompt"},
		{types.TrustLocal, "prompt"},
		{types.TrustUntrusted, "prompt"},
	}

	for _, tc := range tests {
		if got := tc.tier.MCPApprovalMode(); got != tc.expected {
			t.Errorf("MCPApprovalMode(%v) = %v, expected %v", tc.tier, got, tc.expected)
		}
	}
}

func TestTrustTier_Trusted(t *testing.T) {
	tests := []struct {
		tier     types.TrustTier
		expected bool
	}{
		{types.TrustSystem, true},
		{types.TrustOfficial, true},
		{types.TrustCommunity, false},
		{types.TrustLocal, false},
		{types.TrustUntrusted, false},
	}

	for _, tc := range tests {
		if got := tc.tier.Trusted(); got != tc.expected {
			t.Errorf("Trusted(%v) = %v, expected %v", tc.tier, got, tc.expected)
		}
	}
}

func TestTrustTier_String(t *testing.T) {
	tests := []struct {
		tier     types.TrustTier
		expected string
	}{
		{types.TrustSystem, "system"},
		{types.TrustOfficial, "official"},
		{types.TrustCommunity, "community"},
		{types.TrustLocal, "local"},
		{types.TrustUntrusted, "untrusted"},
		{types.TrustTier(999), "untrusted"},
	}

	for _, tc := range tests {
		if got := tc.tier.String(); got != tc.expected {
			t.Errorf("String(%v) = %v, expected %v", tc.tier, got, tc.expected)
		}
	}
}
