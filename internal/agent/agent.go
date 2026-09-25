// File: internal/agent/agent.go
// RuleVerified: [agent-boundary] 禁止直接 import action 具体实现 | [fsm-control] LLM 是协处理器不是控制流
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/internal/agent/fsm"

	agentctx "github.com/polarisagi/polaris/internal/agent/context"
	"github.com/polarisagi/polaris/internal/memory/compact"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/guard"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/internal/sysinfo"
	"github.com/polarisagi/polaris/internal/tool/catalog"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// EffectResult 承载 executeEffect 的异步完成结果。
type EffectResult struct {
	Transition types.AgentTrigger // 执行完成后应触发的 FSM 事件
	Err        error
}

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
	provider          protocol.Provider             // LLM 调用入口（由 M1 提供）
	Security          SecurityBundle                // 安全组件包（GR-6-006）
	hitl              protocol.HITL                 // 人工审批网关
	taintReviewDenied map[string]bool               // 本回合被用户拒绝的 工具+参数 哈希；仅 S_VALIDATE Effect 读写，effectRunning 保证串行（agent_taint_review.go）
	toolRegistry      protocol.AgentToolExecutor    // 工具执行表（由 M7 提供）
	catalog           catalog.Catalog               // 工具目录（用于组装 Schema，由 M7 提供）
	memory            protocol.MemoryFacade         // 四层记忆系统（由 M5 提供）
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
	codeAct           CodeActEngine             // LLM 代码执行引擎；nil 时 code_act 节点返回错误
	skillCache        ScriptSkillCache          // 可选；nil 时 FastPath 跳过缓存查询
	skillExecutor     protocol.SkillExecutor    // 可选；FastPath 缓存命中后执行 Python 脚本（M4 System 1）
	assembler         *agentctx.Assembler       // CC-3 ContextAssembler
	lamEngine         LAMPolicyChecker          // LAM GUI 自动化引擎策略检查（R3）；nil 时跳过 Cedar policy 预检
	surpriseCalc      SurpriseReader            // 可选；非 nil 时替换 ComputeBasic 基础版路由
	terminalCallback  func(ctx context.Context, taskID, taskType string, replanCount int, success bool)
	dagRunner         DAGRunner                // 单 Agent 内工具链 DAG 执行引擎；NewAgentWithDefaults 默认注入
	dagValidator      DAGValidator             // S_VALIDATE 四层校验管线；NewAgentWithDefaults 默认注入
	handoffPoster     HandoffPoster            // D5：transfer_to_agent 工具依赖的 Blackboard 任务投递能力；nil 时该工具返回错误
	personaRefiner    *agentctx.PersonaRefiner // 用户画像精炼（M05 §2.3）；nil 时跳过会话结束画像更新

	// workspaceCtxLoader / workspaceRoot 工作区标准上下文装载（GD-14-005）。
	// 任一为空即禁用该能力。信任判定在 loader 内部完成——未在配置中显式声明
	// 信任的工作区，其 AGENTS.md/CLAUDE.md 只进 ZoneExternalCatalog。
	workspaceCtxLoader *agentctx.WorkspaceContextLoader
	workspaceRoot      string
	// projectResolver 按会话反查所属项目的工作区上下文输入（ADR-0097）。
	// 每轮感知阶段现取（而非装配时快照）：用户在会话中途改了项目指令 / 信任开关
	// 应当下一轮生效。返回 nil = 会话尚无项目（或查询失败），回退 workspaceRoot 行为。
	projectResolver func(ctx context.Context, sessionID string) *agentctx.ProjectContext
	// projectRoot 本会话项目工作目录（string），由 refreshWorkspaceContext 在无 effect
	// 运行的窗口写入、executeEffect 读取注入 ctx（ADR-0097 决策五）。atomic 因二者跨 goroutine。
	projectRoot atomic.Value
	// projectID 本会话所属项目 ID（string），情景记忆打标与检索过滤用（ADR-0097 决策三修订）。
	projectID atomic.Value
	// projectNamespace SetMemoryNamespace 的原子副本，供项目解析回退使用（决策三补）。
	projectNamespace atomic.Value
	// validatedCalls 最近一次通过 S_VALIDATE 的计划中全部"工具+参数"指纹（不可变快照），
	// 能力令牌只为其中的调用 JIT 签发（agent_capability.go）。校验与执行在不同 effect
	// goroutine，DAG 节点并发读取，故用原子指针整体替换。
	validatedCalls atomic.Pointer[map[string]struct{}]

	// sagaRecorder 本轮 DAG 执行的 Saga 补偿结果记录器，由 runExecuteDAG 每次新建，
	// 经 ctx 交给 execute/dag 的 runCompensation 写入，经 buildStateContext 交给
	// FSM 的 rollbackSaga 读取——补偿由 DAG 层唯一执行，FSM 只汇报结果
	// （ADR-0088 决策一）。nil（尚未进入过 S_EXECUTE）时视为无补偿发生。
	sagaRecorder *protocol.SagaCompensationRecorder

	// cwm M04 §7 热路径上下文窗口管理（见 budget.go ContextWindowManager 与
	// agent_context_compaction.go）；NewAgent 默认构造（90000 token），可经
	// InjectContextWindowManager 覆盖。toolOffloader 为 Stage 1 大 tool_result
	// 卸载依赖，nil 时该阶段静默跳过（与网关 Compressor 的 nil-offloader 语义一致）。
	cwm           *ContextWindowManager
	toolOffloader compact.Offloader

	// [M04 §8 崩溃恢复回放] replayCalls/replayIdx 由 InjectReplayData 注入
	// （仅供 cmd/polaris 崩溃恢复驱动器调用），executeEffect 的 LLMFillEffect
	// 主路径在 protocol.IsReplaying()==true 时优先按顺序消费，不发起真实
	// Provider 调用。队列耗尽的那一刻由消费点负责翻转全局 ReplayMode=false
	// （见 agent_execute_effect.go），而不是等 Run() 结束才翻转——本 Agent
	// 之后若还需要继续推进（真实崩溃点晚于最后一条录像），必须立刻恢复真实
	// 调用能力；这是安全的，因为崩溃恢复驱动器严格串行处理各会话，串行窗口
	// 内不存在其他并发会话依赖同一全局标志的读取。
	replayCalls []protocol.ReplayLLMCall
	replayIdx   int

	// [M04 §8 崩溃检测] eventStore 非 nil 时，Run() 在处理循环期间于 KV store
	// 写入 "inflight:session:{id}" 标记，正常退出（终态/ctx取消/idle超时后的
	// suspend）时清除；若进程崩溃，标记残留，供 boot 阶段崩溃恢复驱动器扫描
	// 判定"哪些会话在崩溃前正处于某一轮处理中"。nil 时（未注入，如测试场景）
	// 完全跳过，行为与注入前一致。
	eventStore protocol.Store

	// [GR-4-004] pendingRedirectCh 用于安全地从外部 Interrupt goroutine
	// 向主循环传递重定向意图字符串，避免直接写 sCtx.RawIntentTS 的数据竞争。
	// 缓冲大小为 1：如果主循环尚未消费上一个 redirect，新的 Redirect 会覆盖
	// （select default 分支静默丢弃旧值），与 S_INTERRUPT 语义一致——只有最后
	// 一次 Redirect 有效，历史意图在被挂起时就已失效。
	pendingRedirectCh chan string

	// [UP-06] 流式事件订阅者注册表：每个订阅者持独立缓冲通道。
	// 为什么不用单一共享 channel：共享通道无法区分轮次，斜杠命令短路后
	// 残留事件会污染下一轮订阅者，且并发请求会互相偷取 token。
	streamSubsMu sync.Mutex
	streamSubs   map[uint64]chan types.AgentStreamEvent
	streamSubSeq uint64

	// [GD-13-006] done 在 Run() 返回时关闭，供 Pool.release() 等待内核真正
	// 退出（清理完资源、停止消费 a.intent）后再归还容量令牌，避免 Interrupt
	// 只是异步投递了中止信号、内核尚未退出时就把同一 session 交给新请求，
	// 造成并发访问同一 Agent 内部状态的竞态。
	done     chan struct{}
	doneOnce sync.Once

	taskCheckpointRepo protocol.TaskCheckpointRepository

	effectDone    chan EffectResult // effect 异步完成回传（缓冲 1，防止 goroutine 泄漏）
	effectRunning atomic.Bool       // 串行保证：同一时刻只有一个 effect 在执行
	effectIdle    chan struct{}     // effectRunning 释放信号（缓冲 1，非阻塞投递），替代 1ms 轮询
}

