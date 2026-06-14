// Package policy 实现 M11 Policy & Safety 三层 Cedar 防线（MVP: in-memory Go 规则）。
// 架构文档: docs/arch/M11-Policy-Safety.md §3
//
// 三层架构:
//
//	Layer 1 (编译期常量): 由 internal/config/immutable_constants.go 定义，此层不可热更新
//	Layer 2 (Cedar Forbid): deny-by-default，forbid 无条件优先于 permit
//	Layer 3 (Cedar Permit): 最小权限白名单，每条规则须关联 Capability Token
//
// MVP 实现: in-memory Go 规则引擎，接口与 Cedar Rust FFI 对齐；
// Cedar FFI 集成列为 Tier 1+ 待办（pkg/substrate/policy/ 目录已预留）。
package policy

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/substrate/observability"
)

// Gate 是 PolicyGate 的 substrate 层实现。
// 特性:
//   - deny-by-default: 未命中任何 permit 规则 → 拒绝
//   - forbid-overrides-permit: Forbid 规则无条件优先
//   - fail-closed: Evaluate 超时（>10ms）或异常 → deny
//   - 连续 10 次失败 → 触发 KillSwitch Stage 1
type Gate struct {
	mu              sync.RWMutex
	forbidRules     []ForbidRule
	permitRules     []PermitRule
	consecutiveFail atomic.Int64
	onKillSwitch    func()       // 连续失败 10 次时触发
	cedarLeaks      atomic.Int64 // 累计 Cedar FFI goroutine 泄漏数
	cedar           *CedarEngine // Rust FFI 引擎
}

var _ protocol.PolicyGate = (*Gate)(nil)

// ForbidRule 表示 Layer 2 的强制拒绝规则。
type ForbidRule struct {
	Name    string
	MatchFn func(principal, action, resource string, ctx map[string]any) bool
	Reason  string
}

// PermitRule 表示 Layer 3 的条件许可规则。
type PermitRule struct {
	Name    string
	MatchFn func(principal, action, resource string, ctx map[string]any) bool
}

// NewGate 创建默认策略门，加载内置不可变规则。
// onKillSwitch 在连续 10 次评估失败时调用（可为 nil）。
func NewGate(onKillSwitch func()) *Gate {
	g := &Gate{
		onKillSwitch: onKillSwitch,
		cedar:        NewCedarEngine(),
	}
	g.loadBuiltinRules()
	return g
}

// LoadCedarPolicies 加载 Cedar 策略到 Rust FFI 引擎。
func (g *Gate) LoadCedarPolicies(policies string) error {
	if g.cedar != nil {
		return g.cedar.LoadPolicies(policies)
	}
	return nil
}

// loadBuiltinRules 加载编译期内置的 Forbid/Permit 规则。
// 对应 policies/hard_constraints.cedar（MVP 期间以 Go 规则等价实现）。
func (g *Gate) loadBuiltinRules() { //nolint:gocyclo
	// Layer 2 — Forbid 规则（不可热更新，对应 policies/hard_constraints.cedar）
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
				if action != "infer" && action != "stream_infer" {
					return false
				}
				spend, ok1 := ctx["monthly_spend_usd"].(float64)
				budget, ok2 := ctx["monthly_budget_usd"].(float64)
				return ok1 && ok2 && spend >= budget
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

	// Layer 3 — Permit 规则（对应 policies/soft_constraints.cedar，可热更新）
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
				pmode := protocol.PermissionMode(pmodeStr)
				if pmode == "" {
					pmode = protocol.ModeAutoReview
				}

				tl := trustLevel(ctx)
				extType, _ := ctx["ext_type"].(string)
				hasHooks, _ := ctx["has_hooks"].(bool)

				// Community plugin with hooks require HITL -> never auto approve
				if extType == "plugin" && hasHooks && tl < 3 {
					return false
				}

				if pmode == protocol.ModeFullAccess && tl >= 2 {
					return true
				}
				if pmode == protocol.ModeAutoReview && tl >= 2 {
					return true
				}
				if pmode == protocol.ModeDefault && tl >= 3 {
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
				pmode := protocol.PermissionMode(pmodeStr)
				if pmode == "" {
					pmode = protocol.ModeAutoReview
				}

				tl := trustLevel(ctx)
				approval, _ := ctx["approval_status"].(string)

				if tl >= 4 {
					return true
				}

				//nolint:nestif
				if pmode == protocol.ModeFullAccess {
					if tl >= 2 {
						return true
					}
				} else if pmode == protocol.ModeAutoReview {
					if tl >= 3 {
						return true
					}
					if tl == 2 && approval == "approved" {
						return true
					}
				} else if pmode == protocol.ModeDefault {
					if tl >= 3 {
						return true
					}
					if approval == "approved" {
						return true
					}
				}

				return false
			},
		},
	}
}

// IsAuthorized 执行三层策略评估（超时 >10ms → deny）。
func (g *Gate) IsAuthorized(
	ctx context.Context,
	principal, action, resource string,
	evalCtx map[string]any,
) (bool, error) {
	if principal == "" || action == "" {
		g.recordFailure()
		return false, perrors.New(perrors.CodeInternal, "policy: invalid request: principal and action are required")
	}

	// 超时门控：>10ms → deny + 计数
	type result struct {
		allowed bool
		err     error
	}
	ch := make(chan result, 1)
	go func() {
		allowed, err := g.evaluate(ctx, principal, action, resource, evalCtx)
		ch <- result{allowed, err}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			g.recordFailure()
			return false, r.err
		}
		g.consecutiveFail.Store(0)
		return r.allowed, nil
	case <-time.After(10 * time.Millisecond):
		// 超时 → fail-closed
		g.recordFailure()
		leaks := g.cedarLeaks.Add(1)
		if leaks >= 5 && g.onKillSwitch != nil {
			g.onKillSwitch()
		}
		observability.GlobalCedarDegradedTotal.Add(1)
		return false, perrors.New(perrors.CodeInternal, "policy: evaluation timeout (>10ms)")
	case <-ctx.Done():
		g.recordFailure()
		return false, ctx.Err()
	}
}

