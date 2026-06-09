package kernel

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/polarisagi/polaris/configs"
	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/substrate"
)

// StateMachine 持有控制流。LLM 是概率协处理器——Go 状态机确定性推进，LLM 仅做结构化填空。
// 权威规约: spec/state.yaml §m4_par_state_machine
// 架构文档: docs/arch/M04-Agent-Kernel.md §1

// Transition 是状态机中一条确定性边。
// LLM 仅在 LLMFillEffect 执行时调用，而非 Transition 自身。
type Transition struct {
	From    protocol.AgentState
	Trigger protocol.AgentTrigger
	To      protocol.AgentState
	Guard   func(ctx context.Context, sCtx *StateContext) bool
	// Effects 返回此转移产生的副作用（DeterministicEffect 或 LLMFillEffect）。
	Effects func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error)
}

// StateMachine 管理 Agent 状态生命周期。
type StateMachine struct {
	current       protocol.AgentState
	transitions   map[protocol.AgentState]map[protocol.AgentTrigger]Transition // state → trigger → transition
	history       []protocol.AgentState
	replanCount   int
	eventSeq      int64 // 单调递增事件序列号，用于生成确定性 Event ID（replay key）
	startedAt     time.Time
	interruptFrom protocol.AgentState // S_INTERRUPT 时记录被中断的原状态（Resume 路径用）
	mu            sync.Mutex
}

// StateContext 穿越状态机各转移的共享上下文（与 protocol.StateContext 互补）。
type StateContext struct {
	AgentID       string
	SessionID     string
	RawIntentTS   substrate.TaintedString // 原始自然语言意图 (外部输入，带污点)
	TaskModel     *TaskModel              // S_PERCEIVE 产出
	DAGModel      *DAGModel               // S_PLAN 产出
	Reflection    *ReflectionModel        // S_REFLECT 产出
	ExecuteResult []byte
	MaxReplan     int
	Timeout       time.Duration
	StartedAt     time.Time

	// Inference Budget 控制
	TokenBudget int
	TokensUsed  int
	// BudgetWarned / BudgetPressure 防重复日志标记（仅本轮 Agent 生命周期内有效）。
	// BudgetWarned: 已越过 50% 告警水位（记录一次日志，不改变行为）。
	// BudgetPressure: 已越过 75% 压力水位（S_PLAN 收紧 DAG 规模）。
	BudgetWarned   bool
	BudgetPressure bool

	// Step Budget 控制（Adaptive Max-Steps）
	// MaxStepsLimit 由 AgentConfig.MaxSteps 初始化；StepScorer 低分时动态收紧。
	// 0 = 无上限（不推荐用于生产）。
	StepsUsed     int
	MaxStepsLimit int

	// 认知状态
	SurpriseIndex float64

	// 用户中断（S_INTERRUPT 相关，inv_global_08）
	InterruptReq *protocol.InterruptRequest

	// ReasoningState 跨轮次持久化的推理状态（M04 §7.1 + M05 §3.1）。
	// S_REFLECT 阶段产出，下轮 S_PERCEIVE 时注入 ContextWindow。
	ReasoningState []byte

	// 偏好配置
	Preferences map[string]string

	// 挂起原因（如 capability_gap）
	SuspendReason string

	// SysEnvSnapshot 是启动时获取的系统静态快照，注入到每个 Prompt 头部
	SysEnvSnapshot string

	// InstalledExtensionsInfo 包含当前系统已安装的扩展清单
	InstalledExtensionsInfo string
}

// TaskModel LLM 填槽产出——将自然语言任务结构化。
type TaskModel struct {
	Goal        string
	SubTasks    []string
	Constraints []string
	Complexity  float64
}

// DAGModel LLM 填槽产出——可执行的有向无环图。
// 权威类型 ExecNode/ExecEdge 定义见同包 dag_executor.go。
type DAGModel struct {
	Nodes []ExecNode
	Edges []ExecEdge
}

