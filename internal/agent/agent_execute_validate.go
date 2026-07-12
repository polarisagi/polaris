package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/polarisagi/polaris/internal/prompt"
	"github.com/polarisagi/polaris/internal/prompt/templates"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// runValidateDAG 从 agent_execute_dag.go 拆出（R7 文件行数治理，2026-07-07）：
// S_VALIDATE 状态的完整校验逻辑（PolicyGate 结构化校验 + L3 LLM 看门狗），
// 与 S_EXECUTE 状态的 runExecuteDAG（工具调用/2PC/Saga）职责边界清晰，
// 拆分不改变任何逻辑，仅为职责边界物理隔离。
func (a *Agent) runValidateDAG(ctx context.Context) error {
	var plan *protocol.DAGPlan
	if a.sCtx.DAGModel != nil {
		plan = &protocol.DAGPlan{
			Nodes: a.sCtx.DAGModel.Nodes,
			Edges: a.sCtx.DAGModel.Edges,
		}
	}

	vCtx := &protocol.DAGValidationContext{
		Plan: plan,
		// ActiveTaintLevel 取 DAGModel 所有节点的最高 TaintLevel（PropagateTaint 语义）。
		// 依据: ADR-0007 自然传播规则——output = max(inputs)，只升不降。
		// 不直接用 RawIntentTS.Level()（固定 TaintHigh）原因：
		//   validateTaintGate 对 TaintHigh 会拦截所有非只读工具；
		//   而节点 TaintLevel 来自 parsePlanOnSuccess 中 pCtx.MaxTaintLevel，
		//   若 LLM 已将摘要降级为 TaintMedium，节点级应反映该降级结果。
		// 若所有节点均为 TaintNone（FastPath/空 DAG），网关自然跳过（< TaintMedium）。
		ActiveTaintLevel: maxNodeTaintLevel(plan),
		PolicyGate:       a.policyGate,
		ToolExecutor:     a.toolRegistry, // 用于 isReadOnlyTool 动态查询工具 Capability
		AgentID:          a.sCtx.AgentID,
		SessionID:        a.sCtx.SessionID,
		SystemTier:       a.Config.SystemTier, // 由 M3 HardwareProbe 探测后通过 AgentConfig.SystemTier 注入
	}

	// [Task 11] 向 PolicyGate 填充 monthly_spend_usd 供 Cedar budget_cap 规则使用。
	// MonthlyBudgetUSDConfig == 0 表示不限额，跳过注入避免销耗所有请求。
	if a.sCtx.Budget != nil && a.sCtx.MonthlyBudgetUSDConfig > 0 {
		vCtx.MonthlySpendUSD = a.sCtx.Budget.EstimatedSpendUSD()
		vCtx.MonthlyBudgetUSD = a.sCtx.MonthlyBudgetUSDConfig
	}

	if a.dagValidator == nil {
		// fail-closed: 无校验引擎时拒绝（2026-07-12 execute/dag 迁出后新增；
		// NewAgentWithDefaults/buildAgent 均默认注入，理论上不会命中，仅作防御）。
		a.asyncIntent(types.TriggerValidateFail)
		return apperr.New(apperr.CodeInternal, "runValidateDAG: dagValidator is nil (fail-closed)")
	}

	if err := a.dagValidator.Validate(ctx, vCtx); err != nil {
		// 校验失败→ 异步推送 TriggerValidateFail 以面向 FSM 的 S_REPLAN
		a.asyncIntent(types.TriggerValidateFail)
		// 返回非致命 error 提示调用方失败原因，但不能让 Run 循环崩溃
		return apperr.Wrap(apperr.CodeInternal, "s_validate failed", err)
	}

	// L3: LLM 看门狗校验 (上提为标准 FSM Effect)
	// 仅对 Tier 1+ 生效
	if vCtx.SystemTier >= 1 && a.provider != nil && vCtx.Plan != nil { //nolint:nestif
		var dangerous []string
		for _, node := range vCtx.Plan.Nodes {
			tool, err := a.toolRegistry.Lookup(node.ToolName)
			if err != nil {
				continue
			}
			// L3 仅针对 RiskPrivileged 节点，非只读但低风险节点已由 L1/L2 覆盖
			if tool.RiskLevel == types.RiskPrivileged {
				dangerous = append(dangerous, fmt.Sprintf("Tool: %s, Args: %s", node.ToolName, string(node.Args)))
			}
		}

		if len(dangerous) > 0 {
			// Prompt 统一走 internal/prompt/templates 管理（A-12 修复），且 dangerous
			// 列表（来自 DAG 节点 ToolName/Args，可能间接受 LLM 分解结果影响，非完全可信）
			// 用 prompt.NewRandomBoundary() 生成的随机边界符包裹，防止边界逃逸注入
			// （与 GR-7-001 SecurityAuditAgent 的修复采用同一套防御模式）。
			boundaryStart, boundaryEnd := prompt.NewRandomBoundary()
			userPrompt, tmplErr := templates.Render("l3_watchdog_review.tmpl", map[string]string{
				"BoundaryStart": boundaryStart,
				"BoundaryEnd":   boundaryEnd,
				"DangerousList": strings.Join(dangerous, "\n"),
			})
			if tmplErr != nil {
				return apperr.Wrap(apperr.CodeInternal, "s_validate: render l3 watchdog prompt", tmplErr)
			}
			systemPrompt, tmplErr := templates.Render("l3_watchdog_system.tmpl", nil)
			if tmplErr != nil {
				return apperr.Wrap(apperr.CodeInternal, "s_validate: render l3 watchdog system prompt", tmplErr)
			}

			llmEff := protocol.LLMFillEffect{
				SchemaRef: "l3_watchdog",
				PromptFn: func(pCtx protocol.StateContext) []types.Message {
					return []types.Message{
						{Role: "system", Content: systemPrompt},
						{Role: "user", Content: userPrompt},
					}
				},
				OnSuccess: func(pCtx protocol.StateContext, content []byte) (types.State, error) {
					if strings.HasPrefix(strings.ToUpper(string(content)), "DENY") {
						a.asyncIntent(types.TriggerValidateFail)
						return "S_VALIDATE_FAIL", apperr.New(apperr.CodeForbidden, "LLM Watchdog denied: "+string(content))
					}
					a.asyncIntent(types.TriggerValidateOk)
					return "S_VALIDATE_OK", nil
				},
				OnFailure: func(pCtx protocol.StateContext, err error) (types.State, error) {
					// L3 失败时 fail-open——架构设计，非疏漏。
					// 依据: M04 §L3 LLM 看门狗: "LLM 不可用时 fail-open 推进 S_VALIDATE_OK"。
					// L3 是补充信号层：L0/L1/L2 未放行的动作不可因 L3 通过而放行；
					// L3 DENY 推进 ValidateFail，L3 LLM 不可用时不应因此阻断正常业务流。
					// 禁止改为 fail-closed：L3 LLM 故障会导致所有非只读 DAG 永久卡住。
					a.asyncIntent(types.TriggerValidateOk)
					return "S_VALIDATE_OK", nil
				},
				MaxRetry:  0, // 看门狗不重试
				ModelPool: "reasoning",
			}

			// 递归执行该 Effect，利用标准流程调用 LLM 并计费
			return a.executeEffect(ctx, llmEff)
		}
	}

	// 校验通过→ 异步推送 TriggerValidateOk
	a.asyncIntent(types.TriggerValidateOk)
	return nil
}
