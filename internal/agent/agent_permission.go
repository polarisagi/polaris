package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/polarisagi/polaris/internal/agent/schemavalidate"
	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	protorepo "github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/pkg/types"
)

// 统一权限判定接入（ADR-0117 决策四/五）：S_VALIDATE 对计划里每个节点调用 policy.Decide，
// 放行的直接执行（审计留痕），需询问的合并成一张批量审批卡。Decide 是确定性规则，不调用 LLM。

// permissionModeReader 实时读取权限模式（偏好表是唯一事实源，切换后下一次工具调用即生效）。
type permissionModeReader interface {
	GetPermissionMode(ctx context.Context) (types.PermissionMode, error)
}

// permissionState 是 Agent 上与统一权限判定相关的装配与运行态。
type permissionState struct {
	modes permissionModeReader
	rules protorepo.ApprovalRuleRepository

	granted atomic.Pointer[planGrants] // 本计划各放行调用的授权凭据（ADR-0121 决策二）

	mu         sync.Mutex
	denied     map[string]bool // turnID|callKey：本回合已被用户拒绝，不再重复打扰
	secretTurn *string         // 读取过凭据的回合 ID（nil = 本会话尚未读取过）
}

// planGrants 一次 S_VALIDATE 放行结果：调用指纹 → 凭据，以及本计划实际生效的权限模式。
type planGrants struct {
	grants map[string]protocol.PermissionGrant
	mode   types.PermissionMode
}

// InjectPermission 注入权限模式来源与项目授权规则库。modes 为 nil 时不启用统一权限判定
// （沿用旧的污点复核路径），生产装配必须注入。rules 可为 nil（不支持“记住”）。
func (a *Agent) InjectPermission(modes permissionModeReader, rules protorepo.ApprovalRuleRepository) {
	a.perm.modes = modes
	a.perm.rules = rules
}

// authorizePlan 对计划内每个节点做统一权限判定。返回 nil 表示全部放行（含用户批准）；
// 否则返回的 DAGValidationError 的 Reason 直接回灌给模型（D6：原因 + 替代方案）。
func (a *Agent) authorizePlan(ctx context.Context, vCtx *protocol.DAGValidationContext) error {
	if a.perm.modes == nil || vCtx.Plan == nil || len(vCtx.Plan.Nodes) == 0 {
		return nil
	}
	mode, err := a.perm.modes.GetPermissionMode(ctx)
	if err != nil {
		slog.WarnContext(ctx, "permission: read mode failed, using strictest mode", "err", err)
		mode = types.ModeDefault
	}
	mode = a.currentSecurityPolicy().CapMode(mode) // 部署档位只设上限（ADR-0121 决策一）
	pctx := a.policyContext(ctx, vCtx.Plan)

	var asks []pendingAsk
	pre := make(map[string]protocol.PermissionGrant)
	for _, n := range vCtx.Plan.Nodes {
		tool, ok := a.decisionTool(n.ToolName)
		if !ok {
			continue
		}
		taint := max(n.TaintLevel, vCtx.ActiveTaintLevel)
		d := policy.Decide(mode, tool, n.Args, taint, pctx)
		key := toolCallKey(n.ToolName, n.Args)
		if d.Verdict == policy.VerdictDeny {
			a.auditPermission(ctx, vCtx, "permission_denied", n, mode, "deny:"+d.Reason)
			return denyError(n, denyReasonText(n.ToolName, d.Reason))
		}
		if d.Verdict == policy.VerdictAllow {
			a.noteAutoAllow(ctx, vCtx, n, d, mode)
			a.grantExemption(vCtx, n, taint, "auto:"+d.Auto)
			pre[key] = protocol.GrantAuto
			if isExplicitTrust(d) {
				pre[key] = protocol.GrantExplicit
			}
			continue
		}
		if a.permDenied(key) {
			return denyError(n, denyRepeated(n.ToolName))
		}
		asks = append(asks, pendingAsk{node: n, tool: tool, taint: taint, d: d, key: key})
	}
	if len(asks) > 0 {
		if err := a.askBatch(ctx, vCtx, asks, mode); err != nil {
			return err
		}
		for _, k := range asks {
			pre[k.key] = protocol.GrantExplicit // 用户在审批卡上允许
		}
	}
	a.markSecretFlow(vCtx.Plan)
	a.perm.granted.Store(&planGrants{grants: pre, mode: mode})
	return nil
}

// isExplicitTrust 判定放行是否来自“明确信任”（完全访问 / 已记住的授权）。
// 这类放行下发 GrantExplicit：bash/run_command 宿主执行、工具内命令风险 HITL 让位；
// 自动放行只下发 GrantAuto，仍保留工具内的二次防线。
func isExplicitTrust(d policy.Decision) bool {
	return d.Auto == "full_access" || d.RuleID != ""
}

