package agent

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/tool/catalog"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// capToolExecutor 按名称返回能力等级；未登记的工具查不到（未知工具）。
type capToolExecutor map[string]types.CapabilityLevel

func (c capToolExecutor) Lookup(name string) (types.Tool, error) {
	lv, ok := c[name]
	if !ok {
		return types.Tool{}, apperr.New(apperr.CodeNotFound, name)
	}
	return types.Tool{Name: name, Source: types.ToolBuiltin, Capability: lv}, nil
}
func (capToolExecutor) ExecuteWithTaint(context.Context, string, []byte, types.TaintLevel) (*types.ToolResult, error) {
	return &types.ToolResult{Success: true}, nil
}

func TestAgentProfile_ToolEnforcement(t *testing.T) {
	a := NewAgentWithDefaults("profile-agent")
	a.InjectToolExecutor(capToolExecutor{"read_file": types.CapReadOnly, "write_file": types.CapWriteLocal, "grep": types.CapReadOnly})
	a.SetAgentProfile(&types.AgentProfileSpec{Name: "reviewer", Tools: []string{"Read", "Write", "Agent"}, ReadOnly: true, MaxTurns: 4})

	allowed := map[string]bool{"read_file": true, "write_file": false, "grep": false, "code_act:python": false,
		catalog.DelegateToolName: false, "spawn_planner": false}
	for tool, want := range allowed {
		if err := a.checkProfileTool(tool, ""); (err == nil) != want {
			t.Errorf("%s: allowed=%v err=%v", tool, want, err)
		}
	}
	if a.sCtx.MaxStepsLimit != 4 {
		t.Fatalf("maxTurns must clamp the step budget, got %d", a.sCtx.MaxStepsLimit)
	}

	// 清除角色：Pool 复用实例时不得残留上一次委派的限制。
	a.SetAgentProfile(nil)
	if err := a.checkProfileTool("write_file", ""); err != nil {
		t.Fatalf("profile must be cleared: %v", err)
	}
}

func TestAgentProfile_DelegationTargetsAndCatalogView(t *testing.T) {
	a := NewAgentWithDefaults("profile-agent-2")
	cat := catalog.NewMemoryCatalog()
	for _, n := range []string{"read_file", "write_file", catalog.DelegateToolName} {
		cat.Register(protocol.CatalogEntry{Name: n, Source: types.ToolBuiltin, TrustTier: types.TrustSystem})
	}
	a.InjectCatalog(cat)
	a.SetAgentProfile(&types.AgentProfileSpec{Name: "lead", Tools: []string{"Read", "Agent(worker)"}, AllowDelegation: true})
	if err := a.checkProfileTool(catalog.DelegateToolName, "worker"); err != nil {
		t.Fatalf("worker is allowed: %v", err)
	}
	if err := a.checkProfileTool(catalog.DelegateToolName, "other"); err == nil {
		t.Fatal("Agent(worker) must reject other targets")
	}
	if entries := a.visibleCatalog().List(context.Background(), types.TrustUntrusted); len(entries) != 2 {
		t.Fatalf("catalog view must hide write_file: %v", entries)
	}
	a.SetAgentProfile(&types.AgentProfileSpec{Name: "sub"})
	for _, e := range a.visibleCatalog().List(context.Background(), types.TrustUntrusted) {
		if e.Name == catalog.DelegateToolName {
			t.Fatal("subagents without delegation must not see transfer_to_agent")
		}
	}
}