// ReflectionModel LLM 填槽产出——执行后反思。
type ReflectionModel struct {
	GoalAchieved bool
	Errors       []string
	Learnings    []string
}

func NewStateMachine() *StateMachine {
	sm := &StateMachine{
		current:     protocol.AgentStateIdle,
		transitions: make(map[protocol.AgentState]map[protocol.AgentTrigger]Transition),
		history:     make([]protocol.AgentState, 0),
		startedAt:   time.Now(),
	}
	sm.registerTransitions()
	return sm
}

// nextEventID 生成确定性事件 ID：{session_id}:{seq}:{event_type}
// 满足 inv_M4_02 重放确定性要求——同 session+seq → 同 ID，不依赖 wall clock。
func (sm *StateMachine) nextEventID(sessionID, eventType string) string {
	sm.eventSeq++
	return sessionID + ":" + itoa64(sm.eventSeq) + ":" + eventType
}

func itoa64(i int64) string {
	if i == 0 {
		return "0"
	}
	var buf [12]byte
	pos := len(buf)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

func (sm *StateMachine) Current() protocol.AgentState {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.current
}

func (sm *StateMachine) ReplanCount() int {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.replanCount
}

// Dispatch 接收触发事件，查找匹配转移，执行 guard + effects，推进状态。
// 返回的 effects 由 Agent.Run 消费——LLMFillEffect 调 LLM，DeterministicEffect 直接执行。
func (sm *StateMachine) Dispatch(ctx context.Context, sCtx *StateContext, trigger protocol.AgentTrigger) ([]protocol.Effect, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	current := sm.current

	// ── S_INTERRUPT 通用处理（优先于 transitions 表）──────────────────────────
	// 任意活跃态（非终态、非 S_INTERRUPT 自身）均可接收中断信号。
	if trigger == protocol.TriggerInterruptReceived {
		if !isTerminalState(current) && current != protocol.AgentStateInterrupt {
			sm.interruptFrom = current
			sm.history = append(sm.history, current)
			sm.current = protocol.AgentStateInterrupt
			return nil, nil
		}
	}

	// S_INTERRUPT 出边：Resume → 恢复原状态；Abort → S_FAILED
	if current == protocol.AgentStateInterrupt {
		switch trigger {
		case protocol.TriggerInterruptResume:
			sm.history = append(sm.history, current)
			sm.current = sm.interruptFrom
			return nil, nil
		case protocol.TriggerInterruptAbort:
			sm.history = append(sm.history, current)
			sm.current = protocol.AgentStateFailed
			return nil, nil
		}
	}
	// ─────────────────────────────────────────────────────────────────────────

	triggerMap, ok := sm.transitions[current]
	if !ok {
		return nil, perrors.New(perrors.CodeInternal, fmt.Sprintf("no transitions from state %v", current))
	}

	t, ok := triggerMap[trigger]
	if !ok {
		return nil, perrors.New(perrors.CodeInternal, fmt.Sprintf("no transition from %v with trigger %v", current, trigger))
	}

	// Guard 检查
	if t.Guard != nil && !t.Guard(ctx, sCtx) {
		return nil, perrors.New(perrors.CodeInternal, fmt.Sprintf("guard rejected transition %v → %v", current, t.To))
	}

	// 执行 Effects（LLMFillEffect | DeterministicEffect）
	effects, err := t.Effects(ctx, sCtx)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("transition %v → %v failed", current, t.To), err)
	}

	// 特殊处理: S_REPLAN 计数 + 耗尽检查
	if t.To == protocol.AgentStateReplan {
		sm.replanCount++
		if sm.replanCount >= sCtx.MaxReplan {
			// replan 耗尽 → 自动进阶 S_FAILED，返回 ErrReplanExhausted
			sm.history = append(sm.history, current, t.To)
			sm.current = protocol.AgentStateFailed
			return nil, ErrReplanExhausted
		}
	}

	// 记录历史
	sm.history = append(sm.history, current)
	sm.current = t.To

	return effects, nil
}

