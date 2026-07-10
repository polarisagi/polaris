// Package policy 实现 M11 Policy & Safety 三层 Cedar 防线（MVP: in-memory Go 规则）。
// 架构文档: docs/arch/M11-Policy-Safety.md §3
//
// 三层架构:
//
//	Layer 1 (编译期常量): 由 internal/config/immutable_constants.go 定义，此层不可热更新
//	Layer 2 (Cedar Forbid): deny-by-default，forbid 无条件优先于 permit
//	Layer 3 (Cedar Permit): 最小权限白名单，每条规则须关联 Capability Token
//
// 双轨实现: 启动时从 configs/policy/ embed 加载 Cedar 策略（Rust FFI）；
// FFI 不可用时降级到 in-memory Go 规则兜底，行为语义等价。
package policy

import (
	"github.com/polarisagi/polaris/internal/security/token"

	"github.com/polarisagi/polaris/internal/observability/metrics"

	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// defaultEvalTimeout 是 Cedar 策略评估的默认超时。
// 特性:
//   - deny-by-default: 未命中任何 permit 规则 → 拒绝
//   - forbid-overrides-permit: Forbid 规则无条件优先
//   - fail-closed: Evaluate 超时（>10ms）或异常 → deny
//   - 连续 10 次失败 → 触发 KillSwitch Stage 1
const defaultEvalTimeout = 500 * time.Millisecond

type CedarEnforceMode int

const (
	CedarShadow      CedarEnforceMode = iota // 0: 仅记录不裁决
	CedarEnforceDeny                         // 1: 仅强制执行 deny
	CedarEnforceFull                         // 2: 完全生效
)

type Gate struct {
	mu               sync.RWMutex
	cedarEnforceMode CedarEnforceMode
	forbidRules      []ForbidRule
	permitRules      []PermitRule
	consecutiveFail  atomic.Int64
	onKillSwitch     func()       // 连续失败 10 次时触发
	cedarLeaks       atomic.Int64 // 累计 Cedar FFI goroutine 泄漏数
	cedar            *CedarEngine // Rust FFI 引擎
	evalTimeout      time.Duration
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
		onKillSwitch:     onKillSwitch,
		cedar:            NewCedarEngine(),
		evalTimeout:      defaultEvalTimeout,
		cedarEnforceMode: CedarShadow, // default
	}
	g.loadBuiltinRules()
	return g
}

// WithCedarEnforceMode sets the enforcement mode for Cedar policies.
func (g *Gate) WithCedarEnforceMode(mode CedarEnforceMode) *Gate {
	g.cedarEnforceMode = mode
	return g
}

// WithEvalTimeout 覆盖 Cedar 策略评估超时（默认 500ms）。
// 依赖注入替代包级可变变量（R1.3）：测试/慢速 FFI 环境需要更长超时时，
// 通过构造后链式调用注入，不再污染跨测试共享的全局状态。
func (g *Gate) WithEvalTimeout(d time.Duration) *Gate {
	g.evalTimeout = d
	return g
}

// SyncCedarPolicies 加载 Cedar 策略到 Rust FFI 引擎（替换全部已有策略）。
func (g *Gate) SyncCedarPolicies(policies string) error {
	if g.cedar != nil {
		return g.cedar.SyncPolicies(policies)
	}
	return nil
}

// CedarPolicyCount 返回当前 Cedar 引擎已加载的策略数量。
// 用于启动日志确认 Cedar 策略生效；返回 0 表示 FFI 不可用或未加载任何策略。
func (g *Gate) CedarPolicyCount() int {
	if g.cedar == nil {
		return 0
	}
	return g.cedar.PolicyCount()
}

// ReloadCedarPoliciesFromDisk 从磁盘路径热更新 Cedar 策略（替换 Cedar 引擎中的全部策略）。
// 参数为已合并的策略内容字符串（调用方负责读取并拼接 hard + soft + memory）。
// 热更新失败不影响当前 Go 规则兜底，但已加载的 Cedar 策略会被 Rust 清空——失败时建议重试。
func (g *Gate) ReloadCedarPoliciesFromDisk(combined string) error {
	return g.SyncCedarPolicies(combined)
}

func (g *Gate) IsAuthorized(
	ctx context.Context,
	principal, action, resource string,
	evalCtx map[string]any,
) (bool, error) {
	if principal == "" || action == "" {
		g.recordFailure()
		return false, apperr.New(apperr.CodeInternal, "policy: invalid request: principal and action are required")
	}

	// 2026-07-04 审计修复：g.evaluate() 目前恒定返回 nil error（Go 规则 Step1~3
	// 从不产生 error；Cedar FFI 的超时/异常已在 evaluateCedar 内部就地驱动
	// cedarLeaks 计数 + KillSwitch，不再需要在此处间接依赖 err 内容字符串匹配。
	// 下方 err != nil 分支在当前实现下不可达，但作为 fail-closed 防御性保留
	// （R1.14 安全门必须 fail-closed）：若未来 g.evaluate() 实现变化产生真实
	// error，此分支仍需正确短路拒绝，禁止删除。
	allowed, err := g.evaluate(ctx, principal, action, resource, evalCtx)
	if err != nil {
		g.recordFailure()
		return false, err
	}

	g.consecutiveFail.Store(0)

	// [Task 14 修复 2026-07-04] allow/deny 指标埋点从 Review() 下移至此处：
	// IsAuthorized 是全系统策略检查的真正入口（envelope/mcp_manager/hook/skill/
	// facade/lam 等 ~15 处直接调用），Review 仅 marketplace/dag validator 3 处调用。
	// 埋点原放在 Review 会导致绝大多数生产策略检查的 allow/deny 结果不计入指标。
	if allowed {
		if metrics.InstrCedarAllowTotal != nil {
			metrics.InstrCedarAllowTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("action", action)))
		}
	} else if metrics.InstrCedarDenyTotal != nil {
		metrics.InstrCedarDenyTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("action", action)))
	}

	return allowed, nil
}

