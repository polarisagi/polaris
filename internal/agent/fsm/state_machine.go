package fsm

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// StateMachine 持有控制流。LLM 是概率协处理器——Go 状态机确定性推进，LLM 仅做结构化填空。
// 权威规约: spec/state.yaml §m4_par_state_machine
// 架构文档: docs/arch/M04-Agent-Kernel.md §1

// Transition 是状态机中一条确定性边。
// LLM 仅在 LLMFillEffect 执行时调用，而非 Transition 自身。
type Transition struct {
	From    types.AgentState
	Trigger types.AgentTrigger
	To      types.AgentState
	Guard   func(ctx context.Context, sCtx *StateContext) bool
	// Effects 返回此转移产生的副作用（DeterministicEffect 或 LLMFillEffect）。
	Effects func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error)
}

// StateMachine 管理 Agent 状态生命周期。
type StateMachine struct {
	cb                         ContextBuilder
	current                    types.AgentState
	transitions                map[types.AgentState]map[types.AgentTrigger]Transition // state → trigger → transition
	history                    []types.AgentState
	replanCount                int
	eventSeq                   int64 // 单调递增事件序列号，用于生成确定性 Event ID（replay key）
	startedAt                  time.Time
	interruptFrom              types.AgentState // S_INTERRUPT 时记录被中断的原状态（Resume 路径用）
	mu                         sync.Mutex
	hintsMu                    sync.Mutex
	activator                  ExtensionActivatorIface
	dynamicHints               []ExtActivatedHint
	toolHintProvider           ToolHintProvider
	replanExtActivationTimeout time.Duration        // S_REPLAN 扩展激活 Effect 超时上限，见 WithReplanExtensionActivationTimeout
	stashedTriggers            []types.AgentTrigger // P0-5: 暂存中断期间的事件
	intentDispatcher           func(types.AgentTrigger)
}

const (
	defaultReplanExtActivationTimeout = 3 * time.Second // replanExtActivationTimeout 未被注入时的兜底值
	maxStashedTriggers                = 50
)

