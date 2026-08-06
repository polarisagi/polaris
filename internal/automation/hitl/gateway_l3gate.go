package hitl

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/types"
)

// applyL3RegressionGate 对 L4 自我改进候选执行 L3 全量回归门禁（R7 拆分自 Prompt）。
//
// 返回 (resp != nil) 表示已就地作出终态决策（P0 回归失败自动拒绝），
// 调用方应直接返回该结果，不再进入 pending/waiter 流程。
// 返回 nil 表示门禁未触发或已通过，调用方继续正常审批流程；
// 此时 p 可能已被就地修改（追加影子回归报告到 PromptText、设置强制冷却期）。
//
// 触发条件收窄为 CheckpointType == "l4_multi_sig"（Task 21+22，2026-07-04 审计）：
// 此前用 RiskLevel >= 3 会误捕获所有高风险 HITL 请求（code_act_warning、
// security_review、logic_collapse_high_risk 等，见 internal/action/codeact、
// internal/extension/native/extension_manager.go、cmd/polaris/adapters_misc.go），
// 给这些与"L4 晋升"无关的审批强加 P0/P1 全量回归 + 强制冷却，阻塞正常的
// 工具执行/扩展安装审批。L4 自我改进晋升是本门禁的唯一设计目标
// （见 internal/learning/engine.go detectL4Trigger）。
func (g *GatewayImpl) applyL3RegressionGate(ctx context.Context, p *types.HITLPrompt) *types.HITLResponse {
	if p.CheckpointType != "l4_multi_sig" || g.evalRunner == nil || g.regression == nil {
		return nil
	}
	slog.Info("hitl_gateway: triggering L3 full regression", "checkpoint", p.ID)

	// TODO(GR-10-002): "regression_p0_p1" partition 当前在 EvalStore/control.Engine
	// 体系中尚无完整数据面支撑，RunSuite 调用会返回 "unknown suite" 错误。
	// 过渡方案：暂用 "validation" suite 作为等价门禁，直到 regression_p0_p1
	// 分区的数据写入路径完成接线后再切换回来（参考 ADR-0048 待补充决策）。
	report, err := g.evalRunner.RunSuite(context.Background(), "validation", "")
	if err != nil {
		// L3 回归门禁失败**不能静默**（此前 `if err == nil` 直接把错误吞掉，
		// 上面 TODO 描述的 "unknown suite" 就是这样长期无人察觉的）。
		// 刻意不 fail-closed 直接拒绝：门禁不可用属于运维故障而非候选补丁有问题，
		// 拒绝会让 L4 自我改进晋升整体卡死；降级为"跳过回归、转入正常人工审批"，
		// 并留下 Error 级痕迹供运维发现。
		slog.Error("hitl_gateway: L3 regression suite failed, falling back to plain human review",
			"checkpoint", p.ID, "err", err)
		return nil
	}
	if report == nil {
		return nil
	}

	if report.P0Fail > 0 {
		return g.autoDenyOnP0Regression(ctx, p)
	}

	g.attachShadowReport(ctx, p)
	g.applyMandatoryCooldown(p)
	return nil
}

// autoDenyOnP0Regression P0 回归失败时就地拒绝并归档。
//
// 刻意不经过 Respond（GR-10-001 修复）：此刻 pending 尚未写入 store、waiter
// 尚未注册，Respond 会因 "no active waiter" 报错，归档与清理都不会发生。
// 归档记录仍需落盘以保留审计轨迹。
func (g *GatewayImpl) autoDenyOnP0Regression(ctx context.Context, p *types.HITLPrompt) *types.HITLResponse {
	slog.Warn("hitl_gateway: P0 regression failed, auto-denying patch", "checkpoint", p.ID)
	resp := types.HITLResponse{Approved: false, Reason: "auto_denied_p0_regression_failed"}
	metrics.RecordHITLDecision(ctx, p.CheckpointType, "denied", "auto_denied_p0_regression")

	archiveKey := []byte(fmt.Sprintf("hitl:archive:%s:%d", p.ID, time.Now().UnixNano()))
	if archiveData, mErr := json.Marshal(resp); mErr == nil {
		if aErr := g.store.Put(ctx, archiveKey, archiveData); aErr != nil {
			slog.Warn("hitl_gateway: auto-deny archive failed", "checkpoint", p.ID, "err", aErr)
		}
	} else {
		slog.Error("hitl_gateway: auto-deny archive marshal failed, skipping archive",
			"checkpoint", p.ID, "err", mErr)
	}
	return &resp
}

// attachShadowReport 把影子回归差异报告追加到审批文案。
// 报告不可用时显式告警并在文案中标注——否则审批人看到的是一个
// "看起来正常但没有回归证据"的请求，比没有报告更危险。
func (g *GatewayImpl) attachShadowReport(ctx context.Context, p *types.HITLPrompt) {
	shadowReport, rErr := g.regression.DetectRegression(context.Background(), p.CheckpointType)
	switch {
	case rErr != nil:
		slog.ErrorContext(ctx, "hitl_gateway: shadow regression report unavailable, approver will see no diff evidence",
			"checkpoint", p.ID, "err", rErr)
		p.PromptText += "\n\n⚠️ 影子回归报告生成失败，本次审批缺少回归差异证据，请谨慎批准。"
	case shadowReport != nil:
		p.PromptText += "\n\n" + shadowReport.Markdown
	}
}

// applyMandatoryCooldown 设置强制冷却期：审批人必须等待该时间点之后才能批准，
// 用于强制"先读完影子回归报告再决策"（Respond 内校验 EligibleApproveTime）。
func (g *GatewayImpl) applyMandatoryCooldown(p *types.HITLPrompt) {
	cooldown := g.l3Cooldown
	if cooldown == 0 {
		cooldown = 10 * time.Minute
	}
	p.EligibleApproveTime = time.Now().Add(cooldown).Unix()
}