// grantFor 取该调用的授权凭据与本计划生效的权限模式（供 JIT 签发时写入 ctx）。
// 不在放行表里的调用（fail-closed 兜底）返回 GrantNone。
func (a *Agent) grantFor(toolName string, args []byte) (protocol.PermissionGrant, types.PermissionMode) {
	pg := a.perm.granted.Load()
	if pg == nil {
		return protocol.GrantNone, ""
	}
	return pg.grants[toolCallKey(toolName, args)], pg.mode
}

// AuthorizeCall 实现 protocol.ToolCallAuthorizer：code_act 工具桥内层调用逐次走 policy.Decide
// （ADR-0121 决策六）。桥内没有审批通道，Ask 与 Deny 一律拒绝，由脚本改为在 code_act 之外单独调用。
func (a *Agent) AuthorizeCall(ctx context.Context, tool types.Tool, args []byte, taint types.TaintLevel) (protocol.PermissionGrant, string, bool) {
	if a.perm.modes == nil {
		return protocol.GrantNone, "permission mode source not configured", false
	}
	if err := schemavalidate.ValidateAgainstStrict(tool.InputSchema, args); err != nil {
		return protocol.GrantNone, "tool arguments failed schema validation", false
	}
	mode, err := a.perm.modes.GetPermissionMode(ctx)
	if err != nil {
		slog.WarnContext(ctx, "permission: read mode failed in code_act bridge, using strictest mode", "err", err)
		mode = types.ModeDefault
	}
	mode = a.currentSecurityPolicy().CapMode(mode)
	d := policy.Decide(mode, tool, args, taint, a.policyContext(ctx, nil))
	switch d.Verdict {
	case policy.VerdictAllow:
		if tool.Name == "secret_get" {
			a.markSecretRead() // 桥内读取凭据同样要让之后的联网按外流处理
		}
		if isExplicitTrust(d) {
			return protocol.GrantExplicit, "", true
		}
		return protocol.GrantAuto, "", true
	case policy.VerdictDeny:
		return protocol.GrantNone, denyReasonText(tool.Name, d.Reason), false
	default:
		reason := d.Reason
		if reason == "" {
			reason = "requires user approval in " + string(mode) + " mode"
		}
		return protocol.GrantNone, fmt.Sprintf("tool %q %s", tool.Name, reason), false
	}
}

// markSecretRead 记下“本回合读取过凭据”（桥内单次调用版的 markSecretFlow）。
func (a *Agent) markSecretRead() {
	id := a.turnID()
	a.perm.mu.Lock()
	a.perm.secretTurn = &id
	a.perm.mu.Unlock()
}

// decisionTool 取判定用的工具定义。控制类伪工具不参与；code_act 按特权工具处理；
// 查不到的工具交给后续校验/执行层报错，不在这里放行也不在这里拦截。
func (a *Agent) decisionTool(name string) (types.Tool, bool) {
	switch {
	case name == "finish" || name == "transfer_to_agent":
		return types.Tool{}, false
	case name == "code_act" || strings.HasPrefix(name, "code_act:"):
		return types.Tool{Name: "code_act", Capability: types.CapPrivileged, RiskLevel: types.RiskHigh}, true
	case a.toolRegistry == nil:
		return types.Tool{}, false
	}
	t, err := a.toolRegistry.Lookup(name)
	if err != nil {
		return types.Tool{}, false
	}
	return t, true
}

// policyContext 组装判定环境：工作区可信根、家目录、本项目已记住的授权、凭据外流标志。
func (a *Agent) policyContext(ctx context.Context, plan *protocol.DAGPlan) policy.Context {
	pc := policy.Context{SecretFlow: a.secretFlowActive()}
	roots := []string{a.currentProjectRoot(), a.workspaceRoot}
	if cfg := config.Get(); cfg != nil {
		roots = append(roots, cfg.Sandbox.AllowedPaths...) // 与工具/沙箱白名单同一组可信根（ADR-0121 决策四）
	}
	for _, r := range roots {
		if r = strings.TrimSpace(r); r != "" {
			pc.Roots = append(pc.Roots, filepath.Clean(r))
		}
	}
	pc.Home, _ = os.UserHomeDir()
	if plan != nil && planReadsSecret(plan) {
		pc.SecretFlow = true // 同一批里既取凭据又联网：并行执行时无法保证先后，按外流处理
	}
	if a.perm.rules != nil {
		rows, err := a.perm.rules.ListRules(ctx, a.currentProjectID())
		if err != nil {
			slog.WarnContext(ctx, "permission: list approval rules failed, ignoring remembered rules", "err", err)
		}
		for _, r := range rows {
			pc.Rules = append(pc.Rules, policy.Rule{ID: r.ID, Kind: policy.RuleKind(r.Kind), Pattern: r.Pattern})
		}
	}
	return pc
}