// History 返回状态遍历历史。
func (sm *StateMachine) History() []protocol.AgentState {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	h := make([]protocol.AgentState, len(sm.history))
	copy(h, sm.history)
	return h
}

// Reset 重置状态机到初始状态（用于 Agent 复用时）。
func (sm *StateMachine) Reset() {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.current = protocol.AgentStateIdle
	sm.history = sm.history[:0]
	sm.replanCount = 0
	sm.startedAt = time.Now()
}

func (sm *StateMachine) add(t Transition) {
	if sm.transitions[t.From] == nil {
		sm.transitions[t.From] = make(map[protocol.AgentTrigger]Transition)
	}
	sm.transitions[t.From][t.Trigger] = t
}

// ============================================================================
// Effect 工厂方法（PromptFn + OnSuccess/OnFailure）
// ============================================================================

func (sm *StateMachine) promptPerceive(sCtx *StateContext, pCtx protocol.StateContext) []protocol.Message {
	// 有记忆系统时注入历史事件（Episodic 相关任务 + ReasoningState 跨轮持久化）
	if pCtx.Mem != nil {
		if msgs, err := buildPerceiveContext(sm.bgCtx(), pCtx.Mem, sCtx); err == nil {
			return msgs
		}
	}

	b := NewPromptBuilder()
	if sCtx.SysEnvSnapshot != "" {
		b.WriteSystemEnvironment(sCtx.SysEnvSnapshot)
	}
	tmpl, _ := configs.LoadPromptTemplate("kernel/perceive.md", map[string]any{
		"ExtensionsSection": sCtx.InstalledExtensionsInfo,
	})
	safeInst, _ := substrate.SanitizeToSafe(substrate.NewTaintedString(
		tmpl,
		substrate.TaintSource{OriginTaintLevel: protocol.TaintNone},
		"system_prompt",
	))
	b.WriteInstruction(safeInst)
	b.WriteUserData(sCtx.RawIntentTS)
	return b.Build()
}

func (sm *StateMachine) onPerceiveSuccess(sCtx protocol.StateContext, fill []byte) (protocol.State, error) {
	return protocol.State("S_PERCEIVE_DONE"), nil
}

func (sm *StateMachine) onPerceiveFailure(sCtx protocol.StateContext, err error) (protocol.State, error) {
	return protocol.State("S_PERCEIVE_FAILED"), perrors.New(perrors.CodeInternal, "perceive: LLM fill failed")
}