// StateContext 穿越状态机各转移的共享上下文（与 protocol.StateContext 互补）。
type StateContext struct {
	AgentID   string
	SessionID string
	TaskID    string // 当前认领的 Blackboard task_id；由 Worker 在 Run() 前通过 SetTaskID() 注入
	// NamespaceID 协同任务共享记忆命名空间（GD-14-001，见 docs/arch/decisions ADR）。
	// 空值 = 不共享，行为与引入本字段前完全一致。非空时，episodic 写入的分区键
	// （复用现有 types.Event.TaskID / types.EpisodicQuery.SessionID 字段承载，
	// 见 internal/memory/store/episodic_mem.go Query() 的 ev.TaskID==q.SessionID
	// 过滤逻辑）改用 NamespaceID 而非 SessionID，使同一 Namespace 下的多个 Worker
	// Agent 能检索到彼此写入的记忆片段；不同 Namespace 之间仍然隔离。
	// 由 Worker 在 Run() 前通过 SetMemoryNamespace() 注入（对应 types.TaskEntry.Namespace）。
	NamespaceID          string
	RawIntentTS          taint.TaintedString // 原始自然语言意图 (外部输入，带污点)
	TaskModel            *TaskModel          // S_PERCEIVE 产出
	DAGModel             *DAGModel           // S_PLAN 产出
	Reflection           *ReflectionModel    // S_REFLECT 产出
	ExecuteResult        []byte
	ExecuteImageParts    []types.ImagePart
	MaxReplan            int
	Timeout              time.Duration
	StartedAt            time.Time
	WhisperChan          <-chan protocol.MemoryWhisper // 异步接收 MemoryAgent 线索
	ProviderSuspendCount int                           // 连续无可用 provider 失败次数
	DegradedReplan       bool                          // 收到全 provider 熔断时，图剪枝并要求降级重规划

	ContextEpoch int64         // B2: 记录当前 Prompt 序列的版本号
	EpochTracker *epochTracker // B2: tracker 实例

	// Inference Budget 控制
	TokenBudget int
	TokensUsed  int
	// BudgetWarned / BudgetPressure 防重复日志标记（仅本轮 Agent 生命周期内有效）。
	// BudgetWarned: 已越过 50% 告警水位（记录一次日志，不改变行为）。
	// BudgetPressure: 已越过 75% 压力水位（S_PLAN 收紧 DAG 规模）。
	BudgetWarned   bool
	BudgetPressure bool

	// Budget 会话级预算控制器（Task 11: BudgetManager 接入主控制流）。
	// nil 时向后兼容，跳过会话级预算检查，仍用内联 TokenBudget 逻辑。
	// 注入点: Agent.SetBudget() 在 Worker.tryClaimAndExecute 前调用。
	Budget BudgetController

	// MonthlyBudgetUSDConfig 来自配置项，0 = 不限额（不向 Cedar budget_cap 传入约束）。
	MonthlyBudgetUSDConfig float64

	// Token 分项记账（Gap-A, HE-Rule-1）。
	// Worker.tryClaimAndExecute 在 Run 返回后读取这三个字段，写入 Blackboard。
	// TokensUsed 保持不变（= TokensInput + TokensOutput），兼容现有预算逻辑。
	TokensInput     int
	TokensOutput    int
	TokensCacheRead int

	// Step Budget 控制（Adaptive Max-Steps）
	// MaxStepsLimit 由 AgentConfig.MaxSteps 初始化；StepScorer 低分时动态收紧。
	// 0 = 无上限（不推荐用于生产）。
	StepsUsed            int
	MaxStepsLimit        int
	InitialMaxStepsLimit int              // 原始步骤上限 (ISSUE-08)
	SagaLog              []types.SagaStep // Saga 记录 (ISSUE-03)

	// KillThrottle 降级标记（M03 §5 ThrottlePolicy）。
	// KillThrottle 阶段生效：MaxStepsLimit 被收紧至 3，网络写工具被拒绝。
	ThrottleNoNetwork bool

	// 认知状态
	SurpriseIndex float64

	// ReasoningState 跨轮次持久化的推理状态（M04 §7.1 + M05 §3.1）。
	// S_REFLECT 阶段产出，下轮 S_PERCEIVE 时注入 ContextWindow。
	ReasoningState []byte

	// GlobalTaintLevel 跨轮次累积的最高污点等级（只升不降，ADR-0007）。
	// 覆盖场景：多轮任务、记忆召回、跨会话 ReasoningState 注入。
	GlobalTaintLevel types.TaintLevel

	// 偏好配置
	Preferences map[string]string

	// 挂起原因（如 capability_gap）
	SuspendReason string

	// SysEnvSnapshot 是启动时获取的系统静态快照，注入到每个 Prompt 头部
	SysEnvSnapshot string

	// InstalledExtensionsInfo 包含当前系统已安装的扩展清单
	InstalledExtensionsInfo string

	// Cognitive 语义检索接口（L2）
	Cognitive CognitiveSearcher

	// KnowledgeSearcher 知识 RAG 检索接口（M10）
	KnowledgeSearcher KnowledgeSearcher

	// LastReasoningContent 上一轮 LLM 在 thinking 模式下产出的推理内容。
	// 由 agent_execute.go 在成功 Infer 后写入，供下一轮 PromptFn 注入消息历史。
	LastReasoningContent string

	// GroundingGap 记录知识接地的缺口信息，由 WorldModel.AssessGrounding 产出，用于注入 Prompt
	GroundingGap string

	// BlindZoneHITLRequired 标记本次任务被 BlindZoneDetector 判定为盲区候选。
	// S_PLAN 阶段写入（agent_execute.go），S_VALIDATE 阶段读取以触发 HITL 检查点。
	BlindZoneHITLRequired bool

	// SkillVersions 记录已注入的技能版本，用于验证版本单调性
	SkillVersions map[string]int64
}

// TaskModel LLM 填槽产出——将自然语言任务结构化。
type TaskModel struct {
	Goal        string
	SubTasks    []string
	Constraints []string
	Complexity  float64
}

// DAGModel LLM 填槽产出——可执行的有向无环图。
// 权威类型 protocol.ExecNode/protocol.ExecEdge 定义见 internal/protocol/dag_node.go
// （2026-07-12 随 internal/execute 模块化，fsm 包不再直接 import execute/dag，
// 改为直接引用 protocol 共享类型，二者本就是同一底层类型的别名，无行为变化）。
type DAGModel struct {
	Nodes []protocol.ExecNode
	Edges []protocol.ExecEdge
}

// ReflectionModel LLM 填槽产出——执行后反思。
type ReflectionModel struct {
	GoalAchieved bool
	Errors       []string
	Learnings    []string
}

func NewStateMachine(cb ContextBuilder) *StateMachine {
	sm := &StateMachine{
		cb:                         cb,
		current:                    types.AgentStateIdle,
		transitions:                make(map[types.AgentState]map[types.AgentTrigger]Transition),
		history:                    make([]types.AgentState, 0),
		startedAt:                  time.Now(),
		replanExtActivationTimeout: defaultReplanExtActivationTimeout,
		stashedTriggers:            make([]types.AgentTrigger, 0),
	}
	sm.registerTransitions()
	return sm
}