func planReadsSecret(plan *protocol.DAGPlan) bool {
	for _, n := range plan.Nodes {
		if n.ToolName == "secret_get" {
			return true
		}
	}
	return false
}

func (a *Agent) turnID() string {
	if a.sCtx == nil {
		return ""
	}
	a.sCtx.Mu.RLock()
	defer a.sCtx.Mu.RUnlock()
	return a.sCtx.TurnID
}

// markSecretFlow 计划里含 secret_get 时记下“本回合读取过凭据”，之后本回合的联网操作按外流硬限制处理。
func (a *Agent) markSecretFlow(plan *protocol.DAGPlan) {
	if plan == nil || !planReadsSecret(plan) {
		return
	}
	id := a.turnID()
	a.perm.mu.Lock()
	a.perm.secretTurn = &id
	a.perm.mu.Unlock()
}

func (a *Agent) secretFlowActive() bool {
	a.perm.mu.Lock()
	defer a.perm.mu.Unlock()
	return a.perm.secretTurn != nil && *a.perm.secretTurn == a.turnID()
}

func (a *Agent) permDenied(key string) bool {
	a.perm.mu.Lock()
	defer a.perm.mu.Unlock()
	return a.perm.denied[a.turnID()+"|"+key]
}

func (a *Agent) markPermDenied(key string) {
	a.perm.mu.Lock()
	defer a.perm.mu.Unlock()
	if a.perm.denied == nil {
		a.perm.denied = make(map[string]bool)
	}
	a.perm.denied[a.turnID()+"|"+key] = true
}

// grantExemption 污点 >= Medium 的节点获准后铸造内容哈希豁免令牌，让 L1 污点闸门与
// 出口污点检查认可这次已授权的调用；令牌随参数变化即失效。
func (a *Agent) grantExemption(vCtx *protocol.DAGValidationContext, n protocol.ExecNode, taint types.TaintLevel, by string) {
	if taint < types.TaintMedium || len(n.Args) == 0 {
		return
	}
	if g, ok := a.Security.TaintReviewChecker.(protocol.TaintExemptionGranter); ok {
		g.Grant(vCtx.AgentID, n.Args, by)
	}
}

// noteAutoAllow 每次自动/规则放行都写审计，并更新规则命中计数。
func (a *Agent) noteAutoAllow(ctx context.Context, vCtx *protocol.DAGValidationContext, n protocol.ExecNode, d policy.Decision, mode types.PermissionMode) {
	if d.RuleID != "" && a.perm.rules != nil {
		if err := a.perm.rules.TouchRule(ctx, d.RuleID); err != nil {
			slog.WarnContext(ctx, "permission: touch approval rule failed", "rule", d.RuleID, "err", err)
		}
	}
	decision := "auto:" + d.Auto
	if d.RuleID != "" {
		decision = "rule:" + d.RuleID
	}
	a.auditPermission(ctx, vCtx, "permission_auto_allow", n, mode, decision)
}

func (a *Agent) auditPermission(ctx context.Context, vCtx *protocol.DAGValidationContext, event string, n protocol.ExecNode, mode types.PermissionMode, decision string) {
	if a.Security.TaintAuditor == nil {
		return
	}
	err := a.Security.TaintAuditor.Log(ctx, event, map[string]any{
		"tool":       n.ToolName,
		"decision":   decision,
		"mode":       string(mode),
		"node_id":    n.ID,
		"agent_id":   vCtx.AgentID,
		"session_id": vCtx.SessionID,
		"project_id": a.currentProjectID(),
	})
	if err != nil {
		slog.ErrorContext(ctx, "permission: audit write failed", "event", event, "tool", n.ToolName, "err", err)
	}
}

func denyError(n protocol.ExecNode, reason string) error {
	return &protocol.DAGValidationError{Layer: "L1_policy", NodeID: n.ID, Reason: reason}
}

// denyRepeated 同一调用本回合已被拒绝：不再询问，直接让模型改道（D6）。
func denyRepeated(tool string) string {
	return fmt.Sprintf("the user already declined this exact %q call in this turn; do not propose it again — explain to the user what you wanted to do and why, or pick a different approach", tool)
}

// denyReasonText 自保护路径的拒绝说明（D6：原因 + 替代方案）；不弹审批卡，任何模式都不能放开。
func denyReasonText(tool, reason string) string {
	return fmt.Sprintf("the %q call touches Polaris's own protected files (keys, config, kill switches or data directories) [%s]; this is denied in every permission mode and cannot be approved — use a different path, or tell the user what you needed so they can handle it outside the agent", tool, reason)
}