func (sm *StateMachine) promptPlan(sCtx *StateContext, pCtx protocol.StateContext) []protocol.Message {
	// 有记忆系统时注入历史执行经验（Episodic Top-5 + 任务目标 + 工具列表）
	if pCtx.Mem != nil {
		if msgs, err := buildPlanContext(sm.bgCtx(), pCtx.Mem, sCtx, pCtx.Tools); err == nil {
			return msgs
		}
	}

	b := NewPromptBuilder()
	if sCtx.SysEnvSnapshot != "" {
		b.WriteSystemEnvironment(sCtx.SysEnvSnapshot)
	}

	tmpl, _ := configs.LoadPromptTemplate("kernel/plan.md", map[string]any{
		"ToolsSection":      buildToolListSection(pCtx.Tools),
		"ExtensionsSection": sCtx.InstalledExtensionsInfo,
	})
	safeInst, _ := substrate.SanitizeToSafe(substrate.NewTaintedString(
		tmpl,
		substrate.TaintSource{OriginTaintLevel: protocol.TaintNone},
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
		goalTS := substrate.NewTaintedString(
			"Task Goal: "+sCtx.TaskModel.Goal,
			substrate.TaintSource{OriginTaintLevel: protocol.TaintMedium},
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
			budgetCriticalPct,
			sCtx.TokenBudget-sCtx.TokensUsed,
		)
		budgetTS := substrate.NewTaintedString(
			budgetHint,
			substrate.TaintSource{OriginTaintLevel: protocol.TaintNone},
			"system_budget_constraint",
		)
		safeHint, _ := substrate.SanitizeToSafe(budgetTS)
		b.WriteInstruction(safeHint)
	}

	return b.Build()
}

func (sm *StateMachine) onPlanFailure(sCtx protocol.StateContext, err error) (protocol.State, error) {
	return protocol.State("S_PLAN_FAILED"), perrors.New(perrors.CodeInternal, "plan: LLM fill failed")
}

func (sm *StateMachine) promptReflect(sCtx *StateContext, pCtx protocol.StateContext) []protocol.Message {
	// 有记忆系统时注入历史执行经验
	if pCtx.Mem != nil {
		if msgs, err := buildReflectContext(sm.bgCtx(), pCtx.Mem, sCtx); err == nil {
			return msgs
		}
	}

	b := NewPromptBuilder()
	if sCtx.SysEnvSnapshot != "" {
		b.WriteSystemEnvironment(sCtx.SysEnvSnapshot)
	}
	tmpl, _ := configs.LoadPromptTemplate("kernel/reflect.md", nil)
	safeInst, _ := substrate.SanitizeToSafe(substrate.NewTaintedString(
		tmpl,
		substrate.TaintSource{OriginTaintLevel: protocol.TaintNone},
		"system_prompt",
	))
	b.WriteInstruction(safeInst)

	resultTS := substrate.NewTaintedString(
		"Execution Result: "+string(sCtx.ExecuteResult),
		substrate.TaintSource{OriginTaintLevel: protocol.TaintHigh},
		"m4_execute_result",
	)
	b.WriteUserData(resultTS)
	return b.Build()
}

// bgCtx 返回用于内存检索的后台 context（不绑定任务生命周期）。
func (sm *StateMachine) bgCtx() context.Context { return context.Background() }

func (sm *StateMachine) onReflectSuccess(sCtx protocol.StateContext, fill []byte) (protocol.State, error) {
	return protocol.State("S_REFLECT_DONE"), nil
}

func (sm *StateMachine) onReflectFailure(sCtx protocol.StateContext, err error) (protocol.State, error) {
	return protocol.State("S_REFLECT_FAILED"), perrors.New(perrors.CodeInternal, "reflect: LLM fill failed")
}

// ============================================================================
// DeterministicEffect 函数——纯函数，重放时不重新调 LLM
// ============================================================================

func (sm *StateMachine) validateDAG(ctx context.Context, sCtx protocol.StateContext) (protocol.State, error) {
	// validateDAG 是纯函数存根，真正的四层校验通过 Agent.runValidateDAG 调用。
	// 这里返回 OK 是因为油门环节的真正输入（DAGModel + PolicyGate + TaintLevel）
	// 需要通过 Agent.sCtx 传递，所以该调用需要在带有完整 StateContext 的 Agent 上运行。
	// 在 DeterministicEffect.Fn 的签名限制下，我们返回占位状态；
	// 实际验证调用逻辑在 Agent.runValidateDAG 中。
	return protocol.State("S_VALIDATE_OK"), nil
}

func (sm *StateMachine) executeDAG(ctx context.Context, sCtx protocol.StateContext) (protocol.State, error) {
	// executeDAG 是纯函数存根。
	// 真正的执行在 Agent.runExecuteDAG 中，因为需要访问 a.toolRegistry。
	// S_EXECUTE 阶段拦截逻辑与 S_VALIDATE 相同，在 executeEffect 中进行。
	return protocol.State("S_EXECUTE_OK"), nil
}

func (sm *StateMachine) rollbackSaga(ctx context.Context, sCtx protocol.StateContext) (protocol.State, error) {
	// Saga 逆序补偿——已执行步骤的 Undo 操作
	return protocol.State("S_ROLLBACK_OK"), nil
}
