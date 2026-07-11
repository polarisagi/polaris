package protocol

import (
	"context"

	"github.com/polarisagi/polaris/pkg/types"
)

type

// StepScorer 对执行步骤实时打分。
// 权重: toolSuccess=0.4, schemaCheck=0.3, latency=0.2, tokenEfficiency=0.1。
// 双路径输出: Best-of-N 剪枝 + 低分标记 MEMF 候选。
StepScorer interface {
	Score(ctx context.Context, step types.StepContext) float64
}

type

// Effect 是状态转移的副作用抽象。
// 关键设计: IsLLMFill() 方法在编译期区分两类执行路径——
//   - DeterministicEffect: 重放时正常执行
//   - LLMFillEffect: 重放时从 EventLog 录像取响应，不重新调 LLM（g_inv_08）
Effect interface {
	IsLLMFill() bool
}

type

// DeterministicEffect 确定性副作用——纯函数，重放时正常执行。
DeterministicEffect struct {
	Fn func(ctx context.Context, sCtx StateContext) (types.State, error)
}

type

// LLMFillEffect LLM 协处理器副作用——重放时从 EventLog 录像取响应。
// PromptFn 必须是纯函数（同 StateContext → 同 prompt 字节，par_inv_03）。
LLMFillEffect struct {
	SchemaRef      string                                                    // → internal/agent/schemavalidate/schemas.json（GR-4-005 复核修复，2026-07-11）
	PromptFn       func(sCtx StateContext) []types.Message                   // 纯函数
	OnSuccess      func(sCtx StateContext, fill []byte) (types.State, error) // LLM 产出 → 下一状态
	OnFailure      func(sCtx StateContext, err error) (types.State, error)   // LLM 失败 → 错误状态
	MaxRetry       int
	ModelPool      string // budget / standard / reasoning
	ThinkingMode   types.ThinkingMode
	IdempotencyKey types.IdempotencyKey
}

type

// StateMachine 是 M4 状态机的执行接口。
// 定义见 spec/state.yaml §par。LLM 不直接驱动状态变迁，Go 确定性推进。
StateMachine interface {
	Initial() types.State
	Dispatch(ctx context.Context, sCtx StateContext, ev types.StateEvent) (next types.State, effects []Effect, err error)
}

type

// StateContext 穿越状态机各转移的共享上下文。
StateContext struct {
	AgentID              string
	SessionID            string
	MaxTaintLevel        types.TaintLevel // 继承自上下文请求的最高污点等级 (Taint Washing Fix)
	Mem                  MemoryFacade
	Tools                ToolRegistry
	Provider             Provider
	Policy               PolicyGate
	Preferences          map[string]string // 从 DB 加载的用户偏好配置
	SagaLog              []types.SagaStep  // Saga 记录日志
	InitialMaxStepsLimit int               // Agent 启动时的原始步骤上限
	ProviderSuspendCount int               // 连续无可用 provider 失败次数
}

type

// AgentController 供 gateway 调用的 Agent 控制接口（consumer-side）
AgentController interface {
	AgentID() string
	SetTaskIntent(intent []byte)
	SendIntent(trigger types.AgentTrigger) error
	SurpriseIndex() float64
	Memory() MemoryFacade
	Interrupt(req types.InterruptRequest)
	SetPreferences(map[string]string)
	CurrentState() types.AgentState
	ConfigInfo() map[string]any
	// SetMonthlyBudgetUSD 热更新月度预算上限，供 Cedar budget_cap 规则使用。
	// 2026-07-04 审计修复（附录·任务11）：GET/PUT /v1/config/budget 此前只读写
	// kv_store，从未回填到运行中的 Agent（启动时也硬编码 0），两条链路完全断开。
	SetMonthlyBudgetUSD(budget float64)
	// SubscribeStream 订阅 FSM 事件流，用于向 SSE 客户端回推流式响应 (UP-06)。
	SubscribeStream(ctx context.Context) <-chan types.AgentStreamEvent
}

// @consumer internal/gateway/server/chat
// AgentPool 管理 per-session Agent 生命周期。
// Acquire 返回该 session 专属 Agent 及 release 回调；调用方 defer release()。
// 超出容量时 Acquire 阻塞最多 100ms，超时返回 apperr.CodeResourceExhausted。
type AgentPool interface {
	Acquire(ctx context.Context, sessionID string) (AgentController, func(), error)
	// AcquireHeadless 供 Cron/Workflow/Webhook 等非交互式触发方注入 Intent 并同步获取最终结果，
	// 内部完整复用 Agent Kernel 的 FSM/DAG/安全 Gate/Reflection/Replan 能力。
	AcquireHeadless(ctx context.Context, intent types.Intent, opts ...types.HeadlessOption) (*types.AgentResult, error)
}

type

// AgentInvoker 用于触发 Agent 会话。
AgentInvoker interface {
	InvokeAgent(ctx context.Context, intent string, opts ...any) (string, error)
}

type

// Reranker 用于对检索结果进行重排序。
Reranker interface {
	Rerank(ctx context.Context, query string, docs []types.CognitiveSearchResult) ([]types.CognitiveSearchResult, error)
}

func (DeterministicEffect) IsLLMFill() bool { return false }
func (LLMFillEffect) IsLLMFill() bool       { return true }
