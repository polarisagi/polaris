package native

import (
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

func TestMapCapability_AllValues(t *testing.T) {
	cases := []struct {
		input    string
		expected types.CapabilityLevel
	}{
		{"write-local", types.CapWriteLocal},
		{"write-network", types.CapWriteNetwork},
		{"privileged", types.CapPrivileged},
		{"unknown", types.CapReadOnly},
		{"", types.CapReadOnly},
	}

	for _, c := range cases {
		if out := mapCapability(c.input); out != c.expected {
			t.Errorf("expected %v for %q, got %v", c.expected, c.input, out)
		}
	}
}

func TestMapRiskLevel_AllValues(t *testing.T) {
	cases := []struct {
		input    string
		expected types.RiskLevel
	}{
		{"medium", types.RiskMedium},
		{"high", types.RiskHigh},
		{"privileged", types.RiskPrivileged},
		{"low", types.RiskLow},
		{"", types.RiskLow},
	}

	for _, c := range cases {
		if out := mapRiskLevel(c.input); out != c.expected {
			t.Errorf("expected %v for %q, got %v", c.expected, c.input, out)
		}
	}
}

func TestMapSandbox_AllValues(t *testing.T) {
	cases := []struct {
		input    string
		expected types.SandboxTier
	}{
		{"container", types.SandboxContainer},
		{"remote", types.SandboxRemote},
		{"wasm", types.SandboxWasm},
		{"in-process", types.SandboxInProcess},
		{"", types.SandboxInProcess},
	}

	for _, c := range cases {
		if out := mapSandbox(c.input); out != c.expected {
			t.Errorf("expected %v for %q, got %v", c.expected, c.input, out)
		}
	}
}

func TestMapSideEffects_Mixed(t *testing.T) {
	cases := []struct {
		input    []string
		expected []types.SideEffect
	}{
		{[]string{"file-write", "network-call"}, []types.SideEffect{types.SideFileWrite, types.SideNetworkCall}},
		{[]string{"process-spawn", "state-mutate"}, []types.SideEffect{types.SideProcessSpawn, types.SideStateMutate}},
		{[]string{"unknown"}, []types.SideEffect{types.SideNone}},
		{[]string{}, []types.SideEffect{types.SideNone}},
	}

	for _, c := range cases {
		out := mapSideEffects(c.input)
		if len(out) != len(c.expected) {
			t.Fatalf("length mismatch: expected %d, got %d", len(c.expected), len(out))
		}
		for i := range out {
			if out[i] != c.expected[i] {
				t.Errorf("mismatch at %d: expected %v, got %v", i, c.expected[i], out[i])
			}
		}
	}
}

func TestGetExtensionToolMeta_NotFound(t *testing.T) {
	_, err := GetExtensionToolMeta("this_does_not_exist")
	if err == nil {
		t.Errorf("expected error for non-existent tool")
	}
}