// NextEventID 生成确定性事件 ID：{session_id}:{seq}:{event_type}
// 满足 inv_M4_02 重放确定性要求——同 session+seq → 同 ID，不依赖 wall clock。
func (sm *StateMachine) NextEventID(sessionID, eventType string) string {
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

func (sm *StateMachine) Current() types.AgentState {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.current
}

func (sm *StateMachine) ReplanCount() int {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.replanCount
}

// SetContextBuilder sets the ContextBuilder for the state machine.
func (sm *StateMachine) SetContextBuilder(cb ContextBuilder) {
	sm.cb = cb
}

func (sm *StateMachine) SetIntentDispatcher(dispatcher func(types.AgentTrigger)) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.intentDispatcher = dispatcher
}

// Dispatch 接收触发事件，查找匹配转移，执行 guard + effects，推进状态。
// 返回的 effects 由 Agent.Run 消费——LLMFillEffect 调 LLM，DeterministicEffect 直接执行。
func (sm *StateMachine) Dispatch(ctx context.Context, sCtx *StateContext, trigger types.AgentTrigger) ([]protocol.Effect, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	current := sm.current

	// ── S_INTERRUPT 通用处理（优先于 transitions 表）──────────────────────────
	// 任意活跃态（非终态、非 S_INTERRUPT 自身）均可接收中断信号。
	if trigger == types.TriggerInterruptReceived {
		if !isTerminalState(current) && current != types.AgentStateInterrupt {
			sm.interruptFrom = current
			sm.history = append(sm.history, current)
			sm.current = types.AgentStateInterrupt
			return nil, nil
		}
	}

	// S_INTERRUPT 出边：Resume → 恢复原状态；Abort → S_FAILED
	if current == types.AgentStateInterrupt {
		switch trigger {
		case types.TriggerInterruptResume:
			sm.history = append(sm.history, current)
			sm.current = sm.interruptFrom

			if len(sm.stashedTriggers) > 0 {
				toRequeue := make([]types.AgentTrigger, len(sm.stashedTriggers))
				copy(toRequeue, sm.stashedTriggers)
				sm.stashedTriggers = sm.stashedTriggers[:0]
				if sm.intentDispatcher != nil {
					concurrent.SafeGo(context.Background(), "fsm.requeue_stashed", func(ctx context.Context) {
						for _, tr := range toRequeue {
							sm.intentDispatcher(tr)
						}
					})
				}
			}
			return nil, nil
		case types.TriggerInterruptAbort:
			sm.history = append(sm.history, current)
			sm.current = types.AgentStateFailed
			if len(sm.stashedTriggers) > 0 {
				slog.Info("fsm: discarding stashed triggers due to abort", "count", len(sm.stashedTriggers))
				sm.stashedTriggers = sm.stashedTriggers[:0]
			}
			return nil, nil
		default:
			// P0-5: S_INTERRUPT 状态下收到非预期的 trigger 时，暂存到 stashedTriggers
			if len(sm.stashedTriggers) >= maxStashedTriggers {
				slog.Error("fsm: stashed triggers exceeded max capacity, dropping oldest", "max", maxStashedTriggers)
				sm.stashedTriggers = sm.stashedTriggers[1:]
			}
			sm.stashedTriggers = append(sm.stashedTriggers, trigger)
			slog.Debug("fsm: stashed trigger during S_INTERRUPT", "trigger", trigger)
			return nil, nil
		}
	}
	// ─────────────────────────────────────────────────────────────────────────

	triggerMap, ok := sm.transitions[current]
	if !ok {
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("no transitions from state %v", current))
	}

	t, ok := triggerMap[trigger]
	if !ok {
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("no transition from %v with trigger %v", current, trigger))
	}

	// Guard 检查
	if t.Guard != nil && !t.Guard(ctx, sCtx) {
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("guard rejected transition %v → %v", current, t.To))
	}

	// 执行 Effects（LLMFillEffect | DeterministicEffect）
	effects, err := t.Effects(ctx, sCtx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("transition %v → %v failed", current, t.To), err)
	}

	// 特殊处理: S_REPLAN 计数 + 耗尽检查
	if t.To == types.AgentStateReplan {
		sm.replanCount++

		// S_REPLAN：尝试按需激活未加载的扩展，补充工具集后重规划。
		// 仅在第一次 replan 时触发（避免每次 replan 都触发语义搜索）。
		goalToActivate, needActivate := sm.shouldActivateExtensions(sCtx)

		if sm.replanCount >= sCtx.MaxReplan {
			// replan 耗尽 → 自动进阶 S_FAILED，返回 ErrReplanExhausted
			sm.history = append(sm.history, current, t.To)
			sm.current = types.AgentStateFailed
			return nil, ErrReplanExhausted
		}

		sm.history = append(sm.history, current)
		sm.current = t.To

		if needActivate {
			slog.Debug("kernel: returning deterministic effect for extension activation", "goal", goalToActivate)

			if sm.intentDispatcher != nil {
				concurrent.SafeGo(context.Background(), "fsm.replan_extension_activation", func(actCtx context.Context) {
					actCtx, cancel := context.WithTimeout(actCtx, sm.replanExtActivationTimeout)
					defer cancel()

					hints, hintErr := sm.activator.FindAndActivate(actCtx, goalToActivate)
					if hintErr != nil {
						slog.Warn("extension_activator: failed to activate extensions for replan", "err", hintErr)
					} else if len(hints) > 0 {
						sm.hintsMu.Lock()
						sm.dynamicHints = hints
						sm.hintsMu.Unlock()
						slog.Info("extension_activator: activated extensions for replan",
							"count", len(hints),
							"goal", goalToActivate)
					}
					sm.intentDispatcher(types.TriggerReplanDone)
				})
			} else {
				// GR-4-003 修复：移除持锁期间的同步 IO fallback。
				// 原 fallback 分支在 sm.mu.Lock() 作用域内同步调用 FindAndActivate（含语义检索/IO），
				// 会阀塞其他并发 Dispatch 调用直至完成或超时，违反 HE-5（FSM 锁内禁止 IO）。
				// intentDispatcher 为 nil 是配置错误（生产环境组装一定会注入），需显式暴露而不是静默降级。
				slog.Error("fsm: intentDispatcher not configured, cannot activate extension for replan — this is a configuration error",
					"goal", goalToActivate)
				// 不做扩展激活，也不推进 ReplanDone——上层调用方有 timeout 保护。
			}

			eff := protocol.DeterministicEffect{
				Fn: func(effCtx context.Context, sCtx protocol.StateContext) (types.State, error) {
					if sm.intentDispatcher == nil {
						return "S_REPLAN_DONE", nil
					}
					return "", nil
				},
			}
			effects = append(effects, eff)
		} else {
			// 如果不需要激活，则直接返回一个空 effect 立即触发 ReplanDone
			eff := protocol.DeterministicEffect{
				Fn: func(effCtx context.Context, sCtx protocol.StateContext) (types.State, error) {
					return "S_REPLAN_DONE", nil
				},
			}
			effects = append(effects, eff)
		}
		return effects, nil
	}

	// 记录历史
	sm.history = append(sm.history, current)
	sm.current = t.To

	return effects, nil
}

