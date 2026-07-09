package fsm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/configs"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// Effect 工厂方法（PromptFn + OnSuccess/OnFailure）
// ============================================================================

func (sm *StateMachine) promptPerceive(sCtx *StateContext, pCtx protocol.StateContext) []types.Message {
	// 有记忆系统时注入历史事件（Episodic 相关任务 + ReasoningState 跨轮持久化）
	if pCtx.Mem != nil {
		ctx, cancel := sm.bgCtx()
		defer cancel()
		if msgs, err := sm.cb.BuildPerceiveContext(ctx, pCtx.Mem, sCtx, sCtx.Cognitive); err == nil {
			return msgs
		}
	}

	b := protocol.NewPromptBuilder()
	if sCtx.SysEnvSnapshot != "" {
		b.WriteSystemEnvironment(sCtx.SysEnvSnapshot)
	}
	tmpl, _ := configs.LoadPromptTemplate("kernel/perceive.md", map[string]any{
		"ExtensionsSection": sCtx.InstalledExtensionsInfo,
	})
	safeInst, _ := taint.SanitizeToSafe(taint.NewTaintedString(
		tmpl,
		taint.TaintSource{OriginTaintLevel: types.TaintNone},
		"system_prompt",
	))
	b.WriteInstruction(safeInst)
	b.WriteUserData(sCtx.RawIntentTS)
	msgs := b.Build()
	if sCtx.EpochTracker != nil {
		sCtx.ContextEpoch = sCtx.EpochTracker.check(msgs)
	}
	return msgs
}

func (sm *StateMachine) onPerceiveSuccess(sCtx protocol.StateContext, fill []byte) (types.State, error) {
	return types.State("S_PERCEIVE_DONE"), nil
}

func (sm *StateMachine) onPerceiveFailure(sCtx protocol.StateContext, err error) (types.State, error) {
	return types.State("S_PERCEIVE_FAILED"), apperr.New(apperr.CodeInternal, "perceive: LLM fill failed")
}

func (sm *StateMachine) promptPlan(sCtx *StateContext, pCtx protocol.StateContext) []types.Message {
	// 有记忆系统时注入历史执行经验（Episodic Top-5 + 任务目标 + 工具列表）
	if pCtx.Mem != nil {
		ctx, cancel := sm.bgCtx()
		defer cancel()
		if msgs, err := sm.cb.BuildPlanContext(ctx, pCtx.Mem, sCtx, nil, sCtx.Cognitive); err == nil {
			sm.appendDynamicHints(msgs)
			if sCtx.EpochTracker != nil {
				sCtx.ContextEpoch = sCtx.EpochTracker.check(msgs)
			}
			return msgs
		}
	}

	b := protocol.NewPromptBuilder()
	if sCtx.SysEnvSnapshot != "" {
		b.WriteSystemEnvironment(sCtx.SysEnvSnapshot)
	}

	// TaskID 激活作用域同源于 pCtx.SessionID（与 agent_execute.go 的
	// executor.Execute(ctx, plan, a.sCtx.SessionID, a.sCtx.AgentID) 保持一致），
	// 否则 search_tools 上一轮激活的工具在本轮（无 Memory 系统时的降级路径）
	// 重建 ToolsSection 时读不到，见 internal/tool/catalog/composite.go Schemas()。
	toolListCtx, cancel := sm.bgCtx()
	defer cancel()
	toolListCtx = context.WithValue(toolListCtx, protocol.CtxTaskIDKey{}, pCtx.SessionID)

	tmpl, _ := configs.LoadPromptTemplate("kernel/plan.md", map[string]any{
		"ToolsSection":      sm.cb.BuildToolListSection(toolListCtx, nil),
		"ExtensionsSection": sCtx.InstalledExtensionsInfo,
	})

	if sCtx.GroundingGap != "" {
		tmpl += "\n\nCritical Knowledge Gap:\n" + sCtx.GroundingGap + "\n(Please address this gap explicitly in the plan.)"
	}

	safeInst, _ := taint.SanitizeToSafe(taint.NewTaintedString(
		tmpl,
		taint.TaintSource{OriginTaintLevel: types.TaintNone},
		"system_prompt",
	))
	b.WriteInstruction(safeInst)

	mode := "auto_review"
	anyAppEnabled := false
	chromeEnabled := false
	if sCtx.Preferences != nil {
		if v, ok := sCtx.Preferences["computer_use_mode"]; ok && v != "" {
			mode = v
		}
		if v, ok := sCtx.Preferences["computer_any_app_enabled"]; ok {
			anyAppEnabled = v == "true"
		}
		if v, ok := sCtx.Preferences["computer_chrome_enabled"]; ok {
			chromeEnabled = v == "true"
		}
	}
	b.WriteComputerUsePolicy(mode, anyAppEnabled, chromeEnabled)

	if sCtx.TaskModel != nil {
		goalTS := taint.NewTaintedString(
			"Task Goal: "+sCtx.TaskModel.Goal,
			taint.TaintSource{OriginTaintLevel: types.TaintMedium},
			"m4_task_model",
		)
		b.WriteUserData(goalTS)
	}

	// 预算压力约束：75% 阈值已触发时，要求 LLM 生成最小必要 DAG。
	// 防止在预算末尾生成大型 DAG 导致 S_EXECUTE 阶段触发 INFERENCE_OOM。
	if sCtx.BudgetPressure {
		budgetHint := fmt.Sprintf(
			"[BUDGET_CONSTRAINT] Token budget is above %d%%. Generate a MINIMAL DAG: "+
				"maximum 3 nodes, only strictly necessary tool calls, no exploratory steps. "+
				"Remaining budget: %d tokens.",
			BudgetCriticalPct,
			sCtx.TokenBudget-sCtx.TokensUsed,
		)
		budgetTS := taint.NewTaintedString(
			budgetHint,
			taint.TaintSource{OriginTaintLevel: types.TaintNone},
			"system_budget_constraint",
		)
		safeHint, _ := taint.SanitizeToSafe(budgetTS)
		b.WriteInstruction(safeHint)
	}

	msgs := b.Build()
	if sCtx.EpochTracker != nil {
		sCtx.ContextEpoch = sCtx.EpochTracker.check(msgs)
	}
	return msgs
}

