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
//
// 2026-08-02 拆分说明（Test_inv_FileLineLimit R7 400 行上限存量债务，见
// local_playground/upgrade/99-new-findings.md 阶段03 R-07 发现，纯搬运无行为
// 变更）：本文件保留 Gate 核心结构与 IsAuthorized/Review 决策入口；出口污点
// 检查迁至 gate_egress.go；Cedar FFI 求值细节迁至 gate_cedar.go。
package policy

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/polarisagi/polaris/internal/observability/metrics"
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

// cedarLeakWindow Cedar FFI 泄漏计数的观察窗口。
// 语义：KillSwitch 关心的是「短时间内密集泄漏」（说明 Rust 侧真的卡死了），
// 而不是「进程跑了 30 天累计遇到 5 次偶发超时」。后者用固定阈值会造成
// 长时运行必然停服的可用性漏洞（阶段03 R-01，gemini-review Batch 2 #2）。
// 阈值 SSoT：docs/arch/spec/state.yaml（阶段06 登记）。
const cedarLeakWindow = 30 * time.Minute

// cedarLeakKillSwitchThreshold 窗口内累计泄漏数达到该值即触发 KillSwitch。
const cedarLeakKillSwitchThreshold = 5

type Gate struct {
	mu               sync.RWMutex
	cedarEnforceMode CedarEnforceMode
	forbidRules      []ForbidRule
	permitRules      []PermitRule
	consecutiveFail  atomic.Int64
	onKillSwitch     func() // 连续失败 10 次、或 cedarLeak 窗口内密集泄漏时触发

	// cedarLeakMu/cedarLeakTimes 保护窗口内泄漏时间戳（Unix 纳秒），实现无
	// 后台 goroutine 的惰性衰减：每次记录时顺带淘汰早于 now-cedarLeakWindow
	// 的记录。容量上限 = cedarLeakKillSwitchThreshold × 2，超限丢弃最旧。
	cedarLeakMu    sync.Mutex
	cedarLeakTimes []int64
	// cedarLeaksTotal 累计泄漏总量，只增不减，仅用于 Prometheus Gauge
	// polaris_cedar_ffi_leaks_total 的长期趋势观测，不参与 KillSwitch 判定
	// （避免"长期缓慢泄漏"被监控发现，但又不误触发短时窗口停服判定）。
	cedarLeaksTotal atomic.Int64

	cedar       *CedarEngine // Rust FFI 引擎
	evalTimeout time.Duration
}

// recordCedarLeak 记录一次 FFI 泄漏并返回窗口内的有效泄漏数。
// 淘汰早于 now-cedarLeakWindow 的记录，实现无后台 goroutine 的惰性衰减。
func (g *Gate) recordCedarLeak(now time.Time) int {
	g.cedarLeakMu.Lock()
	defer g.cedarLeakMu.Unlock()
	cutoff := now.Add(-cedarLeakWindow).UnixNano()
	kept := g.cedarLeakTimes[:0]
	for _, t := range g.cedarLeakTimes {
		if t >= cutoff {
			kept = append(kept, t)
		}
	}
	g.cedarLeakTimes = append(kept, now.UnixNano())
	if capLimit := cedarLeakKillSwitchThreshold * 2; len(g.cedarLeakTimes) > capLimit {
		g.cedarLeakTimes = g.cedarLeakTimes[len(g.cedarLeakTimes)-capLimit:]
	}
	g.cedarLeaksTotal.Add(1)
	metrics.GlobalCedarFFILeaksTotal.Store(g.cedarLeaksTotal.Load())
	return len(g.cedarLeakTimes)
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

// SetOnKillSwitch 允许在构造后覆盖 onKillSwitch 毁调（用于解决启动期循环依赖，如等待 HITL 网关就绪后注入）。
func (g *Gate) SetOnKillSwitch(fn func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.onKillSwitch = fn
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
	if g == nil {
		return false, apperr.New(apperr.CodeInternal, "policy: nil receiver")
	}
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
	if g == nil {
		return types.PolicyReviewResult{Allowed: false, Reason: "nil receiver"}, apperr.New(apperr.CodeInternal, "Gate.Review: nil receiver")
	}
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
	if n >= 10 {
		g.mu.RLock()
		onKS := g.onKillSwitch
		g.mu.RUnlock()
		if onKS != nil {
			onKS()
		}
	}
}

// ErrTaintBlockedEgress 实际阻断阈值为 TaintMedium 及以上（>= TaintMedium）。
// 与 SafeDialer.TaintEgressCheck 采用同一阈值，两层一致——见 M11 §6。
var ErrTaintBlockedEgress = apperr.New(apperr.CodeInternal, "policy: taint egress blocked (TaintMedium+ data cannot exit without sanitization)")