// History 返回状态遍历历史。
func (sm *StateMachine) History() []types.AgentState {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	h := make([]types.AgentState, len(sm.history))
	copy(h, sm.history)
	return h
}

// Reset 重置状态机到初始状态（用于 Agent 复用时）。
func (sm *StateMachine) Reset() {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.current = types.AgentStateIdle
	sm.history = sm.history[:0]
	sm.replanCount = 0
	sm.startedAt = time.Now()
	sm.stashedTriggers = sm.stashedTriggers[:0]
}

func (sm *StateMachine) add(t Transition) {
	if sm.transitions[t.From] == nil {
		sm.transitions[t.From] = make(map[types.AgentTrigger]Transition)
	}
	sm.transitions[t.From][t.Trigger] = t
}

// budgetWarnPct / budgetCriticalPct：Token 预算分级阈值（百分比，整数）。
const (
	BudgetCriticalPct = 75
)

// ErrReplanExhausted Replan 次数耗尽
var ErrReplanExhausted = apperr.New(apperr.CodeResourceExhausted, "replan guard: max replan count reached, escalate to HITL")

func isTerminalState(s types.AgentState) bool {
	return s == types.AgentStateComplete || s == types.AgentStateFailed || s == types.AgentStateInterrupt
}

// ForceState 强制设置状态机的当前状态，并记录历史。通常用于致命异常（如超时、预算耗尽、步数超限）直接切 S_FAILED。
func (sm *StateMachine) ForceState(state types.AgentState) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.history = append(sm.history, sm.current)
	sm.current = state
}