// Done 返回一个在 Run() 循环真正退出时关闭的 channel。
func (a *Agent) Done() <-chan struct{} {
	return a.done
}

// GetStateMachine 返回 Agent 内部的 StateMachine
func (a *Agent) GetStateMachine() *fsm.StateMachine {
	return a.sm
}

// SecurityBundle 将离散的安全组件打包。
type SecurityBundle struct {
	PolicyGate         protocol.PolicyGate          // Cedar 策略引擎（由 M11 提供）
	TaintReviewChecker protocol.TaintReviewChecker  // S_VALIDATE TaintGate 人工复核豁免查询（M11 §2.5）；nil 时跳过
	TaintAuditor       protocol.AuditLogger         // Taint 受控降级审计落盘（M11 §2.5 每次降级写 audit_log）；nil 时仅打 slog
	TokenVault         *guard.PIITokenVault         // PII OpaqueToken 会话级可逆令牌库
	PIIDetector        *guard.PIIDetector           // PII 检测与脱敏器
	PIIDesensitizer    *guard.PIIDesensitizer       // 阶段03 R-02：格式保留假数据脱敏映射，按 SessionID 分区；终态需 ReleasePartition
	AnomalyFilter      *guard.AnomalyDistanceFilter // OWASP LLM08 输入异常检测（M11 §2.2），按会话隔离；NewAgent 默认构造
}

