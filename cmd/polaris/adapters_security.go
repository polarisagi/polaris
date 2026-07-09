package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/automation/hitl"
	swarmAgents "github.com/polarisagi/polaris/internal/swarm/agents"
	"github.com/polarisagi/polaris/pkg/types"
)

// ─── hitlNotifierAdapter ──────────────────────────────────────────────────────
//
// 将 hitl.GatewayImpl 适配为 orchestrator.HITLNotifier（LogicCollapseMonitor 依赖）。
// 在 cmd/ 层定义以避免 pkg/swarm → pkg/edge/hitl 包循环。
type hitlNotifierAdapter struct {
	gateway *hitl.GatewayImpl
}

// NotifyHITL 发起高风险技能的异步 HITL 审批请求（fire-and-forget）。
// triggerCollapse 本身已在 goroutine 中运行，此处再异步是因为 GatewayImpl.Prompt
// 会阻塞直到审批完成或超时，不应占用 triggerCollapse 的 goroutine。
func (a *hitlNotifierAdapter) NotifyHITL(_ context.Context, skillID, reason string) error {
	p := types.HITLPrompt{
		ID:             fmt.Sprintf("logic_collapse_%s_%d", skillID, time.Now().UnixNano()),
		CheckpointType: "logic_collapse_high_risk",
		PromptText:     fmt.Sprintf("高风险 Skill 请求 Logic Collapse 审批 [skill=%s reason=%s]", skillID, reason),
		RiskLevel:      3, // high
		TaintLevel:     2, // TaintMedium → 超时自动拒绝
		DeadlineNs:     time.Now().Add(24 * time.Hour).UnixNano(),
	}
	// 异步发起：不阻塞 triggerCollapse，HITL 审批结果由 M13 Interface 侧处理
	//custom-nolint:bare-goroutine // 历史代码暂留，需结合上下文梳理 ctx 传递链路，后续重构替换
	go func() {
		bCtx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
		defer cancel()
		if _, err := a.gateway.Prompt(bCtx, p); err != nil {
			slog.Warn("hitl_notifier: HITL prompt failed",
				"skill_id", skillID, "checkpoint_id", p.ID, "err", err)
		}
	}()
	return nil
}

// govAgentAdapter 将 *agents.GovernanceAgent 适配为 codeact 包的 govAgent 接口。
// codeact 包在 internal/action/codeact/ 定义私有 govAgent 接口：
//
//	ValidateCode(language string, code []byte, caps map[string]bool) error
//
// 由于 CapabilitySet 已改为类型别名，GovernanceAgent 直接满足接口，此适配器仅供文档目的。
// 若将来 CapabilitySet 改回命名类型，在此处加转换逻辑即可。
type govAgentAdapter struct {
	inner *swarmAgents.GovernanceAgent
}

func (a *govAgentAdapter) ValidateCode(language string, code []byte, caps map[string]bool) error {
	return a.inner.ValidateCode(language, code, caps) //nolint:wrapcheck
}

// securityAuditReviewerAdapter 将 *agents.SecurityAuditAgent 适配为 codeact 包的
// LLMPeerReviewer 接口（L2，同步阻塞）。codeact.CodeAct.validateL2 仅在
// req.TaintLevel>=TaintHigh 时调用，返回 "danger"/"warning" 触发拒绝或 HITL 审批，
// 这是 CodeAct 执行前唯一的语义级（而非规则/AST）安全审查——LLM 生成代码通过
// L0(AST)+L1(regex) 静态检查后，真正决定"这段代码想干什么"仍需要 L2。
type securityAuditReviewerAdapter struct {
	inner *swarmAgents.SecurityAuditAgent
}

func (a *securityAuditReviewerAdapter) Review(ctx context.Context, code string) (string, error) {
	// protocol.CodeActRequest 只允许 "python"|"bash"（validateBasic 强制），
	// 此处语言信息已在校验链路上游确认，Review 接口本身不携带 language 参数，
	// 交由 audit prompt 自行从代码内容推断更省事；直接传 "code" 作占位语言标签，
	// ReviewSync 内部仅用它做审计文案里的语言展示，不影响安全判断。
	return a.inner.ReviewSync(ctx, "code", []byte(code)) //nolint:wrapcheck
}
