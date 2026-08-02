package policy

// 2026-08-02 从 gate.go 拆分（Test_inv_FileLineLimit R7 400 行上限存量债务，见
// local_playground/upgrade/99-new-findings.md 阶段03 R-07 发现），纯搬运无行为变更。
// 本文件收敛 Cedar Rust FFI 求值细节（EntityUID 格式化 + 三档 enforce 语义）。

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/observability/metrics"
)

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

		// GD-2-001 修复：此前无论 cedarEnforceMode 取何值，Cedar 的 Allow 结果
		// 恒不作为终裁——只有 Deny 分支会在 EnforceDeny/EnforceFull 下立即返回，
		// Allow 分支永远 "falls through to go rules"，导致配置为 CedarEnforceFull
		// （"完全生效"）时与 CedarEnforceDeny 行为完全等价：Cedar Layer 3 Permit
		// 规则从未真正授予过任何权限，必须由 Go 兜底 permitRules 独立再次命中
		// 才能放行，Cedar 白名单形同虚设。这与双轨实现的设计意图矛盾——
		// configs/policy/soft_constraints.cedar（Layer 3）在 Cedar FFI 可用时
		// 应当是权威源，Go 规则只是 FFI 不可用时的降级兜底（本函数外层
		// `g.cedar != nil && g.cedar.PolicyCount() > 0` 已保证走到这里时 Cedar
		// 确实已加载策略）。
		//
		// 三档语义现在真正区分：
		//   - CedarEnforceFull : Cedar 结果（Allow 与 Deny）均直接终裁，不回退 Go 规则。
		//   - CedarEnforceDeny : 仅 Cedar 的 Deny 立即生效（新增 Forbid 可灰度先行）；
		//     Cedar 的 Allow 仍回退 Go permitRules 独立判定（迁移期放权更谨慎）。
		//   - CedarShadow      : 只记录，不影响终裁，始终回退 Go 规则。
		switch {
		case g.cedarEnforceMode == CedarEnforceFull:
			slog.DebugContext(ctx, "cedar evaluated (full enforce, authoritative)", "allowed", allowed, "reason", reason)
			return true, allowed, nil
		case !allowed && g.cedarEnforceMode == CedarEnforceDeny:
			slog.DebugContext(ctx, "cedar evaluated deny (enforced)", "reason", reason)
			return true, false, nil
		default:
			slog.DebugContext(ctx, "cedar evaluated (falling through to go rules)", "allowed", allowed, "reason", reason)
			return false, false, nil
		}
	}

	metrics.GlobalCedarDegradedTotal.Add(1)
	if strings.Contains(err.Error(), "timeout") {
		// R-01：窗口内密集泄漏才触发 KillSwitch，长期偶发泄漏只计入
		// cedarLeaksTotal（Prometheus 趋势观测），不再用全进程生命周期的
		// 只增计数误判为"持续故障"（见 gate.go 顶部 cedarLeakWindow 注释）。
		leaksInWindow := g.recordCedarLeak(time.Now())
		slog.WarnContext(ctx, "cedar ffi evaluate timed out, degrading to go rules", "error", err, "leaks_in_window", leaksInWindow, "cumulative_leaks_total", g.cedarLeaksTotal.Load())
		if leaksInWindow >= cedarLeakKillSwitchThreshold && g.onKillSwitch != nil {
			g.onKillSwitch()
		}
	} else {
		slog.WarnContext(ctx, "cedar ffi failed, degrading to go rules", "error", err)
	}
	return false, false, nil
}