// Review 实现 protocol.PolicyGate.Review（详细审查，附 Reason 与 Etag）。
func (g *Gate) Review(ctx context.Context, req protocol.PolicyReviewRequest) (protocol.PolicyReviewResult, error) {
	allowed, err := g.IsAuthorized(ctx, req.Principal, req.Action, req.Resource, req.Context)
	if err != nil {
		return protocol.PolicyReviewResult{Allowed: false, Reason: err.Error()}, err
	}

	reason := "denied by default"
	if allowed {
		reason = "permitted"
	} else {
		// 精确 reason：找到触发的 forbid 规则
		g.mu.RLock()
		for _, fr := range g.forbidRules {
			if fr.MatchFn(req.Principal, req.Action, req.Resource, req.Context) {
				reason = "forbidden: " + fr.Reason
				break
			}
		}
		g.mu.RUnlock()
	}

	return protocol.PolicyReviewResult{
		Allowed: allowed,
		Reason:  reason,
		Etag:    fmt.Sprintf("%d", time.Now().UnixNano()),
	}, nil
}

// formatCedarUID 确保输入符合 Cedar EntityUID 格式 (Type::"ID")。
func formatCedarUID(defaultType, val string) string {
	if val == "" {
		return defaultType + `::"anonymous"`
	}
	if strings.Contains(val, `::"`) {
		return val
	}
	// 转义双引号
	escaped := strings.ReplaceAll(val, `"`, `\"`)
	return defaultType + `::"` + escaped + `"`
}

// TaintEgressCheck 检查 Taint 出口：TaintMedium 级别数据不可直接输出到外部接口。
// 违反 → ErrTaintBlockedEgress（对应 M11 §2.3 SanitizeBySchema 规则）。
func (g *Gate) TaintEgressCheck(levels ...protocol.TaintLevel) error {
	result := protocol.PropagateTaint(levels...)
	// TaintMedium 硬地板：Medium 及以上级别数据不得直接出口，必须经过清洗
	if result >= protocol.TaintMedium {
		return ErrTaintBlockedEgress
	}
	return nil
}

// AddForbidRule 热更新添加 Forbid 规则（仅限 Layer 3 策略热更新；Layer 1/2 内置规则不可删除）。
func (g *Gate) AddForbidRule(r ForbidRule) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.forbidRules = append(g.forbidRules, r)
}

// AddPermitRule 热更新添加 Permit 规则。
func (g *Gate) AddPermitRule(r PermitRule) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.permitRules = append(g.permitRules, r)
}

// evaluate 执行实际策略评估（在 goroutine 内调用以支持超时）。
func (g *Gate) evaluate(ctx context.Context, principal, action, resource string, evalCtx map[string]any) (bool, error) {
	// Step 0: 如果 Cedar 引擎加载了策略，优先通过 Rust FFI 评估
	if g.cedar != nil && g.cedar.PolicyCount() > 0 {
		pUID := formatCedarUID("Principal", principal)
		aUID := formatCedarUID("Action", action)
		rUID := formatCedarUID("Resource", resource)

		allowed, reason, err := g.cedar.Evaluate(pUID, aUID, rUID, evalCtx)
		// 如果 Cedar 评估成功且未抛出 FFI 层级的异常，则直接返回其结果
		if err == nil {
			// 将 Cedar reason 注入 ctx（或者只是为了区分）
			if !allowed && evalCtx != nil {
				evalCtx["cedar_reason"] = reason
			}
			return allowed, nil
		}
		// 若 Cedar 失败 (如 JSON marshal 错误)，降级到 Go 兜底规则
		observability.GlobalCedarDegradedTotal.Add(1)
		slog.WarnContext(ctx, "cedar ffi failed, degrading to go rules", "error", err)
	}

	g.mu.RLock()
	defer g.mu.RUnlock()

	// Step 1: Forbid 规则优先（任意一条命中 → deny）
	for _, fr := range g.forbidRules {
		if fr.MatchFn(principal, action, resource, evalCtx) {
			return false, nil
		}
	}

	// Step 2: Permit 规则（任意一条命中 → allow）
	for _, pr := range g.permitRules {
		if pr.MatchFn(principal, action, resource, evalCtx) {
			return true, nil
		}
	}

	// Step 3: deny-by-default
	return false, nil
}

func (g *Gate) recordFailure() {
	n := g.consecutiveFail.Add(1)
	if n >= 10 && g.onKillSwitch != nil {
		g.onKillSwitch()
	}
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func trustLevel(ctx map[string]any) int {
	if v, ok := ctx["trust_level"].(float64); ok {
		return int(v)
	}
	if v, ok := ctx["trust_level"].(int); ok {
		return v
	}
	return 0
}

// ErrTaintBlockedEgress 实际阻断阈值为 TaintMedium 及以上（>= TaintMedium）。
// 与 SafeDialer.TaintEgressCheck 采用同一阈值，两层一致——见 M11 §6。
var ErrTaintBlockedEgress = perrors.New(perrors.CodeInternal, "policy: taint egress blocked (TaintMedium+ data cannot exit without sanitization)")