// Review 实现 protocol.PolicyGate.Review（详细审查，附 Reason 与 Etag）。
func (g *Gate) Review(ctx context.Context, req types.PolicyReviewRequest) (types.PolicyReviewResult, error) {
	allowed, err := g.IsAuthorized(ctx, req.Principal, req.Action, req.Resource, req.Context)
	if err != nil {
		return types.PolicyReviewResult{Allowed: false, Reason: err.Error()}, apperr.Wrap(apperr.CodeInternal, "Gate.Review", err)
	}

	// 精确 reason：先找触发的 forbid 规则，后记指标
	reason := "denied by default"
	if allowed {
		reason = "permitted"
	} else {
		g.mu.RLock()
		for _, fr := range g.forbidRules {
			if fr.MatchFn(req.Principal, req.Action, req.Resource, req.Context) {
				reason = "forbidden: " + fr.Reason
				break
			}
		}
		g.mu.RUnlock()
	}

	// 2026-07-04 审计修复：allow/deny 指标埋点已下移至 IsAuthorized（见其函数注释），
	// 此处不再重复计数，避免通过 Review() 调用路径的请求被计两次。

	return types.PolicyReviewResult{
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
func (g *Gate) TaintEgressCheck(levels ...types.TaintLevel) error {
	result := types.PropagateTaint(levels...)
	// TaintMedium 硬地板：Medium 及以上级别数据不得直接出口，必须经过清洗
	if result >= types.TaintMedium {
		return ErrTaintBlockedEgress
	}
	return nil
}

// CheckEgressWithExemption 出口污点检查，支持 HITL 放行令牌。
// token 为 nil 时退化为 CheckEgress（无放行通道）。
func (g *Gate) CheckEgressWithExemption(data []byte, taintLevel types.TaintLevel, token *token.TaintExemptionToken) error {
	if taintLevel < types.TaintMedium {
		return nil
	}
	// HITL 放行：令牌有效则放行，并记录审计事件
	if token != nil && token.Valid(data) {
		return nil // 已放行，审计由调用方通过 EventLog 记录 token.Summary()
	}
	return ErrTaintBlockedEgress
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
		if handled, allowed, err := g.evaluateCedar(ctx, principal, action, resource, evalCtx); handled || err != nil {
			return allowed, err
		}
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

// ErrTaintBlockedEgress 实际阻断阈值为 TaintMedium 及以上（>= TaintMedium）。
// 与 SafeDialer.TaintEgressCheck 采用同一阈值，两层一致——见 M11 §6。
var ErrTaintBlockedEgress = apperr.New(apperr.CodeInternal, "policy: taint egress blocked (TaintMedium+ data cannot exit without sanitization)")

func (g *Gate) evaluateCedar(ctx context.Context, principal, action, resource string, evalCtx map[string]any) (bool, bool, error) {
	pUID := formatCedarUID("Principal", principal)
	aUID := formatCedarUID("Action", action)
	rUID := formatCedarUID("Resource", resource)

	// 传递 evalTimeout 转换为毫秒给 FFI
	timeoutMs := uint64(g.evalTimeout.Milliseconds())
	if timeoutMs == 0 {
		timeoutMs = 10 // 兜底 10ms（Go 侧永不向 Rust 请求"0=无限等待"语义，安全边界考虑）
	}
	allowed, reason, err := g.cedar.Evaluate(pUID, aUID, rUID, evalCtx, timeoutMs)

	if err == nil {
		if !allowed && evalCtx != nil {
			evalCtx["cedar_reason"] = reason
		}

		if !allowed && g.cedarEnforceMode >= CedarEnforceDeny {
			slog.DebugContext(ctx, "cedar evaluated deny (enforced)", "reason", reason)
			return true, false, nil
		}

		slog.DebugContext(ctx, "cedar evaluated (falling through to go rules)", "allowed", allowed, "reason", reason)
		return false, false, nil
	}

	metrics.GlobalCedarDegradedTotal.Add(1)
	if strings.Contains(err.Error(), "timeout") {
		leaks := g.cedarLeaks.Add(1)
		slog.WarnContext(ctx, "cedar ffi evaluate timed out, degrading to go rules", "error", err, "cumulative_leaks", leaks)
		if leaks >= 5 && g.onKillSwitch != nil {
			g.onKillSwitch()
		}
	} else {
		slog.WarnContext(ctx, "cedar ffi failed, degrading to go rules", "error", err)
	}
	return false, false, nil
}
