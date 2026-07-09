package policy

import (
	"strings"

	"github.com/polarisagi/polaris/pkg/types"
)

// loadBuiltinRules 加载编译期内置的 Forbid/Permit 规则（Go 兜底层）。
// Cedar FFI 可用时由 configs/policy/hard_constraints.cedar 替代；此处为降级保障。
func (g *Gate) loadBuiltinRules() { //nolint:gocyclo
	// Layer 2 — Forbid 规则（不可热更新，对应 configs/policy/hard_constraints.cedar）
	g.forbidRules = []ForbidRule{
		{
			Name:   "audit_log_always_on",
			Reason: "L4 编译期不变量: 审计日志不可关闭（g_inv_01）",
			MatchFn: func(_, action, resource string, _ map[string]any) bool {
				// 任何试图关闭 audit_log 的操作 → Forbid
				return (action == "disable" || action == "delete") &&
					strings.Contains(resource, "audit_log")
			},
		},
		{
			Name:   "self_modification_guard",
			Reason: "L4 编译期不变量: 禁止 AI 自我修改二进制（g_inv_02）",
			MatchFn: func(principal, action, resource string, _ map[string]any) bool {
				if action != "write" && action != "execute" {
					return false
				}
				// 试图写入/执行自身二进制路径 → Forbid
				return strings.Contains(resource, "polaris") &&
					(strings.HasSuffix(resource, ".bin") || strings.HasSuffix(resource, "main"))
			},
		},
		{
			Name:   "kill_switch_immutable",
			Reason: "KillSwitch 触发后禁止任何非 DRAIN 操作",
			MatchFn: func(_, action, _ string, ctx map[string]any) bool {
				if ks, ok := ctx["kill_switch_active"].(bool); ok && ks {
					return action != "drain" && action != "read" && action != "health_check"
				}
				return false
			},
		},
		{
			Name:   "privileged_action_requires_approval",
			Reason: "高风险操作（delete_data/deploy）必须携带 approval_status=approved",
			MatchFn: func(_, action, _ string, ctx map[string]any) bool {
				if action != "delete_data" && action != "deploy_to_production" {
					return false
				}
				status, ok := ctx["approval_status"].(string)
				return !ok || status != "approved"
			},
		},
		{
			Name:   "budget_cap",
			Reason: "月度 token 预算耗尽时禁止推理请求",
			MatchFn: func(_, action, _ string, ctx map[string]any) bool {
				budgetedActions := map[string]bool{
					"infer":        true,
					"stream_infer": true,
					"tool_call":    true,
					"mcp_call":     true,
					"http_request": true,
					"browse":       true,
				}
				if !budgetedActions[action] {
					return false
				}
				spend, ok1 := ctx["monthly_spend_usd"].(float64)
				budget, ok2 := ctx["monthly_budget_usd"].(float64)
				return ok1 && ok2 && spend >= budget
			},
		},
		{
			Name:   "forbid_external_comm_tainted",
			Reason: "外发通信要求 TaintLevel<2 或已获 HITL 审批",
			MatchFn: func(_, action, _ string, ctx map[string]any) bool {
				if action != "send_external_communication" {
					return false
				}

				tl := 0
				if v, ok := ctx["taint_level"].(float64); ok {
					tl = int(v)
				} else if v, ok := ctx["taint_level"].(int); ok {
					tl = v
				} else if v, ok := ctx["taint_level"].(types.TaintLevel); ok {
					tl = int(v)
				}

				approvalStatus, _ := ctx["approval_status"].(string)
				return tl >= 2 && approvalStatus != "approved"
			},
		},
		{
			Name:   "forbid_financial_transaction_unapproved",
			Reason: "金融交易必须经 HITL 显式审批",
			MatchFn: func(_, action, _ string, ctx map[string]any) bool {
				if action != "financial_transaction" {
					return false
				}
				approvalStatus, _ := ctx["approval_status"].(string)
				return approvalStatus != "approved"
			},
		},
		{
			Name:   "holdout_set_read_isolation",
			Reason: "M12 评估 Holdout Set 必须与 Agent 读取路径隔离（staging_inv_03）",
			MatchFn: func(principal, action, resource string, ctx map[string]any) bool {
				if action != "read_local" && action != "read_file" {
					return false
				}
				// ci_gate 角色不受此规则限制（CI/Canary 需要读取 Holdout Set）
				if principal == "ci_gate" {
					return false
				}
				holdoutPath, ok := ctx["polaris_eval_holdout_path"].(string)
				if !ok || holdoutPath == "" {
					return false
				}
				return strings.HasPrefix(resource, holdoutPath)
			},
		},
		{
			Name:   "llm_generated_privileged",
			Reason: "LLM 生成代码不得执行特权操作（网络写入/部署）（Layer 2 forbid）",
			MatchFn: func(_, action, _ string, ctx map[string]any) bool {
				source, _ := ctx["source"].(string)
				if source != "llm_generated" {
					return false
				}
				// write_network 和 deploy 是特权操作，不允许 LLM 生成代码直接触发
				return action == "write_network" || action == "deploy_to_production"
			},
		},
		{
			Name:   "delegation_chain_depth",
			Reason: "跨 Agent 委托链深度 ≥ 3 → deny（Layer 4 多 Agent 治理规则）",
			MatchFn: func(_, action, _ string, ctx map[string]any) bool {
				if action != "delegate_task" {
					return false
				}
				depth, _ := ctx["delegation_chain_depth"].(float64)
				return depth >= 3
			},
		},
		{
			Name:   "install_untrusted_forbid",
			Reason: "Tier 0 (Untrusted) extensions are strictly forbidden to install",
			MatchFn: func(_, action, _ string, ctx map[string]any) bool {
				if action != "install_extension" {
					return false
				}
				return trustLevel(ctx) == 0 // Untrusted
			},
		},
	}

	// Layer 3 — Permit 规则（对应 configs/policy/soft_constraints.cedar，可热更新）
	g.permitRules = []PermitRule{
		{
			Name: "read_local_trusted",
			MatchFn: func(_, action, resource string, ctx map[string]any) bool {
				if action != "read_local" && action != "read_file" {
					return false
				}
				return trustLevel(ctx) >= 1
			},
		},
		{
			Name: "write_local_trusted",
			MatchFn: func(_, action, resource string, ctx map[string]any) bool {
				if action != "write_local" && action != "write_file" {
					return false
				}
				return trustLevel(ctx) >= 2
			},
		},
		{
			Name: "network_dial_with_capability",
			MatchFn: func(_, action, _ string, ctx map[string]any) bool {
				if action != "network_dial" {
					return false
				}
				valid, _ := ctx["capability_token_valid"].(bool)
				return trustLevel(ctx) >= 3 && valid
			},
		},
		{
			Name: "infer_standard",
			MatchFn: func(principal, action, _ string, ctx map[string]any) bool {
				if action != "infer" && action != "stream_infer" {
					return false
				}
				return trustLevel(ctx) >= 1 && principal != ""
			},
		},
		{
			Name: "install_extension_permit",
			MatchFn: func(_, action, _ string, ctx map[string]any) bool {
				if action != "install_extension" {
					return false
				}

				pmodeStr, _ := ctx["permission_mode"].(string)
				pmode := types.PermissionMode(pmodeStr)
				if pmode == "" {
					pmode = types.ModeAutoReview
				}

				tl := trustLevel(ctx)
				extType, _ := ctx["ext_type"].(string)
				hasHooks, _ := ctx["has_hooks"].(bool)

				// Community plugin with hooks require HITL -> never auto approve
				if extType == "plugin" && hasHooks && tl < 3 {
					return false
				}

				if pmode == types.ModeFullAccess && tl >= 2 {
					return true
				}
				if pmode == types.ModeAutoReview && tl >= 2 {
					return true
				}
				if pmode == types.ModeDefault && tl >= 3 {
					return true
				}

				// TrustSystem (4) is always allowed
				return tl >= 4
			},
		},
		{
			Name: "write_network_permit",
			MatchFn: func(_, action, _ string, ctx map[string]any) bool {
				if action != "write_network" {
					return false
				}

				pmodeStr, _ := ctx["permission_mode"].(string)
				pmode := types.PermissionMode(pmodeStr)
				if pmode == "" {
					pmode = types.ModeAutoReview
				}

				tl := trustLevel(ctx)
				approval, _ := ctx["approval_status"].(string)

				if tl >= 4 {
					return true
				}

				//nolint:nestif
				switch pmode {
				case types.ModeFullAccess:
					if tl >= 2 {
						return true
					}
				case types.ModeAutoReview:
					if tl >= 3 {
						return true
					}
					if tl == 2 && approval == "approved" {
						return true
					}
				case types.ModeDefault:
					if tl >= 3 {
						return approval == "approved"
					}
					if approval == "approved" {
						return true
					}
				}

				return false
			},
		},
		// ── ExecEnvelope 六路执行：Go 兜底 permit（与 soft_constraints.cedar 等价）──
		{
			Name: "tool_execute_permit",
			MatchFn: func(_, action, _ string, ctx map[string]any) bool {
				if action != "tool_execute" {
					return false
				}
				// 内置/官方（trust>=3）直放；其余需有效能力令牌（与 Cedar capability 条件等价）
				if trustTierOf(ctx) >= 3 {
					return true
				}
				valid, _ := ctx["capability_token_valid"].(bool)
				return valid
			},
		},
		{
			Name: "process_spawn_permit",
			MatchFn: func(principal, action, _ string, ctx map[string]any) bool {
				if action != "process_spawn" || principal != "mcp_mgr" {
					return false
				}
				tt := trustTierOf(ctx)
				if tt >= 3 {
					return true
				}
				auto, _ := ctx["sandbox_auto"].(bool)
				return tt == 2 && auto
			},
		},
		{
			Name: "script_execute_permit",
			MatchFn: func(_, action, _ string, ctx map[string]any) bool {
				if action != "script_execute" {
					return false
				}
				if trustTierOf(ctx) >= 1 {
					return true
				}
				src, _ := ctx["tool_source"].(string)
				return src == "llm_generated" // CodeAct(Untrusted) 显式放行；隔离 L2 + govAgent 为边界
			},
		},
		{
			Name:    "hook_execute_permit",
			MatchFn: func(_, action, _ string, _ map[string]any) bool { return action == "hook_execute" },
		},
		{
			Name: "browser_automate_permit",
			MatchFn: func(_, action, resource string, ctx map[string]any) bool {
				if action != "browser_automate" || resource != "lam" {
					return false
				}
				net, _ := ctx["allow_net"].(bool)
				return net
			},
		},
	}
}

func trustLevel(ctx map[string]any) int {
	if v, ok := ctx["trust_level"].(float64); ok {
		return int(v)
	}
	if v, ok := ctx["trust_level"].(int); ok {
		return v
	}
	return 0
}

func trustTierOf(ctx map[string]any) int {
	switch v := ctx["trust_tier"].(type) {
	case int:
		return v
	case float64:
		return int(v)
	case types.TrustTier:
		return int(v)
	default:
		return 0 // 缺省 Untrusted（最严）
	}
}