type AgentConfig struct {
	MaxReplan      int
	DefaultBudget  int
	MaxSteps       int
	IdleTimeoutSec int
	// SystemTier 对应硬件层级（0=Tier0/8GB, 1+=Tier1+）。
	// L3 LLM 看门狗仅在 SystemTier >= 1 时激活。
	// 由 M3 HardwareProbe 探测结果注入。
	SystemTier            int
	SurpriseHintThreshold float64
}

func NewAgent(id string, taskRepo protocol.TaskReadRepository, provider protocol.Provider) *Agent {
	ctx, cancel := context.WithCancel(context.Background())
	wCh := make(chan protocol.MemoryWhisper, 4) // 缓冲 4 条，防 PlannerPool 阻塞
	tracker := fsm.NewEpochTracker()
	agent := &Agent{
		ID:       id,
		taskRepo: taskRepo,
		intent:   make(chan types.AgentTrigger, 10),
		sm:       fsm.NewStateMachine(&agentContextBuilder{}),
		sCtx: &fsm.StateContext{
			AgentID: id,
			// SessionID 2026-07-14 补齐：此前从未赋值，导致 sCtx.SessionID 在生产环境
			// 全程为空字符串——WriteStateTransEvent/WriteLLMCallEvent/WriteToolCallEvent
			// （internal/agent/fsm/state_machine.go、agent_execute_effect.go、
			// internal/tool/tool_outcome.go）全部以 sCtx.SessionID 为 key 前缀写入
			// events:session:{id}: ，空 ID 导致所有并发 Agent 会话的事件塌缩进同一个
			// events:session:: 桶，读侧 harness.TrajectoryRecorder.Record(ctx, sessionID)
			// 按真实 sessionID 查询永远查不到数据（M9 founding_anchor 漂移检测的根因
			// 之一）。id 与 chat_sessions.id 是同一取值来源（cmd/polaris/boot_agent.go
			// buildAgent(sessionID, ...) → NewAgent(sessionID, ...)，且
			// gateway/server/chat/sessions_helpers.go 用同一 sessionID 建 chat_sessions
			// 行），复用为 SessionID 不引入新的 ID 命名空间。
			// 附带激活此前因 SessionID=="" 而跳过的分支：tokenVault.ClearTask、
			// memory-consolidate outbox 事件、withTaskScopeCtx 的 ctx 任务域注入、
			// PII 快照 session_id 字段——均为待激活的既有防御逻辑，非新增副作用。
			SessionID:      id,
			MaxReplan:      3,
			SysEnvSnapshot: sysinfo.GetSystemInfo().FormatMarkdown(),
			WhisperChan:    wCh,
			EpochTracker:   tracker,
		},
		ctx:               ctx,
		cancel:            cancel,
		provider:          provider,
		scorer:            newStepScorer(provider),
		whisperChan:       wCh,
		whisperSendChan:   wCh,
		streamSubs:        make(map[uint64]chan types.AgentStreamEvent),
		pendingRedirectCh: make(chan string, 1),
		done:              make(chan struct{}),
		// anomalyFilter 2026-07-14 补齐（ADR-0062 关联接线）：读侧
		// internal/tool/tool.go checkAnomaly 此前已完整实现（从 ctx 取
		// *guard.AnomalyDistanceFilter，检测越界后经 HITL 网关升级审批），但没有
		// 任何调用方构造过该 filter 并写入 ctx，OWASP LLM08 输入异常检测在生产
		// 环境从未真正生效（checkAnomaly 的 ok 断言恒为 false，静默直接放行）。
		// 每个 Agent（= 每个会话）持有独立实例，与其 docstring "按会话隔离"一致。
		Security: SecurityBundle{
			AnomalyFilter: guard.NewAnomalyDistanceFilter(0),
		},
		cwm:        NewContextWindowManager(0),
		effectDone: make(chan EffectResult, 1),
		effectIdle: make(chan struct{}, 1),
	}
	agent.sm.SetIntentDispatcher(agent.asyncIntent)
	return agent
}

