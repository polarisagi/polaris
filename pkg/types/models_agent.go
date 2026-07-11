package types

type

// StepContext 单步执行上下文（供 StepScorer 打分，Best-of-N 剪枝）。
StepContext struct {
	ToolName     string
	Input        []byte
	Output       []byte
	LatencyMs    int64
	TokensUsed   int
	SchemaPassed bool
}

type

// TaskSnapshot Task 状态只读快照（避免拷贝含原子字段的 TaskEntry）。
TaskSnapshot struct {
	ID     string
	Status TaskStatus
	Result []byte
	// Namespace 协同任务共享记忆命名空间（GD-14-001），透传自 TaskEntry.Namespace，
	// 供 Worker 在派发前注入 AgentKernel.SetMemoryNamespace。空值 = 不共享。
	Namespace string
}

type

// TaskHint 技能选择启发（供 SkillSelector 使用）。
TaskHint struct {
	TaskType           string
	CapabilitiesNeeded []string
	ComplexityScore    float64
}

type

// Trajectory 记录单次状态转移的详情（供 TrajectoryStoreReader 读取）。
Trajectory struct {
	State       AgentState
	Action      string
	Observation string
}

type

// TaskModel represents the structured output of the perception phase.
// LLM fills this slot during the S_PERCEIVE→S_PLAN transition.
TaskModel struct {
	Goal        string   `json:"goal"`
	Context     string   `json:"context"`
	Constraints []string `json:"constraints,omitempty"`
	Priority    int      `json:"priority"`
}

type

// DAGModel represents the compiled execution plan.
// LLM fills this slot during the S_PLAN→S_VALIDATE transition.
DAGModel struct {
	Nodes []DAGNode `json:"nodes"`
	Edges []DAGEdge `json:"edges"`
}
type DAGNode struct {
	ID      string         `json:"id"`
	Action  string         `json:"action"`
	Params  map[string]any `json:"params"`
	Retry   int            `json:"retry"`
	Timeout string         `json:"timeout"`
}
type DAGEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}
type TaskEntry struct {
	ID          string
	Type        string
	Priority    int
	Status      TaskStatus
	ClaimedBy   string
	ClaimedAt   int64
	ExpiresAt   int64
	Toxicity    int
	Intent      []byte
	IntentTaint TaintLevel // Taint 污点随 Intent 传播，禁止跨 Agent 边界降级
	Result      []byte
	ResultTaint TaintLevel // Taint 污点随 Result 传播，禁止跨 Agent 边界降级
	DependsOn   []string
	SubTasks    []string
	Deadline    int64
	SpawnDepth  int // 防止 Custom Agent 递归超限
	CreatedAt   int64
	UpdatedAt   int64

	// ── 流水线阶段 handoff 字段 ──────────────────────────────────────────────
	// PipelineID 标识本 Task 所属流水线实例；空表示非流水线任务。
	PipelineID string
	// PipelineStage 标识本 Task 在流水线中的阶段名（如 "research"/"plan"/"execute"/"verify"）。
	PipelineStage string
	// ContextPayload 携带前序阶段的结构化产出（JSON），由 PipelineOrchestrator 填充。
	// 下游 Agent 在 S_PERCEIVE 时优先读取此字段，而非全局记忆检索。
	ContextPayload []byte

	// 隔离沙箱相关字段
	Namespace     string   `json:"namespace,omitempty"`
	ToolWhitelist []string `json:"tool_whitelist,omitempty"`

	// ── Token 记账字段（Gap-A, HE-Rule-1）────────────────────────────────────
	// Worker.tryClaimAndExecute 完成后调用 Blackboard.UpdateTaskTokens 写入。
	TokensInput     int
	TokensOutput    int
	TokensCacheRead int
	CostUSD         float64
}

type

