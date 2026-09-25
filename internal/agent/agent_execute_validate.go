package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

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
	// 旧计划的签发依据先作废：校验失败到下次通过之间不得为任何调用签发令牌。
	a.validatedCalls.Store(nil)
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
		PolicyGate:       a.Security.PolicyGate,
		ToolExecutor:     a.toolRegistry, // 用于 isReadOnlyTool 动态查询工具 Capability
		AgentID:          a.sCtx.AgentID,
		SessionID:        a.sCtx.SessionID,
		SystemTier:       a.Config.SystemTier, // 由 M3 HardwareProbe 探测后通过 AgentConfig.SystemTier 注入
		ReviewChecker:    a.Security.TaintReviewChecker,
		TaintAuditor:     a.Security.TaintAuditor,
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

	if err := a.validateWithTaintReview(ctx, vCtx); err != nil {
		a.sCtx.RecordReplanFeedback(validationFeedback(plan, err))
		a.sCtx.RecordFailure(validationFailureKind(err))
		// 校验失败→ 异步推送 TriggerValidateFail 以面向 FSM 的 S_REPLAN
		a.asyncIntent(types.TriggerValidateFail)
		// 返回非致命 error 提示调用方失败原因，但不能让 Run 循环崩溃
		return apperr.Wrap(apperr.CodeInternal, "s_validate failed", err)
	}
	a.recordValidatedPlan(plan)

	// BlindZone HITL 检查点（GR-4.1-005）：S_PLAN 由 BlindZoneDetector 置位，此前无任何读取方。
	if handled, err := a.runBlindZoneHITL(ctx, plan); handled {
		return err
	}

	// L3: LLM 看门狗校验 (上提为标准 FSM Effect)
	// 仅对 Tier 1+ 生效
	if vCtx.SystemTier >= 1 && a.provider != nil && vCtx.Plan != nil {
		if handled, err := a.runL3Watchdog(ctx, vCtx); handled {
			return err
		}
	}

	// 校验通过→ 异步推送 TriggerValidateOk
	a.asyncIntent(types.TriggerValidateOk)
	return nil
}

// runL3Watchdog 执行 S_VALIDATE 阶段 L3 LLM 看门狗审查（从 runValidateDAG 拆出，
// gocyclo 治理，行为不变）。仅当 DAG 中存在 RiskPrivileged 节点时才触发看门狗 Effect；
// handled=false 表示本次未触发（无危险节点），调用方应继续走默认的 ValidateOk 路径。
func (a *Agent) runL3Watchdog(ctx context.Context, vCtx *protocol.DAGValidationContext) (bool, error) {
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

	if len(dangerous) == 0 {
		return false, nil
	}

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
		return true, apperr.Wrap(apperr.CodeInternal, "s_validate: render l3 watchdog prompt", tmplErr)
	}
	systemPrompt, tmplErr := templates.Render("l3_watchdog_system.tmpl", nil)
	if tmplErr != nil {
		return true, apperr.Wrap(apperr.CodeInternal, "s_validate: render l3 watchdog system prompt", tmplErr)
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
	return true, a.executeEffect(ctx, llmEff).Err
}

// runBlindZoneHITL 对盲区任务（该类任务屡次生产却无失败记忆闭环，系统对其可靠性无据可依）
// 中含副作用的 DAG 请求人工确认。只读计划不打扰人（盲区的风险在于"做错事"，读不改变外部状态）。
// handled=true 表示已给出最终结论（拒绝），调用方直接返回 err；false 表示继续后续校验。
//
// HITL 网关未装配时降级放行并告警：盲区是可靠性信号而非安全违规，与 L3 看门狗 fail-open 同口径，
// 否则未配置审批渠道的部署会让所有盲区写操作永久卡死。
func (a *Agent) runBlindZoneHITL(ctx context.Context, plan *protocol.DAGPlan) (bool, error) {
	if !a.sCtx.BlindZoneHITLRequired || plan == nil {
		return false, nil
	}
	var sideEffects []string
	for _, node := range plan.Nodes {
		t, err := a.toolRegistry.Lookup(node.ToolName)
		if err != nil || t.Capability > types.CapReadOnly {
			sideEffects = append(sideEffects, node.ToolName)
		}
	}
	if len(sideEffects) == 0 {
		return false, nil
	}
	if a.hitl == nil {
		slog.WarnContext(ctx, "agent: blind-zone task requires HITL but no gateway is wired; proceeding",
			"session_id", a.sCtx.SessionID, "tools", sideEffects)
		return false, nil
	}
	resp, err := a.promptHITLInTurn(ctx, types.HITLPrompt{
		ID:             fmt.Sprintf("hitl_%d", time.Now().UnixNano()),
		AgentID:        a.sCtx.AgentID,
		CheckpointType: "blind_zone",
		PromptText: fmt.Sprintf("Blind-zone task (no failure-memory feedback for this task type). "+
			"Plan performs side effects via: %s. Approve to execute.", strings.Join(sideEffects, ", ")),
		TaintLevel: a.sessionTaint(),
		DeadlineNs: time.Now().Add(10 * time.Minute).UnixNano(),
	}, strings.Join(sideEffects, ", "), nil)
	if err == nil && resp != nil && resp.Approved {
		a.sCtx.BlindZoneHITLRequired = false // 本任务已获批，replan 后不重复打扰
		return false, nil
	}
	a.asyncIntent(types.TriggerValidateFail)
	if err != nil {
		return true, apperr.Wrap(apperr.CodeForbidden, "s_validate: blind-zone HITL unavailable", err)
	}
	return true, apperr.New(apperr.CodeForbidden, "s_validate: blind-zone plan rejected by human reviewer")
}