// WorldModel 定义了认知模型所需的知识接地感知接口。
type WorldModel interface {
	AssessGrounding(ctx context.Context, task string, contextText string) (bool, string)
}

// InjectWorldModel 注入认知世界模型
func (a *Agent) InjectWorldModel(wm WorldModel) {
	a.worldModel = wm
}

func (a *Agent) InjectTaskCheckpointRepo(repo protocol.TaskCheckpointRepository) {
	a.taskCheckpointRepo = repo
}

// InjectSkillMatcher 注入 System-1 Bypass 技能匹配器
func (a *Agent) InjectSkillMatcher(sm fsm.SkillMatcher) {
	a.sm.SetSkillMatcher(sm)
}

// InjectReplayData 见 protocol.AgentController 接口注释（M04 §8 崩溃恢复回放）。
func (a *Agent) InjectReplayData(calls []protocol.ReplayLLMCall) {
	a.replayCalls = calls
	a.replayIdx = 0
}

// InjectEventStore 注入 KV Store，供 Run() 写入/清除崩溃检测用的 in-flight
// 标记（见 eventStore 字段注释）。可选注入，nil 时崩溃检测能力静默跳过。
func (a *Agent) InjectEventStore(store protocol.Store) {
	a.eventStore = store
}

// inFlightKey 返回本会话的崩溃检测 in-flight 标记 KV key。
func (a *Agent) inFlightKey() []byte {
	return []byte("inflight:session:" + a.sCtx.SessionID)
}

// markInFlight 在 Run() 循环开始处理时写入 in-flight 标记。
func (a *Agent) markInFlight(ctx context.Context) {
	if a.eventStore == nil || a.sCtx.SessionID == "" {
		return
	}
	putCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := a.eventStore.Put(putCtx, a.inFlightKey(), []byte(time.Now().UTC().Format(time.RFC3339Nano))); err != nil {
		slog.Warn("agent: failed to write in-flight crash-detection marker", "session", a.sCtx.SessionID, "err", err)
	}
}

// clearInFlight 在 Run() 循环退出（无论正常终态、超步熔断还是 ctx 取消）时
// 清除 in-flight 标记——干净退出的会话不应被崩溃恢复驱动器误判为崩溃。
// 用 context.Background() 而非调用方 ctx：Run() 退出路径常见于 ctx 已取消
// 的场景（如 Reaper 主动 Cancel），若沿用同一个已取消 ctx，清除操作会立即
// 失败，标记残留反而制造误报。
func (a *Agent) clearInFlight() {
	if a.eventStore == nil || a.sCtx.SessionID == "" {
		return
	}
	delCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := a.eventStore.Delete(delCtx, a.inFlightKey()); err != nil {
		slog.Warn("agent: failed to clear in-flight crash-detection marker", "session", a.sCtx.SessionID, "err", err)
	}
}

