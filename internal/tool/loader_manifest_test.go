package tool

import (
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/polarisagi/polaris/pkg/types"
)

// 遍历全部内置 tool.yaml：capability/risk_level 必须是合法枚举值（fail-closed 门控，ADR-0117）。
func TestAllBuiltinManifestsValid(t *testing.T) {
	entries, err := builtinFS.ReadDir("builtin")
	if err != nil {
		t.Fatal(err)
	}
	validCap := map[string]bool{"read-only": true, "read-network": true, "write-local": true, "write-network": true, "privileged": true}
	validRisk := map[string]bool{"low": true, "medium": true, "high": true}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, err := builtinFS.ReadFile("builtin/" + e.Name() + "/tool.yaml")
		if err != nil {
			continue // 仅含 Go 代码的子目录
		}
		n++
		var meta toolYAML
		if err := yaml.Unmarshal(raw, &meta); err != nil {
			t.Errorf("%s: parse: %v", e.Name(), err)
			continue
		}
		if !validCap[meta.Capability] {
			t.Errorf("%s: invalid capability %q", e.Name(), meta.Capability)
		}
		if !validRisk[meta.RiskLevel] {
			t.Errorf("%s: invalid risk_level %q", e.Name(), meta.RiskLevel)
		}
		if _, err := GetBuiltinToolMeta(e.Name()); err != nil {
			t.Errorf("%s: GetBuiltinToolMeta: %v", e.Name(), err)
		}
	}
	if n < 40 {
		t.Fatalf("expected >=40 builtin manifests, scanned %d", n)
	}
}

// 能力分级修正的回归：错标为只读/写网络的工具各归其位。
func TestBuiltinCapabilityCorrections(t *testing.T) {
	want := map[string]types.CapabilityLevel{
		"web_fetch": types.CapReadNetwork, "feed_fetch": types.CapReadNetwork, "web_search": types.CapReadNetwork,
		"agent_message": types.CapWriteLocal, "agent_stop": types.CapWriteLocal,
		"spawn_agents": types.CapReadOnly, "checkpoint_restore": types.CapWriteLocal,
		"execute_wasm": types.CapWriteLocal, "job_kill": types.CapWriteLocal, "harness_rollback": types.CapWriteLocal,
		"video_analysis": types.CapWriteNetwork, "agent_status": types.CapReadOnly, "agent_result": types.CapReadOnly,
	}
	for name, c := range want {
		meta, err := GetBuiltinToolMeta(name)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if meta.Capability != c {
			t.Errorf("%s: capability=%v want %v", name, meta.Capability, c)
		}
	}
	if m, _ := GetBuiltinToolMeta("checkpoint_restore"); m.RiskLevel != types.RiskHigh {
		t.Errorf("checkpoint_restore risk should be high, got %v", m.RiskLevel)
	}
}

func TestMapCapabilityFailClosed(t *testing.T) {
	for _, in := range []string{"", "unknown", "CapWrite", "write_local", "write"} {
		if _, err := mapCapability(in); err == nil {
			t.Errorf("mapCapability(%q) must fail closed", in)
		}
	}
	for in, want := range map[string]types.CapabilityLevel{
		"read-only": types.CapReadOnly, "read-network": types.CapReadNetwork, "write-local": types.CapWriteLocal,
		"write-network": types.CapWriteNetwork, "privileged": types.CapPrivileged,
	} {
		if got, err := mapCapability(in); err != nil || got != want {
			t.Errorf("mapCapability(%q)=%v,%v", in, got, err)
		}
	}
}

func TestCapabilityHelpers(t *testing.T) {
	cases := []struct {
		c                  types.CapabilityLevel
		taintRO, net, wnet bool
	}{
		{types.CapReadOnly, true, false, false},
		{types.CapReadNetwork, true, true, false},
		{types.CapWriteLocal, false, false, false},
		{types.CapWriteNetwork, false, true, true},
		{types.CapPrivileged, false, true, true},
	}
	for _, c := range cases {
		if c.c.IsReadOnlyForTaint() != c.taintRO || c.c.AllowsNetwork() != c.net || c.c.WritesNetwork() != c.wnet {
			t.Errorf("cap %v helpers mismatch", c.c)
		}
	}
}