func (sm *StateMachine) onPlanFailure(sCtx protocol.StateContext, err error) (types.State, error) {
	return types.State("S_PLAN_FAILED"), apperr.New(apperr.CodeInternal, "plan: LLM fill failed")
}

func (sm *StateMachine) promptReflect(sCtx *StateContext, pCtx protocol.StateContext) []types.Message {
	// 有记忆系统时注入历史执行经验
	if pCtx.Mem != nil {
		ctx, cancel := sm.bgCtx()
		defer cancel()
		if msgs, err := sm.cb.BuildReflectContext(ctx, pCtx.Mem, sCtx); err == nil {
			return msgs
		}
	}

	b := protocol.NewPromptBuilder()
	if sCtx.SysEnvSnapshot != "" {
		b.WriteSystemEnvironment(sCtx.SysEnvSnapshot)
	}
	tmpl, _ := configs.LoadPromptTemplate("kernel/reflect.md", nil)
	safeInst, _ := taint.SanitizeToSafe(taint.NewTaintedString(
		tmpl,
		taint.TaintSource{OriginTaintLevel: types.TaintNone},
		"system_prompt",
	))
	b.WriteInstruction(safeInst)

	resultTS := taint.NewTaintedString(
		"Execution Result: "+string(sCtx.ExecuteResult),
		taint.TaintSource{OriginTaintLevel: types.TaintHigh},
		"m4_execute_result",
	)
	b.WriteUserData(resultTS)
	msgs := b.Build()
	if sCtx.EpochTracker != nil {
		sCtx.ContextEpoch = sCtx.EpochTracker.check(msgs)
	}
	return msgs
}

// bgCtx 返回带超时的后台 context，用于内存检索（不绑定任务生命周期）。
// 超时 3s 对齐 M05 §2 L1 Episodic 读 <5ms 要求，防 SQLite 锁争用无限阻塞。
func (sm *StateMachine) bgCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 3*time.Second)
}

func (sm *StateMachine) onReflectSuccess(sCtx protocol.StateContext, fill []byte) (types.State, error) {
	var ref ReflectionModel
	if err := json.Unmarshal(fill, &ref); err != nil {
		slog.Warn("reflect: failed to parse ReflectionModel", "err", err)
		return types.State("S_REFLECT_DONE"), nil
	}

	if len(ref.Learnings) > 0 && sCtx.Mem != nil {
		ctx, cancel := sm.bgCtx()
		defer cancel()
		for _, learning := range ref.Learnings {
			if learning == "" {
				continue
			}
			event := types.Event{
				ID:        fmt.Sprintf("reflect_%d", time.Now().UnixNano()),
				Type:      types.EventType("learning"),
				Status:    types.StatusDone,
				TaskID:    sCtx.SessionID,
				AgentID:   sCtx.AgentID,
				Payload:   []byte(`{"learning":` + fmt.Sprintf("%q", learning) + `}`),
				CreatedAt: time.Now(),
			}
			if err := sCtx.Mem.AppendEpisodicEvent(ctx, event, types.TaintNone); err != nil {
				slog.Warn("reflect: failed to write learning to episodic memory",
					"session_id", sCtx.SessionID, "err", err)
			}
		}
	}

	return types.State("S_REFLECT_DONE"), nil
}

func (sm *StateMachine) onReflectFailure(sCtx protocol.StateContext, err error) (types.State, error) {
	return types.State("S_REFLECT_FAILED"), apperr.New(apperr.CodeInternal, "reflect: LLM fill failed")
}