// PipelineStageSpec 定义流水线中的一个阶段。
PipelineStageSpec struct {
	// Name 阶段名称（唯一标识符），如 "research"、"plan"、"execute"。
	Name string
	// Capability 需要的 Agent 能力标签；Orchestrator 据此选择最优 Worker。
	Capability string
	// TaskType 投递到 Blackboard 的任务类型（影响 FindBestAgent 能力匹配）。
	TaskType string
	// Priority 阶段任务的优先级（越小越优先）。
	Priority int
	// BudgetTokens 本阶段允许的最大 token 用量（0 = 继承全局预算）。
	BudgetTokens int
	// TimeoutSec 阶段超时（秒），0 表示不限制。
	TimeoutSec int
}

type

// VerificationPolicy 控制流水线中对抗性验证阶段的行为。
// 当 PipelineDescriptor.VerificationPolicy != nil 时，
// PipelineOrchestrator 在最后执行阶段完成后自动追加验证阶段。
VerificationPolicy struct {
	// Capability 验证 Agent 的能力标签（默认 "verify"）。
	Capability string
	// Adversarial 为 true 时，向 Verifier 注入对抗性初始假设：
	// "目标未达成，直到代码证据证明"。
	Adversarial bool
	// BlockOnFail 为 true 时，BLOCKER 级别结果阻止流水线推进到 Done。
	BlockOnFail bool
}

type

// PipelineDescriptor 定义一条多阶段专家 Agent 流水线。
// PipelineOrchestrator 按 Stages 顺序依次执行，每阶段的 Result 作为
// 下一阶段的 ContextPayload 传递，实现精确上下文隔离。
PipelineDescriptor struct {
	// ID 流水线实例唯一标识（空时由 PipelineOrchestrator 自动生成）。
	ID string
	// Goal 原始任务目标，贯穿所有阶段，供 Verifier 做目标反向验证。
	Goal string
	// Stages 有序阶段列表，按索引顺序执行。
	Stages []PipelineStageSpec
	// VerificationPolicy 非 nil 时，在最后阶段之后追加对抗验证。
	VerificationPolicy *VerificationPolicy
	// MaxRetries 单阶段失败后的最大重试次数。
	MaxRetries int
	// CompensateStage 可选补偿函数，stage 名 → 补偿任务 type（若该 stage 失败需要回滚）
	CompensateStage map[string]string `json:"compensate_stage,omitempty"`
}
type State string
type StateEvent struct {
	Type    string
	Payload any
}

type

// SagaStep 记录单个执行步骤的补偿信息
SagaStep struct {
	NodeID   string
	ToolName string
	UndoFn   string
	Args     []byte
}
type Task struct {
	ID             string
	Type           string
	Pool           string // intent_handler / ingest / background / eval / cron
	Payload        []byte
	Priority       int // 0=最高(用户交互)
	MaxAttempts    int
	IdempotencyKey IdempotencyKey
}

type

// InterruptRequest 用户中断请求。
InterruptRequest struct {
	Reason   string // 中断原因（供 Audit 记录）
	Action   InterruptAction
	Redirect string // Action=InterruptRedirect 时的新意图文本
}

type AgentStreamEventType string

const (
	AgentStreamEventToken      AgentStreamEventType = "token"
	AgentStreamEventThinking   AgentStreamEventType = "thinking"
	AgentStreamEventToolCall   AgentStreamEventType = "tool_call"
	AgentStreamEventToolResult AgentStreamEventType = "tool_result"
	AgentStreamEventError      AgentStreamEventType = "error"
	AgentStreamEventStatus     AgentStreamEventType = "status"
)

// AgentStreamEvent defines a token-level or block-level structured event published during FSM reasoning.
type AgentStreamEvent struct {
	Type       AgentStreamEventType `json:"type"`
	Content    string               `json:"content"`
	TaintLevel TaintLevel           `json:"taint_level,omitempty"`
	ToolName   string               `json:"tool_name,omitempty"`
	ToolInput  []byte               `json:"tool_input,omitempty"`
}
