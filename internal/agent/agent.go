package agent

import (
	"github.com/polarisagi/polaris/internal/observability/trace"

	"github.com/polarisagi/polaris/internal/observability/metrics"

	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/agent/fsm"

	"github.com/polarisagi/polaris/internal/action/codeact"
	agentctx "github.com/polarisagi/polaris/internal/agent/context"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/internal/sysmgr/sysinfo"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// Agent 运行循环（Suspend-on-Idle 语义）
// ============================================================================

// Agent 是系统核心执行单元——一个 goroutine，空闲时挂起。
type Agent struct {
	ID                string
	taskRepo          protocol.TaskReadRepository
	intent            chan types.AgentTrigger
	sm                *fsm.StateMachine
	sCtx              *fsm.StateContext
	Config            AgentConfig
	ctx               context.Context
	cancel            context.CancelFunc
	taintGate         TaintGate
	provider          protocol.Provider             // LLM 调用入口（由 M1 提供）
	policyGate        protocol.PolicyGate           // Cedar 策略引擎（由 M11 提供）
	hitl              protocol.HITL                 // 人工审批网关
	toolRegistry      protocol.ToolRegistry         // 工具注册表（由 M7 提供）
	memory            protocol.Memory               // 四层记忆系统（由 M5 提供）
	worldModel        WorldModel                    // 认知世界模型，nil 时安全降级
	prm               *DefaultPRM                   // 可选；nil 时跳过多候选打分
	blindZoneDetector BlindZoneDetector             // 可选；nil 时跳过盲区检查
	scorer            *stepScorer                   // Adaptive Max-Steps 打分器
	whisperChan       <-chan protocol.MemoryWhisper // 接收 MemoryAgent 耳语（只读）
	whisperSendChan   chan<- protocol.MemoryWhisper // PlannerPool 推送端
	plannerSpawner    func(ctx context.Context, goal, taskType string, provider protocol.Provider)
	outboxWriter      protocol.OutboxWriter
	piiVault          *agentctx.SessionPIIVault // PII 快照，nil 时跳过（Tier0 无加密密钥场景）
	extQuerier        protocol.SQLQuerier       // 用于查询已安装扩展；独立字段避免对 taskRepo 做错误类型断言
	toolCallRecorder  ToolCallRecorder          // 可选；工具调用成功录制（M9 Logic Collapse 触发器）
	memInjector       MemoryInjector            // NEW: 组装前主动记忆注入
	codeAct           *codeact.CodeAct          // LLM 代码执行引擎；nil 时 code_act 节点返回错误
}

// MemoryInjector 定义在消息组装前主动检索并注入相关记忆的接口。
type MemoryInjector interface {
	InjectRelevantMemory(ctx context.Context, sessionID string, query string) (string, error)
}

type AgentConfig struct {
	MaxReplan      int
	DefaultBudget  int
	MaxSteps       int
	IdleTimeoutSec int
	// SystemTier 对应硬件层级（0=Tier0/8GB, 1+=Tier1+）。
	// L3 LLM 看门狗仅在 SystemTier >= 1 时激活。
	// 由 M3 HardwareProbe 探测结果注入。
	SystemTier int
}

// SetPIIVault 注入 PIIVault，用于 Suspend 时持久化会话 PII。
// vault 为 nil 时 Snapshot 调用被静默跳过（Tier0 无加密密钥场景）。
func (a *Agent) SetPIIVault(vault *agentctx.SessionPIIVault) {
	a.piiVault = vault
}

// SetExtQuerier 注入用于查询已安装扩展的 SQLQuerier。
// 必须在 Agent 启动前调用；传 nil 时 refreshInstalledExtensions 安全降级为空字符串。
func (a *Agent) SetExtQuerier(q protocol.SQLQuerier) {
	a.extQuerier = q
}

// SetToolCallRecorder 注入工具调用成功录制器（M9 Logic Collapse 触发器）。
// 非必须；nil 时静默跳过，不影响正常执行路径。
func (a *Agent) SetToolCallRecorder(r ToolCallRecorder) {
	a.toolCallRecorder = r
}

// SetMemoryInjector 注入主动记忆注入器。
// 必须在 Agent 启动前调用。
func (a *Agent) SetMemoryInjector(i MemoryInjector) {
	a.memInjector = i
}

// SetCodeAct 注入 CodeAct 引擎，在 Agent 创建后 kernel 启动前调用。
func (a *Agent) SetCodeAct(ca *codeact.CodeAct) { a.codeAct = ca }

// BlindZoneDetector 盲区探测器接口，打破 L1 到 L2 的依赖。
type BlindZoneDetector interface {
	RecordProduction(taskType string)
	IsBlindZone(ctx context.Context, taskType string) bool
}

// ToolCallRecorder 工具调用成功记录器（consumer-side，防 L1→L2 包循环）。
// 实现由 cmd/polaris/main.go 通过 *si.LogicCollapseMonitor 适配器提供，
// Agent 通过 SetToolCallRecorder 注入；nil 时静默跳过。
type ToolCallRecorder interface {
	RecordToolSuccess(ctx context.Context, toolName string)
}

func NewAgent(id string, taskRepo protocol.TaskReadRepository, taintGate TaintGate, provider protocol.Provider) *Agent {
	ctx, cancel := context.WithCancel(context.Background())
	wCh := make(chan protocol.MemoryWhisper, 4) // 缓冲 4 条，防 PlannerPool 阻塞
	tracker := fsm.NewEpochTracker()
	// 挂载漂移告警：记录到 slog，供 M3 指标下游消费
	metrics.GlobalPerformanceDrift().OnDrift = func(alert metrics.DriftAlert) {
		slog.Warn("kernel: performance drift detected",
			"current_rate", alert.CurrentRate,
			"baseline_rate", alert.BaselineRate,
			"relative_drop", alert.RelativeDrop,
			"window_size", alert.WindowSize)
	}

	return &Agent{
		ID:       id,
		taskRepo: taskRepo,
		intent:   make(chan types.AgentTrigger, 10),
		sm:       fsm.NewStateMachine(&agentContextBuilder{}),
		sCtx: &fsm.StateContext{
			AgentID:        id,
			MaxReplan:      3,
			SysEnvSnapshot: sysinfo.GetSystemInfo().FormatMarkdown(),
			WhisperChan:    wCh,
			EpochTracker:   tracker,
		},
		ctx:             ctx,
		cancel:          cancel,
		taintGate:       taintGate,
		provider:        provider,
		scorer:          newDefaultStepScorer(),
		whisperChan:     wCh,
		whisperSendChan: wCh,
	}
}

// WorldModel 定义了认知模型所需的知识接地感知接口。
type WorldModel interface {
	AssessGrounding(ctx context.Context, task string, contextText string) (bool, string)
}

// InjectWorldModel 注入认知世界模型
func (a *Agent) InjectWorldModel(wm WorldModel) {
	a.worldModel = wm
}

// NewAgentWithPolicyGate 创建带策略引擎的 Agent（主要用于生产环境）。
func NewAgentWithPolicyGate(id string, taintGate TaintGate, provider protocol.Provider, policyGate protocol.PolicyGate) *Agent {
	a := NewAgent(id, nil, taintGate, provider)
	a.policyGate = policyGate
	return a
}

func NewAgentWithDefaults(id string) *Agent {
	return NewAgent(id, nil, &defaultTaintGate{threshold: 2}, nil)
}

// Run 启动 Agent 事件循环（Suspend-on-Idle）。
// 空闲时阻塞在 intent channel 上，不轮询——符合 par_inv_05。
//
//nolint:gocyclo
func (a *Agent) Run(ctx context.Context) error {
	// 从 AgentConfig 初始化步骤预算（仅在首次 Run 时设置，支持外部注入覆盖）
	if a.Config.MaxSteps > 0 && a.sCtx.MaxStepsLimit == 0 {
		a.sCtx.MaxStepsLimit = a.Config.MaxSteps
		a.sCtx.InitialMaxStepsLimit = a.Config.MaxSteps
	}
	idleTimeout := a.Config.IdleTimeoutSec
	if idleTimeout <= 0 {
		idleTimeout = 300
	}
	idleTimer := time.NewTimer(time.Duration(idleTimeout) * time.Second)
	defer idleTimer.Stop()

	for {
		// 动态加载已安装插件信息
		a.refreshInstalledExtensions(ctx)

		select {
		case trigger := <-a.intent:
			idleTimer.Reset(time.Duration(idleTimeout) * time.Second)
			// Adaptive Max-Steps: 步骤计数 + 预算熔断
			a.sCtx.StepsUsed++
			if a.sCtx.MaxStepsLimit > 0 && a.sCtx.StepsUsed > a.sCtx.MaxStepsLimit {
				a.sm.ForceState(types.AgentStateFailed)
				return apperr.New(apperr.CodeInternal,
					fmt.Sprintf("MAX_STEPS_EXCEEDED: steps %d > limit %d",
						a.sCtx.StepsUsed, a.sCtx.MaxStepsLimit))
			}

			effects, err := a.sm.Dispatch(ctx, a.sCtx, trigger)
			if err != nil {
				if errors.Is(err, fsm.ErrReplanExhausted) {
					// sm.Dispatch 内部已经将状态转移至 S_FAILED，此处直接返回该错误
					return apperr.Wrap(apperr.CodeInternal, "Agent.Run", err)
				}
				// context 取消由 M8 Reaper 触发——直接退出，不触发 S_ROLLBACK
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return apperr.Wrap(apperr.CodeInternal, "Agent.Run", err)
			}

			// 执行 Effects: LLMFillEffect → 调 LLM；DeterministicEffect → 直接执行
			for _, effect := range effects {
				if err := a.executeEffect(ctx, effect); err != nil {
					return apperr.Wrap(apperr.CodeInternal, "Agent.Run", err)
				}
			}

			// 终态检查
			current := a.sm.Current()
			if current == types.AgentStateComplete || current == types.AgentStateFailed {
				// M3 埋点：任务终态记录（驱动 polaris_task_success_rate）
				trace.RecordTaskOutcome(ctx, current == types.AgentStateComplete)

				// 接入运行时质量漂移检测（M03 §10.1）
				score := 1.0
				if current == types.AgentStateFailed {
					score = 0.0
				}
				metrics.GlobalPerformanceDrift().Record(score)

				// M4 §8：终态 PII 清零——SecureZero 删除 DB 快照，防止 PII 留存（M11 HE-Rule-2）
				if a.piiVault != nil && a.sCtx.TaskID != "" {
					if zeroErr := a.piiVault.SecureZero(ctx, a.sCtx.TaskID); zeroErr != nil {
						slog.WarnContext(ctx, "agent: pii secure zero failed", "task_id", a.sCtx.TaskID, "err", zeroErr)
					}
				}

				return nil
			}

		case <-idleTimer.C:
			if a.sm.Current() == types.AgentStateSuspended {
				// 已经在 Suspended 状态，静默等待意图唤醒
				continue
			}
			// Suspend-on-Idle：持久化状态后退出，由上层 Supervisor 决定是否重启
			if _, err := a.sm.Dispatch(ctx, a.sCtx, types.TriggerSuspend); err != nil {
				slog.Warn("kernel: suspend transition failed", "err", err)
				a.sm.ForceState(types.AgentStateSuspended) // fallback
			}
			return ErrIdleTimeout
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// stateToTriggerMap 将下层 Effect 产生的文本 State 映射回 FSM 驱动所需的 AgentTrigger。
// READ-ONLY: 返回的 map 在调用方不得修改。
func stateToTriggerMap() map[types.State]types.AgentTrigger {
	return map[types.State]types.AgentTrigger{
		"S_PERCEIVE_DONE":   types.TriggerPerceiveDone,
		"S_PERCEIVE_FAILED": types.TriggerReplanExhausted, // 早期失败直接熔断
		"S_PLAN_DONE":       types.TriggerPlanDone,
		"S_PLAN_FAILED":     types.TriggerReplanExhausted,
		"S_VALIDATE_OK":     types.TriggerValidateOk,
		"S_VALIDATE_FAIL":   types.TriggerValidateFail,
		"S_EXECUTE_OK":      types.TriggerExecuteDone,
		"S_EXECUTE_FAIL":    types.TriggerExecuteFail,
		"S_REPLAN_DONE":     types.TriggerReplanDone,
		"S_REPLAN_FAILED":   types.TriggerReplanExhausted,
		"S_REFLECT_DONE":    types.TriggerReflectDone,
		"S_REFLECT_FAILED":  types.TriggerReplanExhausted,
		"S_ROLLBACK_OK":     types.TriggerRollbackDone,
	}
}

// executeEffect 执行单个 Effect。
// LLMFillEffect — 调 LLM → OnSuccess/OnFailure 推进状态；
// DeterministicEffect — 调用纯函数。
// 内部助手：映射内部状态至协议状态，并在此时提权计算最大污点，防止 Taint Washing
func (a *Agent) toProtocolCtx() protocol.StateContext {
	maxTaint := types.TaintNone
	if a.sCtx != nil {
		maxTaint = a.sCtx.GlobalTaintLevel
		if lv := a.sCtx.RawIntentTS.Level(); lv > maxTaint {
			maxTaint = lv
		}
	}
	return protocol.StateContext{
		AgentID:              a.ID,
		SessionID:            a.sCtx.SessionID,
		MaxTaintLevel:        maxTaint,
		Mem:                  a.memory,
		Tools:                a.toolRegistry,
		Provider:             a.provider,
		Policy:               a.policyGate,
		Preferences:          a.sCtx.Preferences,
		SagaLog:              a.sCtx.SagaLog,
		InitialMaxStepsLimit: a.sCtx.InitialMaxStepsLimit,
	}
}

// InjectProvider 注入 LLM Provider（运行时绑定，支持热替换）。
func (a *Agent) InjectProvider(p protocol.Provider) { a.provider = p }

// InjectPRM 注入过程奖励模型（可选）。注入后 S_PLAN 阶段对复杂任务启用多候选打分。
func (a *Agent) InjectPRM(p *DefaultPRM) { a.prm = p }

// InjectBlindZoneDetector 注入认知盲区探测器（可选）。
// 注入后，S_PLAN 阶段对生产出现≥5次但 MEMF 零记录的任务类型强制 System2 路由。
func (a *Agent) InjectBlindZoneDetector(d BlindZoneDetector) {
	a.blindZoneDetector = d
}

// InjectPolicyGate 注入 Cedar PolicyGate（允许运行时替换，例如用于单元测试注入 mock）。
func (a *Agent) InjectPolicyGate(pg protocol.PolicyGate) { a.policyGate = pg }

// InjectHITL 注入人工审批网关。
func (a *Agent) InjectHITL(hitl protocol.HITL) { a.hitl = hitl }

// InjectWhisperChan 注入耳语接收通道（由顶层 wire 调用，可 nil）。
func (a *Agent) InjectWhisperChan(ch <-chan protocol.MemoryWhisper) {
	a.whisperChan = ch
	if a.sCtx != nil {
		a.sCtx.WhisperChan = ch
	}
}

// GetWhisperChan 返回 whisper 推送通道，供 PlannerPool 将最佳结果推回主脑。
// 注意：whisperChan 为 receive-only，PlannerPool 持有 send-only 端。
func (a *Agent) GetWhisperChan() chan<- protocol.MemoryWhisper {
	return a.whisperSendChan
}

// InjectPlannerSpawner 注入 PlannerPool 构造器，打破循环依赖
func (a *Agent) InjectPlannerSpawner(fn func(ctx context.Context, goal, taskType string, provider protocol.Provider)) {
	a.plannerSpawner = fn
}

// InjectOutboxWriter 注入 OutboxWriter
func (a *Agent) InjectOutboxWriter(ow protocol.OutboxWriter) {
	a.outboxWriter = ow
}

// SetTaskID 由 Worker 在调用 Run() 前注入 Blackboard task_id，供内核写 tasks 表时使用。
func (a *Agent) SetTaskID(id string) {
	a.sCtx.TaskID = id
	if a.taskRepo != nil && id != "" {
		count, err := a.taskRepo.GetTaskProviderSuspendCount(context.Background(), id)
		if err == nil {
			a.sCtx.ProviderSuspendCount = count
		}
	}
}

// SetTaskIntent 设置任务意图（供 M8 Orchestrator 注入黑板任务信息）。
func (a *Agent) SetTaskIntent(intent []byte) {
	intentStr := string(intent)
	if a.sCtx.TaskModel == nil {
		a.sCtx.TaskModel = &fsm.TaskModel{
			Goal: intentStr,
		}
	}
	a.sCtx.RawIntentTS = taint.NewTaintedString(
		intentStr,
		taint.TaintSource{
			Module:           "m8_orchestrator",
			OriginTaintLevel: types.TaintHigh,
		},
		"task_intent_input",
	)

	// 单调递增全局污点（只升不降）
	if lv := a.sCtx.RawIntentTS.Level(); lv > a.sCtx.GlobalTaintLevel {
		a.sCtx.GlobalTaintLevel = lv
	}

	{
		// 统一用 []string 消除中间 string 变量，fsm.TaskModel 走 strings.Fields，
		// 无 fsm.TaskModel 时走 RawIntentTS.Fields()（避免裸 .Content() 调用）。
		var goalWords []string
		if a.sCtx.TaskModel != nil {
			goalWords = strings.Fields(a.sCtx.TaskModel.Goal)
		} else {
			goalWords = a.sCtx.RawIntentTS.Fields()
		}
		if len(goalWords) > 8 {
			goalWords = goalWords[:8]
		}
		toolSeq := append([]string{"intent"}, goalWords...)
		a.sCtx.SurpriseIndex = metrics.GlobalSurpriseIndex().ComputeBasic(
			context.Background(),
			nil,
			toolSeq,
		)
	}
}

// GetExecuteResult 获取执行成果（供 M8 Orchestrator 写回黑板）。
func (a *Agent) GetExecuteResult() []byte {
	return a.sCtx.ExecuteResult
}

// GetTokenUsage 返回本轮执行的分项 token 消耗（Gap-A）。
// 由 Worker.tryClaimAndExecute 在 Run 返回后调用，写入 Blackboard.UpdateTaskTokens。
func (a *Agent) GetTokenUsage() (tokensIn, tokensOut, cacheRead int) {
	return a.sCtx.TokensInput, a.sCtx.TokensOutput, a.sCtx.TokensCacheRead
}

// InjectToolRegistry 注入工具注册表（运行时绑定，允许测试注入 mock）。
func (a *Agent) InjectToolRegistry(tr protocol.ToolRegistry) { a.toolRegistry = tr }

// InjectMemory 注入记忆系统（运行时绑定，允许测试注入 mock）。
func (a *Agent) InjectMemory(mem protocol.Memory) { a.memory = mem }

// SetCognitiveSearcher 注入 L2 语义记忆检索器
func (a *Agent) SetCognitiveSearcher(cs fsm.CognitiveSearcher) {
	a.sCtx.Cognitive = cs
}

// SetKnowledgeSearcher 注入 RAG 知识检索器 (M10)
func (a *Agent) SetKnowledgeSearcher(ks fsm.KnowledgeSearcher) {
	a.sCtx.KnowledgeSearcher = ks
}

// Memory 返回 Agent 挂载的物理记忆实例
func (a *Agent) Memory() protocol.Memory { return a.memory }

// SetPreferences 注入用户配置偏好（如 computer_use_mode）。
func (a *Agent) SetPreferences(prefs map[string]string) {
	if a.sCtx.Preferences == nil {
		a.sCtx.Preferences = make(map[string]string)
	}
	for k, v := range prefs {
		a.sCtx.Preferences[k] = v
	}
}

// AgentID 返回 Agent ID.
func (a *Agent) AgentID() string { return a.ID }

// CurrentState 返回 FSM 当前状态.
func (a *Agent) CurrentState() types.AgentState {
	return a.sm.Current()
}

// ConfigInfo 返回 Agent 运行配置.
func (a *Agent) ConfigInfo() map[string]any {
	return map[string]any{
		"max_replan":     a.Config.MaxReplan,
		"default_budget": a.Config.DefaultBudget,
		"max_steps":      a.Config.MaxSteps,
	}
}

// SendIntent 向 Agent 发送意图触发脉冲。
func (a *Agent) SendIntent(trigger types.AgentTrigger) error {
	select {
	case a.intent <- trigger:
		return nil
	case <-time.After(50 * time.Millisecond):
		return apperr.New(apperr.CodeInternal, "SendIntent timeout")
	}
}

// SurpriseIndex 返回最近一次计算的 SurpriseIndex。
func (a *Agent) SurpriseIndex() float64 {
	return a.sCtx.SurpriseIndex
}

// Interrupt 向 Agent 发送中断请求（非阻塞，inv_global_08 <200ms SLO）。
// Resume → 恢复原状态；Redirect → 更新意图后恢复（重新规划）；Abort → S_FAILED。
func (a *Agent) Interrupt(req types.InterruptRequest) {
	a.sCtx.InterruptReq = &req
	switch req.Action {
	case types.InterruptRedirect:
		// 更新意图，Resume 后从当前状态重新规划
		if req.Redirect != "" {
			a.sCtx.RawIntentTS = taint.NewTaintedString(
				req.Redirect,
				taint.TaintSource{OriginTaintLevel: types.TaintHigh},
				"user_interrupt_redirect",
			)
		}
		_ = a.SendIntent(types.TriggerInterruptReceived)
		// 注入到 S_INTERRUPT 后立即 Resume（Redirect = 新意图的 Resume）
		go func() { _ = a.SendIntent(types.TriggerInterruptResume) }()
	case types.InterruptAbort:
		_ = a.SendIntent(types.TriggerInterruptReceived)
		go func() { _ = a.SendIntent(types.TriggerInterruptAbort) }()
	default: // types.InterruptResume
		_ = a.SendIntent(types.TriggerInterruptReceived)
		go func() { _ = a.SendIntent(types.TriggerInterruptResume) }()
	}
}

// Shutdown 关闭 Agent，取消 context。
func (a *Agent) Shutdown() { a.cancel() }

// ContextCancel 返回 Agent 的 cancel 函数（供 M8 Reaper 终止过期任务）。
func (a *Agent) ContextCancel() context.CancelFunc { return a.cancel }

// fsm.StateMachine 返回 Agent 的状态机（供外部检查状态）。
func (a *Agent) StateMachine() *fsm.StateMachine { return a.sm }

// ============================================================================
// TaintGate
// ============================================================================

type TaintGate interface {
	IsClean(level int) bool
	Gate(level int) error
}

type defaultTaintGate struct{ threshold int }

func (g *defaultTaintGate) IsClean(level int) bool { return level < g.threshold }
func (g *defaultTaintGate) Gate(level int) error {
	if level >= g.threshold {
		return apperr.ErrTaintViolation
	}
	return nil
}

// ============================================================================
// 错误类型
// ============================================================================

var (
	ErrReplanExhausted = apperr.New(apperr.CodeResourceExhausted, "replan guard: max replan count reached, escalate to HITL")
	ErrIdleTimeout     = apperr.New(apperr.CodeResourceExhausted, "agent idle timeout")
)

// isTerminalState 判断是否为终态（S_COMPLETE 或 S_FAILED）。

// refreshInstalledExtensions 从 extension_instances 表动态查询已安装扩展并存入 fsm.StateContext。
func (a *Agent) refreshInstalledExtensions(ctx context.Context) {
	if a.extQuerier == nil {
		a.sCtx.InstalledExtensionsInfo = ""
		return
	}

	rows, err := a.extQuerier.QueryContext(ctx,
		"SELECT ext_type, name, publisher FROM extension_instances WHERE status = 'installed'")
	if err != nil {
		return
	}
	defer rows.Close()

	var exts []string
	for rows.Next() {
		var extType, name, pub string
		if err := rows.Scan(&extType, &name, &pub); err == nil {
			exts = append(exts, fmt.Sprintf("- [%s] %s/%s", extType, pub, name))
		}
	}

	if rows.Err() != nil {
		return
	}

	if len(exts) > 0 {
		a.sCtx.InstalledExtensionsInfo = "Installed Extensions:\n" + strings.Join(exts, "\n")
	} else {
		a.sCtx.InstalledExtensionsInfo = ""
	}
}

// InjectExtensionActivator 注入按需扩展激活器。
func (a *Agent) InjectExtensionActivator(activator fsm.ExtensionActivatorIface) {
	a.sm.WithExtensionActivator(activator)
}

type agentContextBuilder struct{}

func (b *agentContextBuilder) BuildPerceiveContext(ctx context.Context, memory protocol.Memory, sCtx *fsm.StateContext, cognitive fsm.CognitiveSearcher) ([]types.Message, error) {
	return agentctx.BuildPerceiveContext(ctx, memory, sCtx, cognitive)
}

func (b *agentContextBuilder) BuildPlanContext(ctx context.Context, memory protocol.Memory, sCtx *fsm.StateContext, tools protocol.ToolRegistry, cognitive fsm.CognitiveSearcher) ([]types.Message, error) {
	return agentctx.BuildPlanContext(ctx, memory, sCtx, tools, cognitive)
}

func (b *agentContextBuilder) BuildReflectContext(ctx context.Context, memory protocol.Memory, sCtx *fsm.StateContext) ([]types.Message, error) {
	return agentctx.BuildReflectContext(ctx, memory, sCtx)
}

func (b *agentContextBuilder) BuildToolListSection(tools protocol.ToolRegistry) string {
	return agentctx.BuildToolListSection(tools)
}