// handleEffectResult 处理一个已完成 effect 的回传；done=true 表示已进入终态、Run 应返回 nil。
func (a *Agent) handleEffectResult(ctx context.Context, result EffectResult) (done bool, err error) {
	if result.Err != nil {
		// [HE-1] Effect 错误此前**只**经 Run() 返回值上抛：Run 退出 → agent
		// goroutine 结束 → 订阅 SubscribeStream 的会话既收不到错误事件、也等不到
		// channel 关闭（订阅通道只在订阅 ctx 结束时才关），整个回合就这么挂在那里
		// 直到上游超时。先把错误发进流再退出，让用户拿到真实原因而不是干等。
		a.abortTurn(ctx, result.Err)
		return true, apperr.Wrap(apperr.CodeInternal, "Agent.Run", result.Err)
	}
	if result.Transition != 0 {
		a.asyncIntent(result.Transition)
	}
	// 终态检查 (可能被 effect transition 修改)
	current := a.sm.Current()
	if current == types.AgentStateComplete || current == types.AgentStateFailed {
		a.handleTerminalState(ctx, current)
		return true, nil
	}
	return false, nil
}

// Run 启动 Agent 事件循环（Suspend-on-Idle）。
// 空闲时阻塞在 intent channel 上，不轮询——符合 par_inv_05。
//
//nolint:gocyclo
func (a *Agent) Run(ctx context.Context) error {
	// [GD-13-006] Run() 循环退出（无论正常终态、超步熔断还是 ctx 取消）时关闭
	// done，通知 Pool.release() 内核已真正停止，可以安全归还容量令牌。
	// doneOnce 防止理论上的重复调用导致 close 已关闭 channel 而 panic。
	defer a.doneOnce.Do(func() { close(a.done) })

	// [M04 §8 崩溃检测] 标记本会话当前有一个 Run() 生命周期在处理中；
	// 无论后续走哪条退出路径都会清除（含正常终态/超步熔断/ctx 取消）。
	// 若进程在两者之间崩溃，标记残留，供 boot 阶段崩溃恢复驱动器识别。
	a.markInFlight(ctx)
	defer a.clearInFlight()

	// polaris.agents_active：以 Run() 生命周期计数（GR-1.2-003，此前全仓无写入方，指标恒 0）。
	metrics.ActiveAgentsCount.Add(1)
	defer metrics.ActiveAgentsCount.Add(-1)

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
		// 动态加载已安装插件信息 (仅当无 effect 运行时安全刷新，避免与 executeEffect 并发竞争)
		if !a.effectRunning.Load() {
			a.refreshInstalledExtensions(ctx)
			// GD-14-005：工作区上下文与扩展清单同周期刷新，共用同一个
			// "无 effect 运行"的安全窗口。
			a.refreshWorkspaceContext(ctx)
		}

		select {
		case trigger := <-a.intent:
			idleTimer.Reset(time.Duration(idleTimeout) * time.Second)
			// ADR-0097 决策三补：headless 子 Agent 的命名空间在 SendIntent 之前才注入，
			// 循环顶部那次刷新可能早于注入。新意图到达时补刷一次，使首个 effect
			// 即带上正确的项目作用域（项目 ID / 工作目录 / 项目指令）。
			if trigger == types.TriggerIntentReceived && !a.effectRunning.Load() {
				a.refreshWorkspaceContext(ctx)
			}
			// Adaptive Max-Steps: 步骤计数 + 预算熔断
			a.sCtx.Mu.Lock()
			a.sCtx.StepsUsed++
			limit := a.sCtx.MaxStepsLimit
			used := a.sCtx.StepsUsed
			a.sCtx.Mu.Unlock()

			if limit > 0 && used > limit {
				// 经统一出口收尾：此前 ForceState 后直接返回，订阅方等不到 task_done（ADR-0098 决策七）。
				stepErr := apperr.New(apperr.CodeInternal,
					fmt.Sprintf("MAX_STEPS_EXCEEDED: steps %d > limit %d", used, limit))
				a.abortTurn(ctx, stepErr)
				return stepErr
			}

			// GR-4-004: 消费 pendingRedirectCh——如果有 InterruptRedirect 请求在途，
			// 在主循环单线程内安全地写入 sCtx.RawIntentTS（避免外部 goroutine 直接写的数据竞争）。
			if trigger == types.TriggerInterruptReceived {
				select {
				case redirect := <-a.pendingRedirectCh:
					if redirect != "" {
						a.sCtx.Mu.Lock()
						a.sCtx.RawIntentTS = taint.NewTaintedString(
							redirect,
							taint.TaintSource{OriginTaintLevel: types.TaintHigh},
							"user_interrupt_redirect",
						)
						a.sCtx.Mu.Unlock()
					}
				default:
				}
			}

			fromState := a.sm.Current()
			effects, err := a.sm.Dispatch(ctx, a.sCtx, trigger)
			// [HE-1] FSM 推进是交互式回合的主控制流，此前全程无埋点：一旦回合
			// 空转（不产 token 也不报错），日志里没有任何线索可区分"没触发状态
			// 转移""转移了但没产生 Effect""Effect 产出了但没调 LLM"。回合级日志
			// 量极小（每轮 1~N 条），值得常驻。
			slog.InfoContext(ctx, "kernel: fsm dispatched",
				"agent_id", a.ID, "session", a.sCtx.SessionID, "trigger", trigger,
				"from", fromState, "to", a.sm.Current(), "effects", len(effects), "err", err)
			if err != nil {
				// context 取消由 M8 Reaper 触发——直接退出，不触发 S_ROLLBACK
				if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, fsm.ErrReplanExhausted) {
					return ctxErr //nolint:wrapcheck // 保留 context 哨兵身份，供调用方 errors.Is/== 判断
				}
				// ErrReplanExhausted：sm.Dispatch 内部已转 S_FAILED；其余为转移表缺口。
				// 两者都必须经统一出口收尾，否则订阅方既收不到原因也等不到 task_done。
				a.abortTurn(ctx, err)
				return apperr.Wrap(apperr.CodeInternal, "Agent.Run", err)
			}

			// 执行 Effects: LLMFillEffect → 调 LLM；DeterministicEffect → 直接执行
			for _, effect := range effects {
				// 串行保证：上一个 effect 未完成时等待其释放。等待期间必须同时消费 effectDone
				// （GR-4.1-001）：effectDone 容量 1，若其中压着一个未读结果，正在运行的 effect
				// 会阻塞在发送上、永远走不到释放 effectRunning 的 defer，主循环在此死等。
				for !a.effectRunning.CompareAndSwap(false, true) {
					select {
					case <-ctx.Done():
						return ctx.Err() //nolint:wrapcheck // 保留 context 哨兵身份
					case result := <-a.effectDone:
						if done, err := a.handleEffectResult(ctx, result); done || err != nil {
							return err
						}
					case <-a.effectIdle:
					}
				}
				concurrent.SafeGo(ctx, "agent.executeEffect", func(execCtx context.Context) {
					// panic 由 SafeGo 外层 recover（打日志+计量），goroutine 静默退出。
					// 此处 defer 保证无论正常返回还是 panic，effectRunning 都能解锁并发出空闲信号，
					// 避免主循环在 effectDone select 上永久阻塞。
					defer func() {
						a.effectRunning.Store(false)
						select {
						case a.effectIdle <- struct{}{}:
						default:
						}
					}()
					result := a.executeEffect(execCtx, effect)
					select {
					case a.effectDone <- result:
					case <-execCtx.Done():
						if result.Err != nil {
							slog.WarnContext(execCtx, "agent: effect result dropped on ctx cancel", "err", result.Err)
						}
					}
				})
			}

			// 终态检查
			current := a.sm.Current()
			if current == types.AgentStateComplete || current == types.AgentStateFailed {
				slog.InfoContext(ctx, "kernel: terminal state reached",
					"agent_id", a.ID, "session", a.sCtx.SessionID, "state", current)
				a.handleTerminalState(ctx, current)
				return nil
			}

		case result := <-a.effectDone:
			if result.Err != nil {
				// [HE-1] Effect 错误此前只经 Run() 返回值上抛，既不进流、也只在
				// pool.go 那层打一条不含错误来源阶段的 warn。
				slog.WarnContext(ctx, "kernel: effect failed",
					"agent_id", a.ID, "session", a.sCtx.SessionID,
					"state", a.sm.Current(), "err", result.Err)
			}
			if done, err := a.handleEffectResult(ctx, result); done || err != nil {
				return err
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
			continue
		case <-ctx.Done():
			return ctx.Err() //nolint:wrapcheck // 保留 context 哨兵身份，供调用方 errors.Is/== 判断
		}
	}
}

// ============================================================================
// 错误类型
// ============================================================================

var (
	ErrReplanExhausted = apperr.NewSentinel(apperr.CodeResourceExhausted, "replan guard: max replan count reached, escalate to HITL")
	ErrIdleTimeout     = apperr.New(apperr.CodeResourceExhausted, "agent idle timeout")
)
