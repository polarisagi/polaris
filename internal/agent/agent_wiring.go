package agent

import (
	"context"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/agent/fsm"

	"github.com/polarisagi/polaris/internal/observability/metrics"

	agentctx "github.com/polarisagi/polaris/internal/agent/context"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/guard"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/internal/tool/catalog"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// SurpriseReader 读取当前 SurpriseIndex 滑动均值（consumer-side 接口）。
// 由 learning/surprise.SurpriseCalculator 实现。
type SurpriseReader interface {
	SubmitToolSeq(taskID string, toolSeq []string)
	CurrentSurprise() float64
}

// MemoryInjector 定义在消息组装前主动检索并注入相关记忆的接口。
type MemoryInjector interface {
	InjectRelevantMemory(ctx context.Context, sessionID string, query string) (string, error)
}

// SetPIIVault 注入 PIIVault，用于 Suspend 时持久化会话 PII。
// vault 为 nil 时 Snapshot 调用被静默跳过（Tier0 无加密密钥场景）。
func (a *Agent) SetPIIVault(vault *agentctx.SessionPIIVault) {
	a.piiVault = vault
}

// InjectPIITokenizer 注入 OpaqueToken 检测器和令牌库。
// 在主 Agent Kernel 构造完成后、供 Supervisor 启动前调用。
// detector/vault 均允许为 nil（优雅降级为不进行输入令牌化）。
func (a *Agent) InjectPIITokenizer(detector *guard.PIIDetector, vault *guard.PIITokenVault) {
	a.piiDetector = detector
	a.tokenVault = vault
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
func (a *Agent) SetCodeAct(ca CodeActEngine) { a.codeAct = ca }

// SetLAMEngine 注入 LAM 策略检查器（R3）。
// interceptComputerUse 在 HITL 审批前调用 CheckPolicy 对 GUI 动作做 Cedar 策略预检。
// nil-safe：未注入时跳过 Cedar 预检，仅走 HITL 审批。
func (a *Agent) SetLAMEngine(e LAMPolicyChecker) { a.lamEngine = e }

// SetBudget 注入 BudgetController（Task 11），在 Worker.tryClaimAndExecute 中任务开始前调用。
// nil-safe：不注入时跳过会话级 BudgetManager 检查，仍使用内联 TokenBudget 逻辑（向后兼容）。
func (a *Agent) SetBudget(b fsm.BudgetController) {
	a.sCtx.Budget = b
}

// SetMonthlyBudgetUSD 设置月度预算 USD 上限，供 Cedar budget_cap 规则使用。
// 0 = 不限额（不向 Cedar 传入约束）。
func (a *Agent) SetMonthlyBudgetUSD(budget float64) {
	a.sCtx.MonthlyBudgetUSDConfig = budget
}

// SetSurpriseCalc 注入完整 SurpriseCalculator，替代 ComputeBasic 基础版路由。
// nil-safe：不注入时降级为 ComputeBasic。
func (a *Agent) SetSurpriseCalc(r SurpriseReader) {
	a.surpriseCalc = r
}

// WithSkillCache 注入 ScriptSkillCache，启用 FastPath 技能命中路径。
// nil-safe：不注入时 FastPath 退回合成 JSON 路径。
func (a *Agent) WithSkillCache(sc ScriptSkillCache) *Agent {
	a.skillCache = sc
	return a
}

// WithSkillExecutor 注入 SkillExecutor，FastPath 缓存命中后实际执行 Python 脚本。
// 必须与 WithSkillCache 配合使用；单独注入任意一个均不会执行技能。
func (a *Agent) WithSkillExecutor(se protocol.SkillExecutor) *Agent {
	a.skillExecutor = se
	return a
}

// SetAssembler 注入 ContextAssembler.
func (a *Agent) SetAssembler(assembler *agentctx.Assembler) {
	a.assembler = assembler
}

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

// InjectTaintReviewChecker 注入 S_VALIDATE 阶段 TaintGate 的人工复核豁免查询器
// （M11 §2.5 SanitizeByUserReview 触发点，2026-07-14 新增；复用
// internal/security/token.ExemptionVault，与 tool.go 出口污点检查共享同一实例）。
// nil 时该降级路径不生效，不影响既有拦截行为（fail-closed）。
func (a *Agent) InjectTaintReviewChecker(c protocol.TaintReviewChecker) { a.taintReviewChecker = c }

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
func (a *Agent) SetTaskID(ctx context.Context, id string) {
	a.sCtx.TaskID = id
	if a.taskRepo != nil && id != "" {
		count, err := a.taskRepo.GetTaskProviderSuspendCount(ctx, id)
		if err == nil {
			a.sCtx.ProviderSuspendCount = count
		}
	}
}

// SetMemoryNamespace 由 Worker 在调用 Run() 前注入协同任务共享记忆命名空间
// （GD-14-001，对应 types.TaskEntry.Namespace）。ns 为空表示不共享，等同于
// 引入本机制前的默认行为。
func (a *Agent) SetMemoryNamespace(ns string) {
	a.sCtx.NamespaceID = ns
}

// memoryPartitionKey 返回 episodic 记忆写入应使用的分区键：设置了协同命名空间时
// 返回 NamespaceID（使同命名空间下的多个 Agent 共享记忆），否则返回 SessionID
// （默认行为，与引入 GD-14-001 前完全一致）。
//
// 仅用于"可协作类"记忆事件（task_perceived/plan_generated/reflection_completed/
// execution_completed）——2PC 幂等性日志（EventActionPending/EventActionDone，
// agent_execute_dag.go）与 FastPath 意图缓存（agent_execute_effect.go）故意不
// 使用本方法，继续按 SessionID 严格隔离：前者是本 Agent 自身崩溃恢复用的私有
// 记账，与其他协作 Agent 共享会造成幂等性判断误判（跨 Agent 的同名工具调用
// 被误判为"已执行"），不属于"协作记忆共享"的语义范围。
func (a *Agent) memoryPartitionKey() string {
	if a.sCtx != nil && a.sCtx.NamespaceID != "" {
		return a.sCtx.NamespaceID
	}
	if a.sCtx == nil {
		return ""
	}
	return a.sCtx.SessionID
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
		if a.surpriseCalc != nil {
			// 完整三分量异步计算（MEMF + Markov + Jaccard），CurrentSurprise 返回上一轮滑动均值
			a.surpriseCalc.SubmitToolSeq(a.sCtx.TaskID, toolSeq)
			a.sCtx.SurpriseIndex = a.surpriseCalc.CurrentSurprise()
			// 同步写入 GlobalSurpriseIndex，保持 SelectThinkingMode（transitions.go）读值一致
			metrics.GlobalSurpriseIndex().SetLastValue(a.sCtx.SurpriseIndex)
		} else {
			// [A6] 不透传 ctx：SetTaskIntent 由外部在 Agent 循环之外触发，且 ComputeBasic 为纯 CPU 同步计算，不涉及 IO 或 Trace，无需 trace ctx 传播。
			a.sCtx.SurpriseIndex = metrics.GlobalSurpriseIndex().ComputeBasic(
				context.Background(),
				nil,
				toolSeq,
			)
		}
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

// InjectToolExecutor 注入工具执行器（运行时绑定，允许测试注入 mock）。
func (a *Agent) InjectToolExecutor(te protocol.AgentToolExecutor) { a.toolRegistry = te }

// InjectCatalog 注入工具目录（运行时绑定）。
func (a *Agent) InjectCatalog(c catalog.Catalog) {
	a.catalog = c
	a.sm.SetContextBuilder(&agentContextBuilder{cata: c})
}

// InjectMemory 注入记忆系统（运行时绑定，允许测试注入 mock）。
func (a *Agent) InjectMemory(mem protocol.MemoryFacade) { a.memory = mem }

// SetCognitiveSearcher 注入 L2 语义记忆检索器
func (a *Agent) SetCognitiveSearcher(cs fsm.CognitiveSearcher) {
	a.sCtx.Cognitive = cs
}

// SetKnowledgeSearcher 注入 RAG 知识检索器 (M10)
func (a *Agent) SetKnowledgeSearcher(ks fsm.KnowledgeSearcher) {
	a.sCtx.KnowledgeSearcher = ks
}

// Memory 返回 Agent 挂载的物理记忆实例
func (a *Agent) Memory() protocol.MemoryFacade { return a.memory }

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

// asyncIntent 异步发送意图触发脉冲，一次性触发派发，不等待/不关心结果
// （调用方原为 `go func() { _ = a.SendIntent(trigger) }()` 内联样板，全仓
// 约 15 处重复；提炼为单一方法，一是消重，二是把"未来给这类一次性派发加
// SafeGo"的改动点收敛到一处，而不必逐个内联 goroutine 修改）。
func (a *Agent) asyncIntent(trigger types.AgentTrigger) {
	concurrent.SafeGo(a.ctx, "agent.async_intent", func(context.Context) {
		_ = a.SendIntent(trigger)
	})
}

// SurpriseIndex 返回最近一次计算的 SurpriseIndex。
func (a *Agent) SurpriseIndex() float64 {
	return a.sCtx.SurpriseIndex
}

// Shutdown 关闭 Agent，取消 context。
func (a *Agent) Shutdown() { a.cancel() }

// ContextCancel 返回 Agent 的 cancel 函数（供 M8 Reaper 终止过期任务）。
func (a *Agent) ContextCancel() context.CancelFunc { return a.cancel }

// fsm.StateMachine 返回 Agent 的状态机（供外部检查状态）。
func (a *Agent) StateMachine() *fsm.StateMachine { return a.sm }

// streamSubBufSize 单订阅者事件缓冲：吸收订阅者短暂消费滞后，满则丢弃并计数。
const streamSubBufSize = 100

// SubscribeStream 订阅 FSM 事件流，用于向 SSE 客户端回推流式响应 (UP-06)。
// 每次调用创建独立缓冲通道；ctx 取消（HTTP 请求结束）时自动注销并关闭通道，
// 订阅者以 channel 关闭为流结束信号之一（另一信号为 task_done 状态事件）。
func (a *Agent) SubscribeStream(ctx context.Context) <-chan types.AgentStreamEvent {
	ch := make(chan types.AgentStreamEvent, streamSubBufSize)
	a.streamSubsMu.Lock()
	a.streamSubSeq++
	id := a.streamSubSeq
	a.streamSubs[id] = ch
	a.streamSubsMu.Unlock()

	concurrent.SafeGo(ctx, "agent.stream_unsubscribe", func(c context.Context) {
		<-c.Done()
		// 注销与 close 同锁完成：publishStreamEvent 发送也持同一把锁，
		// 保证不会向已关闭通道发送。
		a.streamSubsMu.Lock()
		delete(a.streamSubs, id)
		close(ch)
		a.streamSubsMu.Unlock()
	})
	return ch
}

// publishStreamEvent 向所有订阅者非阻塞广播流事件。
// 订阅者缓冲满时丢弃该订阅者的本条事件并计数上报（HE-1），绝不阻塞 FSM 主循环。
func (a *Agent) publishStreamEvent(ev types.AgentStreamEvent) {
	a.streamSubsMu.Lock()
	defer a.streamSubsMu.Unlock()
	for _, ch := range a.streamSubs {
		select {
		case ch <- ev:
		default:
			if metrics.InstrAgentStreamDroppedTotal != nil {
				metrics.InstrAgentStreamDroppedTotal.Add(a.ctx, 1)
			}
		}
	}
}
