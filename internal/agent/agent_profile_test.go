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

// TestAgentProfile_PeerProfileHidesWritesAndMCPDelegation 对端委派任务的角色（只读 + 禁止再委派）：
// 写类工具与 MCP 委派通道都不展示；含越权节点的计划在 S_VALIDATE 前段即被打回，不落到污点 → HITL。
func TestAgentProfile_PeerProfileHidesWritesAndMCPDelegation(t *testing.T) {
	a := NewAgentWithDefaults("peer-agent")
	cat := catalog.NewMemoryCatalog()
	for name, lv := range map[string]types.CapabilityLevel{
		"read_file": types.CapReadOnly, "bash": types.CapPrivileged,
		"mcp__laptop__a2a_delegate": types.CapReadOnly, "mcp__laptop__a2a_delegate_async": types.CapReadOnly,
		"mcp__laptop__a2a_list_agents": types.CapReadOnly, "mcp__laptop__get_task": types.CapReadOnly,
	} {
		src := types.ToolMCP
		if name == "read_file" || name == "bash" {
			src = types.ToolBuiltin
		}
		cat.Register(protocol.CatalogEntry{Name: name, Source: src, Capability: lv, TrustTier: types.TrustCommunity})
	}
	a.InjectCatalog(cat)
	a.InjectToolExecutor(capToolExecutor{"read_file": types.CapReadOnly, "bash": types.CapPrivileged})
	a.SetAgentProfile(&types.AgentProfileSpec{Name: "peer:laptop", ReadOnly: true})

	got := map[string]bool{}
	for _, e := range a.visibleCatalog().List(context.Background(), types.TrustUntrusted) {
		got[e.Name] = true
	}
	// MCP 工具没有能力等级元数据，只读角色一律视为可写（fail-closed）。
	want := map[string]bool{"read_file": true}
	if len(got) != len(want) {
		t.Fatalf("peer profile view = %v, want only %v", got, want)
	}
	for n := range want {
		if !got[n] {
			t.Fatalf("peer profile view must keep %s, got %v", n, got)
		}
	}

	if err := a.checkPlanAgainstProfile(&protocol.DAGPlan{Nodes: []protocol.ExecNode{{ID: "n1", ToolName: "read_file"}}}); err != nil {
		t.Fatalf("read-only plan must pass: %v", err)
	}
	for _, bad := range []string{"bash", "mcp__laptop__a2a_delegate", "mcp__laptop__get_task"} {
		if err := a.checkPlanAgainstProfile(&protocol.DAGPlan{Nodes: []protocol.ExecNode{{ID: "n1", ToolName: bad}}}); apperr.CodeOf(err) != apperr.CodeForbidden {
			t.Fatalf("plan using %s must be rejected by the profile, got %v", bad, err)
		}
	}
}

func TestAgentProfile_FileRootConfinesTaskCtx(t *testing.T) {
	a := NewAgentWithDefaults("peer-agent-root")
	if _, ok := protocol.ConfinedRootFrom(a.withTaskScopeCtx(context.Background())); ok {
		t.Fatal("no profile → no confinement")
	}
	a.SetAgentProfile(&types.AgentProfileSpec{Name: "peer", ReadOnly: true, FileRoot: "/srv/peer-shared"})
	if root, ok := protocol.ConfinedRootFrom(a.withTaskScopeCtx(context.Background())); !ok || root != "/srv/peer-shared" {
		t.Fatalf("profile FileRoot must reach tool ctx, got %q %v", root, ok)
	}
}

// TestAgentProfile_AllowDelegationFalse_BlocksSpawnAgentsAndSubagentTools 验证跨节点/禁用委派的子任务
// 严格禁止调用 spawn_agents、agent_message、agent_status、agent_stop、agent_result（V4 / 安全边界）。
func TestAgentProfile_AllowDelegationFalse_BlocksSpawnAgentsAndSubagentTools(t *testing.T) {
	a := NewAgentWithDefaults("peer-non-delegating-agent")
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot)}
	a.InjectHandoffPoster(poster)

	cat := catalog.NewMemoryCatalog()
	tools := []string{
		"spawn_agents", "agent_message", "agent_status", "agent_stop", "agent_result",
		catalog.DelegateToolName, catalog.ListAgentsToolName, "read_file",
	}
	for _, n := range tools {
		cat.Register(protocol.CatalogEntry{Name: n, Source: types.ToolBuiltin, TrustTier: types.TrustSystem})
	}
	a.InjectCatalog(cat)
	executorTools := make(capToolExecutor)
	for _, n := range tools {
		executorTools[n] = types.CapReadOnly
	}
	a.InjectToolExecutor(executorTools)

	// AllowDelegation: false（如跨节点 mesh / MCP 委派任务）
	a.SetAgentProfile(&types.AgentProfileSpec{Name: "mesh-peer", AllowDelegation: false})

	// 1. visibleCatalog 不应包含任何委派与子智能体管理工具
	visible := a.visibleCatalog().List(context.Background(), types.TrustUntrusted)
	for _, e := range visible {
		if isDelegationTool(e.Name) {
			t.Fatalf("V4 缺陷：AllowDelegation=false 的角色在工具列表中看到了委派工具 %s", e.Name)
		}
	}

	// 2. checkProfileTool 硬拦截这些工具
	for _, dt := range []string{"spawn_agents", "agent_message", "agent_status", "agent_stop", "agent_result"} {
		if err := a.checkProfileTool(dt, ""); err == nil {
			t.Fatalf("V4 缺陷：AllowDelegation=false 必须拒绝工具 %s，但 checkProfileTool 放行了", dt)
		}
	}

	// 3. 直接调用 a.SpawnAgents 也被拦截
	_, err := a.SpawnAgents(context.Background(), types.SpawnAgentsArgs{
		Tasks: []types.SpawnTaskSpec{{Prompt: "nested agent"}},
	})
	if err == nil {
		t.Fatal("V4 缺陷：AllowDelegation=false 下 SpawnAgents 必须拒绝执行")
	}
}
