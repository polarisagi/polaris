package agent

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/tool/catalog"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// SetAgentProfile 以子 Agent 角色运行（ADR-0103 决策三）。nil 清除角色（Pool 复用实例时
// 必须显式调用，防止上一次委派的角色泄漏到下一次执行）。角色只收窄能力：
// 工具白/黑名单与只读在执行入口硬拦截（checkProfileTool），可见性过滤仅减少模型误用。
func (a *Agent) SetAgentProfile(p *types.AgentProfileSpec) {
	var restriction *catalog.ToolRestriction
	if p != nil {
		restriction = catalog.NewToolRestriction(p.Tools, p.DisallowedTools)
	}
	a.profileTools.Store(restriction)

	a.sCtx.Mu.Lock()
	defer a.sCtx.Mu.Unlock()
	a.sCtx.AgentProfile = p
	if p != nil && p.MaxTurns > 0 && (a.sCtx.MaxStepsLimit == 0 || a.sCtx.MaxStepsLimit > p.MaxTurns) {
		a.sCtx.MaxStepsLimit = p.MaxTurns
		a.sCtx.InitialMaxStepsLimit = p.MaxTurns
	}
}

func (a *Agent) agentProfile() *types.AgentProfileSpec {
	a.sCtx.Mu.RLock()
	defer a.sCtx.Mu.RUnlock()
	return a.sCtx.AgentProfile
}

// visibleCatalog 当前角色可见的工具目录（无角色时即完整目录）。
func (a *Agent) visibleCatalog() catalog.Catalog {
	if a.catalog == nil {
		return nil
	}
	p := a.agentProfile()
	if p == nil {
		return a.catalog
	}
	if p.AllowDelegation {
		return catalog.Restrict(a.catalog, a.profileTools.Load())
	}
	// 禁止委派时委派工具也不展示：子 Agent 不应看到自己无权调用的工具。
	deny := append(append([]string(nil), p.DisallowedTools...), delegationTools()...)
	return catalog.Restrict(a.catalog, catalog.NewToolRestriction(p.Tools, deny))
}

// delegationTools 子 Agent 默认不得再委派（Claude 子 Agent 无 Agent 工具；Codex max_depth 默认 1）。
func delegationTools() []string {
	return []string{catalog.DelegateToolName, catalog.ListAgentsToolName, "spawn_planner"}
}

// checkProfileTool 执行入口硬拦截：所有 DAG 工具节点（含 code_act / 委派 / spawn_planner 特判）
// 在分发前都经过这里。可见性过滤不是安全边界——模型可能按名称调用未展示的工具。
func (a *Agent) checkProfileTool(toolName, delegateTarget string) error {
	p := a.agentProfile()
	if p == nil {
		return nil
	}
	tool, known := a.toolMeta(toolName)
	var reason string
	switch {
	case !p.AllowDelegation && isDelegationTool(toolName):
		reason = "subagents cannot delegate to other agents"
	case !a.profileTools.Load().Permits(toolName, tool.Source):
		reason = "tool is not in this agent's tool list"
	case toolName == catalog.DelegateToolName && !a.profileTools.Load().PermitsAgent(delegateTarget):
		reason = fmt.Sprintf("delegation target %q is not allowed for this agent", delegateTarget)
	case p.ReadOnly && writesState(toolName, tool, known):
		reason = "agent runs in read-only mode"
	default:
		return nil
	}
	metrics.GlobalSubagentToolDeniedTotal.Add(1)
	return apperr.New(apperr.CodeForbidden, fmt.Sprintf("agent %s: tool %s denied: %s", p.Name, toolName, reason))
}

func isDelegationTool(name string) bool {
	return slices.Contains(delegationTools(), name)
}

func (a *Agent) toolMeta(name string) (types.Tool, bool) {
	if a.toolRegistry == nil {
		return types.Tool{}, false
	}
	t, err := a.toolRegistry.Lookup(name)
	if err != nil {
		return types.Tool{}, false
	}
	return t, true
}

// writesState 只读角色（Codex sandbox_mode=read-only）只放行能力等级 ≤ CapReadOnly 的工具
// （与 S_VALIDATE isReadOnlyTool 同一判据）；code_act 恒视为可写（任意代码），元数据缺失的
// 未知工具 fail-closed 视为可写。
func writesState(name string, tool types.Tool, known bool) bool {
	return strings.HasPrefix(name, "code_act:") || !known || tool.Capability > types.CapReadOnly
}

// delegateTarget 委派调用的目标名（仅用于 Agent(a, b) 白名单判定）；解析失败返回空串，
// 由 PermitsAgent 按不在名单处理。
func delegateTarget(toolName string, args []byte) string {
	if toolName != catalog.DelegateToolName {
		return ""
	}
	var in struct {
		Target string `json:"target_agent_role"`
	}
	if json.Unmarshal(args, &in) != nil {
		return ""
	}
	return in.Target
}
