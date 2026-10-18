package agent

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
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
	var view catalog.Catalog
	if p.AllowDelegation {
		view = catalog.Restrict(a.catalog, a.profileTools.Load())
	} else {
		// 禁止委派时委派工具也不展示：子 Agent 不应看到自己无权调用的工具。
		deny := append(append([]string(nil), p.DisallowedTools...), delegationTools()...)
		view = catalog.Restrict(a.catalog, catalog.NewToolRestriction(p.Tools, deny))
	}
	if p.ReadOnly || !p.AllowDelegation {
		// 只读角色看不到写类工具、禁止委派的角色看不到 MCP 委派通道（mcp__<peer>__a2a_delegate 等）：
		// 否则模型照样规划出 bash 之类的节点，在 S_VALIDATE 撞上污点闸门转成本机 HITL——对端
		// 节点委派来的任务会把审批弹给本机用户（ADR-0111 双实例实测发现）。
		view = catalog.Filter(view, func(e protocol.CatalogEntry) bool {
			if !p.AllowDelegation && isDelegationTool(e.Name) {
				return false
			}
			return !p.ReadOnly || !writesState(e.Name, types.Tool{Name: e.Name, Source: e.Source, Capability: e.Capability}, true)
		})
	}
	return view
}

// delegationTools 子 Agent 默认不得再委派（Claude 子 Agent 无 Agent 工具；Codex max_depth 默认 1）。
func delegationTools() []string {
	return []string{catalog.DelegateToolName, catalog.ListAgentsToolName, "spawn_planner", "spawn_agents"}
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

// checkPlanAgainstProfile 对整份计划逐节点做 checkProfileTool（无角色时恒通过）。
func (a *Agent) checkPlanAgainstProfile(plan *protocol.DAGPlan) error {
	if plan == nil || a.agentProfile() == nil {
		return nil
	}
	for _, n := range plan.Nodes {
		if err := a.checkProfileTool(n.ToolName, delegateTarget(n.ToolName, n.Args)); err != nil {
			return err
		}
	}
	return nil
}

func isDelegationTool(name string) bool {
	return slices.Contains(delegationTools(), name) || isMCPDelegationTool(name)
}

// isMCPDelegationTool MCP 服务器暴露的 A2A 委派通道（ADR-0084 契约：a2a_delegate / a2a_list_agents，
// 以及客户端派生的 _async 变体）。经 MCP 直接调用它们与 transfer_to_agent 同样是「再委派」。
func isMCPDelegationTool(name string) bool {
	rest, ok := strings.CutPrefix(name, "mcp__")
	if !ok {
		return false
	}
	i := strings.LastIndex(rest, "__")
	if i < 0 {
		return false
	}
	tool := strings.TrimSuffix(rest[i+2:], "_async")
	return tool == "a2a_delegate" || tool == "a2a_list_agents"
}

// toolMeta 先查工具注册表；技能的模型调用名只在目录中（注册表内是 "skill__" 前缀名，见
// dispatch.route），回落目录取来源。
func (a *Agent) toolMeta(name string) (types.Tool, bool) {
	if a.toolRegistry != nil {
		if t, err := a.toolRegistry.Lookup(name); err == nil {
			return t, true
		}
	}
	if a.catalog != nil {
		if e, ok := a.catalog.Lookup(name); ok {
			return types.Tool{Name: e.Name, Source: e.Source, TrustTier: e.TrustTier, Capability: e.Capability}, true
		}
	}
	return types.Tool{}, false
}

// writesState 只读角色（Codex sandbox_mode=read-only）只放行能力等级 ≤ CapReadOnly 的工具
// （与 S_VALIDATE isReadOnlyTool 同一判据）；code_act 恒视为可写（任意代码），技能可能携带
// 沙箱脚本、目录条目无能力等级，同样视为可写；元数据缺失的未知工具 fail-closed 视为可写。
//
// MCP 工具同样视为可写：注册时不带能力等级（零值恰好是 CapReadOnly），不 fail-closed 的话只读角色
// 会放行任意 MCP 服务器的写操作（如文件系统类 MCP），对端委派任务也就能借道写本机。
func writesState(name string, tool types.Tool, known bool) bool {
	return strings.HasPrefix(name, "code_act:") || !known || tool.Source == types.ToolSkill || tool.Source == types.ToolMCP ||
		tool.Capability > types.CapReadOnly
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
