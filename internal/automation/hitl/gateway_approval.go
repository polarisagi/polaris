package hitl

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/security/token"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// 批准路径的前置校验与豁免令牌铸造（R7 拆分自 gateway.go）。
// Respond 主流程见 gateway.go；L3 回归门禁见 gateway_l3gate.go。

// applyApprovalGuards 批准路径的前置校验：强制冷却期 + TaintExemptionToken 铸造。
//
// **读取/解析失败一律 fail-closed 拒绝批准**（2026-08-06 修复）：
// 此前是 `if err == nil { if json.Unmarshal(...) == nil { ... } }`，两层条件把
// 两类失败都吞掉，后果不是"少做一步"而是**安全校验被整体跳过**——
//   - 冷却期校验被跳过 → 审批人可在强制冷却期内批准，"必须先读完影子回归
//     报告"这条门禁形同虚设（Task 21 的全部意义就在这个等待上）；
//   - 豁免令牌不铸造 → M04 §3 出口污点转义路径失效，审批通过了但下一次重试
//     仍撞同一个拦截。
//
// 读不到 pending 记录时拒绝而非放行，符合 HE-2：无法验证的前置条件必须
// 当作未满足处理。若确因超时导致 pending 已被清理，调用方会在后续
// "no active waiter" 分支得到明确错误，而不是拿到一个绕过冷却的批准。
func (g *GatewayImpl) applyApprovalGuards(
	ctx context.Context, key []byte, checkpointID string, response types.HITLResponse,
) error {
	data, err := g.store.Get(ctx, key)
	if err != nil {
		slog.ErrorContext(ctx, "hitl_gateway: cannot read pending record, refusing approval (fail-closed)",
			"checkpoint", checkpointID, "err", err)
		return apperr.Wrap(apperr.CodeForbidden,
			"hitl_gateway: cannot verify approval preconditions (pending record unreadable)", err)
	}
	var p types.HITLPrompt
	if umErr := json.Unmarshal(data, &p); umErr != nil {
		slog.ErrorContext(ctx, "hitl_gateway: pending record corrupted, refusing approval (fail-closed)",
			"checkpoint", checkpointID, "err", umErr)
		return apperr.Wrap(apperr.CodeForbidden,
			"hitl_gateway: cannot verify approval preconditions (pending record corrupted)", umErr)
	}

	// Task 21: 强制冷却期——未到 EligibleApproveTime 一律拒绝。
	if p.EligibleApproveTime > 0 && time.Now().Unix() < p.EligibleApproveTime {
		return apperr.New(apperr.CodeForbidden,
			"hitl_gateway: mandatory cooldown active, please carefully read the shadow regression report before approving")
	}

	g.mintExemptionToken(ctx, p, checkpointID, response)
	return nil
}

// mintExemptionToken 人工批准出口污点拦截后铸造 TaintExemptionToken（M04 §3 转义路径）。
//
// fail-closed 而非 best-effort：ExemptionFieldContent 为空（发起侧未能从错误链
// 取出被拦截数据，或该 checkpoint 根本不是出口污点转义场景）或未注入
// exemptionVault 时，明确跳过铸造并记录原因——不铸造一个内容为空、
// Valid() 对任意 data 都可能误判通过的令牌。
func (g *GatewayImpl) mintExemptionToken(
	ctx context.Context, p types.HITLPrompt, checkpointID string, response types.HITLResponse,
) {
	switch {
	case p.TaintLevel <= 0:
		// 非出口污点转义场景（其余 HITL checkpoint 类型），无需铸造。
	case len(p.ExemptionFieldContent) == 0:
		slog.WarnContext(ctx, "hitl_gateway: approved high-taint checkpoint has empty ExemptionFieldContent, skipping token mint (fail-closed)",
			"checkpoint", checkpointID, "checkpoint_type", p.CheckpointType)
	case g.exemptionVault == nil:
		slog.WarnContext(ctx, "hitl_gateway: exemptionVault not configured, TaintExemptionToken not stored, next retry will not find it",
			"checkpoint", checkpointID)
	case p.AgentID == "":
		slog.WarnContext(ctx, "hitl_gateway: approved high-taint checkpoint has empty AgentID, cannot key exemption vault, skipping mint (fail-closed)",
			"checkpoint", checkpointID)
	default:
		tok := token.NewTaintExemptionToken(p.ExemptionFieldContent, taintExemptionTokenTTL, response.UserID)
		g.exemptionVault.Store(p.AgentID, tok)
		slog.InfoContext(ctx, "hitl_gateway: minted and stored TaintExemptionToken for approved high-taint operation",
			"checkpoint", checkpointID, "agent_id", p.AgentID, "summary", tok.Summary())
	}
}
