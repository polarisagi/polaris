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
		{"read-only", types.CapReadOnly},
		{"read-network", types.CapReadNetwork},
		{"write-local", types.CapWriteLocal},
		{"write-network", types.CapWriteNetwork},
		{"privileged", types.CapPrivileged},
	}
	for _, c := range cases {
		out, err := mapCapability(c.input)
		if err != nil || out != c.expected {
			t.Errorf("expected %v for %q, got %v (err=%v)", c.expected, c.input, out, err)
		}
	}
}

// 未知/空/历史无效值必须报错（fail-closed），不得静默降为只读。
func TestMapCapability_InvalidFailsClosed(t *testing.T) {
	for _, in := range []string{"", "unknown", "CapWrite", "write_local", "write", "READ-ONLY"} {
		if _, err := mapCapability(in); err == nil {
			t.Errorf("mapCapability(%q) should fail closed", in)
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
	}
	for _, c := range cases {
		out, err := mapRiskLevel(c.input)
		if err != nil || out != c.expected {
			t.Errorf("expected %v for %q, got %v (err=%v)", c.expected, c.input, out, err)
		}
	}
	for _, in := range []string{"", "2", "bogus"} {
		if _, err := mapRiskLevel(in); err == nil {
			t.Errorf("mapRiskLevel(%q) should fail closed", in)
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

// 全部内嵌扩展工具 manifest 必须可加载（含合法 capability/risk_level）。
func TestAllEmbeddedExtensionManifestsValid(t *testing.T) {
	for _, n := range []string{"knowledge_search", "knowledge_ingest", "install_extension", "search_extension"} {
		if _, err := GetExtensionToolMeta(n); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
}
