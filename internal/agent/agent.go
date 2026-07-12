// File: internal/agent/agent.go
// RuleVerified: [agent-boundary] 禁止直接 import action 具体实现 | [fsm-control] LLM 是协处理器不是控制流
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/agent/fsm"

	agentctx "github.com/polarisagi/polaris/internal/agent/context"
	"github.com/polarisagi/polaris/internal/execute/dag"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/guard"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/internal/sysinfo"
	"github.com/polarisagi/polaris/internal/tool/catalog"
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
	memInjector       MemoryInjector            // NEW: 组装前主动记忆注入
	codeAct           CodeActEngine             // LLM 代码执行引擎；nil 时 code_act 节点返回错误
	skillCache        ScriptSkillCache          // 可选；nil 时 FastPath 跳过缓存查询
	skillExecutor     protocol.SkillExecutor    // 可选；FastPath 缓存命中后执行 Python 脚本（M4 System 1）
	assembler         *agentctx.Assembler       // CC-3 ContextAssembler
	lamEngine         LAMPolicyChecker          // LAM GUI 自动化引擎策略检查（R3）；nil 时跳过 Cedar policy 预检
	surpriseCalc      SurpriseReader            // 可选；非 nil 时替换 ComputeBasic 基础版路由
	terminalCallback  func(ctx context.Context, taskID, taskType string, replanCount int, success bool)
	tokenVault        *guard.PIITokenVault // PII OpaqueToken 会话级可逆令牌库
	piiDetector       *guard.PIIDetector   // PII 检测与脱敏器
	dagRunner         DAGRunner            // 单 Agent 内工具链 DAG 执行引擎；NewAgentWithDefaults 默认注入
	dagValidator      DAGValidator         // S_VALIDATE 四层校验管线；NewAgentWithDefaults 默认注入

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
}

// Done 返回一个在 Run() 循环真正退出时关闭的 channel。
func (a *Agent) Done() <-chan struct{} {
	return a.done
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

func NewAgent(id string, taskRepo protocol.TaskReadRepository, taintGate TaintGate, provider protocol.Provider) *Agent {
	ctx, cancel := context.WithCancel(context.Background())
	wCh := make(chan protocol.MemoryWhisper, 4) // 缓冲 4 条，防 PlannerPool 阻塞
	tracker := fsm.NewEpochTracker()
	agent := &Agent{
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
		ctx:               ctx,
		cancel:            cancel,
		taintGate:         taintGate,
		provider:          provider,
		scorer:            newStepScorer(provider),
		whisperChan:       wCh,
		whisperSendChan:   wCh,
		streamSubs:        make(map[uint64]chan types.AgentStreamEvent),
		pendingRedirectCh: make(chan string, 1),
		done:              make(chan struct{}),
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

// NewAgentWithPolicyGate 创建带策略引擎的 Agent（主要用于生产环境）。
func NewAgentWithPolicyGate(id string, taintGate TaintGate, provider protocol.Provider, policyGate protocol.PolicyGate) *Agent {
	a := NewAgent(id, nil, taintGate, provider)
	a.policyGate = policyGate
	return a
}

// NewAgentWithDefaults 构造带默认依赖的 Agent，主要供测试/开发场景使用。
//
// dagRunner/dagValidator 默认注入 execute/dag.Runner/Validator（唯二在此处直接
// import internal/execute/dag 的位置，其余 agent 包代码一律通过 provider.go 的
// DAGRunner/DAGValidator 接口消费）。二者均为无状态适配器（零字段），此处构造
// 纯粹是为了让 NewAgentWithDefaults 保持"开箱即可跑通完整 FSM S_EXECUTE/
// S_VALIDATE 路径"的既有约定（历史上 dag 是 agent 同目录子包，测试从不需要
// 显式注入）；生产路径 cmd/polaris/boot_agent.go 的 buildAgent 会显式调用
// InjectDAGRunner/InjectDAGValidator 覆盖此处默认值，语义不变。
func NewAgentWithDefaults(id string) *Agent {
	a := NewAgent(id, nil, &defaultTaintGate{threshold: 2}, nil)
	a.dagRunner = dag.NewRunner()
	a.dagValidator = dag.NewValidator()
	return a
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

			// GR-4-004: 消费 pendingRedirectCh——如果有 InterruptRedirect 请求在途，
			// 在主循环单线程内安全地写入 sCtx.RawIntentTS（避免外部 goroutine 直接写的数据竞争）。
			if trigger == types.TriggerInterruptReceived {
				select {
				case redirect := <-a.pendingRedirectCh:
					if redirect != "" {
						a.sCtx.RawIntentTS = taint.NewTaintedString(
							redirect,
							taint.TaintSource{OriginTaintLevel: types.TaintHigh},
							"user_interrupt_redirect",
						)
					}
				default:
				}
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
				a.handleTerminalState(ctx, current)
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
			continue
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

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
