// Package protocol 跨模块共享类型定义。
// 这些类型是各模块接口契约的一部分，架构文档 docs/arch/ 中的 struct 定义
// 必须与此处一致。
package protocol

import "time"

// ============================================================================
// M1 Inference Runtime — 请求/响应/流式事件
// 架构文档: docs/arch/01-Inference-Runtime-深度选型.md §2
// ============================================================================

type InferRequest struct {
	Model           string
	Messages        []Message
	Tools           []ToolSchema
	MaxTokens       int
	Temperature     float64
	Thinking        *ThinkingConfig
	ResponseFormat  *ResponseFormat // 支持强制 JSON Schema / GBNF 等结构化约束
	ReasoningEffort ReasoningEffort // TTC 推理深度控制（None=不传，High=最大扩展思考）
}

func (req *InferRequest) HasImageParts() bool {
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			if _, ok := p.(ImagePart); ok {
				return true
			}
		}
	}
	return false
}

func (req *InferRequest) HasVideoParts() bool {
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			if _, ok := p.(VideoPart); ok {
				return true
			}
		}
	}
	return false
}

type ResponseFormat struct {
	Type       string // "json_object" | "json_schema" | "gbnf"
	JSONSchema any    // 当 Type="json_schema" 时传递的 Schema
	Grammar    string // 当 Type="gbnf" 时传递的规则串
}

type Message struct {
	Role    string
	Content string
	// Parts 非空时，adapter 应使用 Parts 作为 content（用于 tool_use/tool_result 多块消息）。
	// 向后兼容：nil 时退回到 Content 字符串。
	Parts []any
	// ReasoningContent 保存 DeepSeek 思考模式下的 reasoning_content，
	// 多轮 tool_call 时必须原样回传，否则 API 返回 400。
	ReasoningContent string
}

type ImagePart struct {
	Type      string // "image"
	MediaType string // "image/jpeg" | "image/png" | "image/webp" | "image/gif"
	Data      []byte // base64 decoded raw bytes
	URL       string // 互斥于 Data，远程 URL 路径
	Width     int    // 可选，0=未知；token 计算用
	Height    int    // 可选，0=未知；token 计算用
	Detail    string // "low" | "high" | "auto"，空串等同 "auto"
}

type VideoPart struct {
	Type      string // "video"
	MediaType string // "video/mp4" | "video/webm"
	Data      []byte // 文件内容 (≤20MB inline)
	URI       string // Provider File API 上传后的 URI
}

type ToolSchema struct {
	Name        string
	Description string
	Parameters  any // JSON Schema
}

type ThinkingConfig struct {
	BudgetTokens int
	Mode         string // "auto" | "enabled" | "disabled"
}

// InferToolCall LLM 返回的工具调用请求（finish_reason=tool_calls / stop_reason=tool_use 时）。
type InferToolCall struct {
	ID    string
	Name  string
	Input []byte // JSON 编码的工具输入参数
}

type InferResponse struct {
	Content      string
	ToolCalls    []InferToolCall // LLM 请求调用的工具列表；为空表示纯文本回复
	Usage        Usage
	Model        string
	FinishReason string
}

type Usage struct {
	InputTokens         int
	OutputTokens        int
	CacheHitTokens      int // Anthropic: cache_read_input_tokens
	CacheCreationTokens int // Anthropic: cache_creation_input_tokens（写入缓存消耗）
	ReasoningTokens     int // 扩展思考消耗的 token 数（不计入 OutputTokens）
}

type StreamEventType int

const (
	StreamTextDelta StreamEventType = iota
	StreamToolCall
	StreamThinking
	StreamError
	// StreamCancelled 用户主动取消时发出，Usage 字段携带补偿计费数据：
	// InputTokens=估算的输入 token（完整请求已发出），OutputTokens=已收到的输出 token 数。
	StreamCancelled
)

type StreamEvent struct {
	Type    StreamEventType
	Content string
	Usage   Usage
}

type ProviderCapabilities struct {
	SupportsStreaming bool
	SupportsTools     bool
	SupportsThinking  bool
	SupportsVision    bool
	SupportsVideo     bool
	SupportsTTS       bool
	MaxContextTokens  int
	CostPer1KInput    float64
	CostPer1KOutput   float64
	CostPer1KCacheHit float64
}

type TokenizerAdapter interface {
	CountTokens(text string) int
	CountTokensBatch(texts []string) []int
}

// MultimodalTokenizer 扩展接口，支持多模态 token 精确计算。
// 由具体 tokenizer 实现（如 TiktokenTokenizer），调用方用类型断言升级。
// 基础 TokenizerAdapter 实现无需实现此接口（向后兼容）。
type MultimodalTokenizer interface {
	TokenizerAdapter
	// CountImageTokens 按 OpenAI GPT-4V tile 规则计算图片 token。
	// detail: "low"=85 tokens 固定；"high"/"auto"/""=按 tile 公式。
	// width/height=0 时用默认 1024×1024 估算。
	CountImageTokens(width, height int, detail string) int
	// CountVideoTokens 估算视频 token（每秒 fps 帧，每帧按 512×512 high detail 计）。
	CountVideoTokens(durationSecs float64, fps float64) int
	// EstimateRequest 估算完整 InferRequest 的输入 token 数（含文本+多模态）。
	// 用于流式请求取消时的补偿计费。
	EstimateRequest(req *InferRequest) int
}

// ============================================================================
// M2 Storage Fabric — Store 支持类型
// 架构文档: docs/arch/02-Storage-Fabric-深度选型.md §1
// ============================================================================

type Iterator interface {
	Next() bool
	Key() []byte
	Value() []byte
	Err() error
	Close() error
}

type Op struct {
	Key   []byte
	Value []byte
	Type  OpType
}

type OpType int

const (
	OpPut OpType = iota
	OpDelete
)

type Transaction interface {
	Get(key []byte) ([]byte, error)
	Put(key, value []byte) error
	Delete(key []byte) error
	Scan(prefix []byte) (Iterator, error)
}

type StoreCapabilities struct {
	SupportsSQL       bool
	SupportsVector    bool
	SupportsGraph     bool
	SupportsFullText  bool
	SupportsStreaming bool
	Engine            string
}

// ============================================================================
// M4 Agent Kernel — FSM 状态枚举（全系统唯一权威定义）
// 架构文档: docs/arch/04-Agent-Kernel-深度选型.md §1
// 11 状态: 5 主执行态 + 2 恢复态 + 2 终态 + 1 中断暂停态
// ============================================================================

type AgentState int

const (
	AgentStateIdle      AgentState = iota // 空闲等待意图
	AgentStatePerceive                    // S_PERCEIVE: LLM 填槽理解任务
	AgentStatePlan                        // S_PLAN: LLM 填槽生成 DAG
	AgentStateValidate                    // S_VALIDATE: 四层校验
	AgentStateExecute                     // S_EXECUTE: DAG 执行
	AgentStateReflect                     // S_REFLECT: LLM 填槽反思
	AgentStateReplan                      // S_REPLAN: 重新规划（Recovery）
	AgentStateRollback                    // S_ROLLBACK: Saga 逆序补偿（Recovery）
	AgentStateComplete                    // S_COMPLETE: 成功终态
	AgentStateFailed                      // S_FAILED: 失败终态（ReplanGuard 超限）
	AgentStateInterrupt                   // S_INTERRUPT: 用户中断暂停态（非终态，可 Resume/Redirect/Abort）
)

type AgentTrigger int

const (
	TriggerIntentReceived AgentTrigger = iota
	TriggerPerceiveDone
	TriggerPlanDone
	TriggerValidateOk
	TriggerValidateFail
	TriggerExecuteDone
	TriggerExecuteFail
	TriggerReflectDone
	TriggerRollbackDone
	TriggerReplanDone
	TriggerReplanExhausted
	TriggerInterruptReceived // 用户中断信号（任意活跃态均可接收，inv_global_08）
	TriggerInterruptResume   // 中断后恢复执行（回到 interruptFrom 状态）
	TriggerInterruptAbort    // 中断后终止任务 → S_FAILED
)

// ReasoningEffort 跨厂商推理深度枚举（TTC：Test-Time Compute）。
// 各适配器映射规则:
//
//	OpenAI:   None→omit, Low→"low", Medium→"medium", High→"high"
//	DeepSeek: None→0, Low→1024, Medium→8192, High→32768 (thinking_budget_tokens)
//	Claude:   None→disabled, Low→1024, Medium→8192, High→32768 (budget_tokens)
//
// 架构文档: docs/arch/M01-Inference-Runtime.md §5.2-bis
type ReasoningEffort int

const (
	ReasoningEffortNone   ReasoningEffort = iota // 禁用扩展思考，走 System 1
	ReasoningEffortLow                           // 轻量推理（~1K tokens），System 1.5
	ReasoningEffortMedium                        // 标准推理（~8K tokens），System 2 基础
	ReasoningEffortHigh                          // 深度推理（~32K tokens），System 2 完整
)

// ============================================================================
// M4 Agent Kernel — 步骤上下文
// 架构文档: docs/arch/04-Agent-Kernel-深度选型.md §5.5
// ============================================================================

type StepContext struct {
	ToolName     string
	Input        []byte
	Output       []byte
	LatencyMs    int64
	TokensUsed   int
	SchemaPassed bool
}

// ============================================================================
// M5/M10 — 共享检索类型
// 架构文档: docs/arch/05-Memory-System-深度选型.md §7,
//           docs/arch/10-Knowledge-RAG-深度选型.md §2.2
// ============================================================================

type SearchScope struct {
	Type    string // "memory" | "document_tree"
	Subtree string // 限定检索范围（如 doc_node_id、memory_layer）
}

type RetrievalConfig struct {
	BM25Weight   float64
	VectorWeight float64
	GraphWeight  float64
	RRFK         int
	OversampleN  int
	RerankTopM   int
	FinalTopK    int
}

type ScoredFragment struct {
	Content  string
	Score    float64
	Source   string
	Metadata map[string]string
}

// ============================================================================
// M7 Tool & Action — Tool/ToolResult
// 架构文档: docs/arch/07-Tool-Action-Layer-深度选型.md §3
// ============================================================================

type Tool struct {
	Name         string
	Description  string
	Version      string
	InputSchema  any // JSON Schema
	OutputSchema any // JSON Schema
	Capability   CapabilityLevel
	SideEffects  []SideEffect
	RiskLevel    RiskLevel
	SandboxTier  SandboxTier
	Source       ToolSource
	SourceURI    string
	Timeout      time.Duration
	RetryPolicy  *RetryPolicy
}

type CapabilityLevel int

const (
	CapReadOnly CapabilityLevel = iota
	CapWriteLocal
	CapWriteNetwork
	CapPrivileged
)

type SideEffect string

const (
	SideFileWrite    SideEffect = "file_write"
	SideNetworkCall  SideEffect = "network_call"
	SideProcessSpawn SideEffect = "process_spawn"
	SideStateMutate  SideEffect = "state_mutate"
	SideNone         SideEffect = "none"
)

type RiskLevel int

const (
	RiskLow RiskLevel = iota
	RiskMedium
	RiskHigh
	RiskPrivileged
)

type SandboxTier int

const (
	SandboxInProcess SandboxTier = iota + 1
	SandboxWasm
	SandboxContainer
	// SandboxRemote 委托给远端 HTTP 执行器（Modal / Lambda / 自托管 VPS）。
	// 用于 Tier-0 内存受限时将重计算任务外包，不影响本地内存预算。
	SandboxRemote
)

type ToolSource string

const (
	ToolBuiltin      ToolSource = "builtin"
	ToolMCP          ToolSource = "mcp"
	ToolSkill        ToolSource = "skill"
	ToolA2A          ToolSource = "a2a"
	ToolLLMGenerated ToolSource = "llm_generated"
)

type RetryPolicy struct {
	MaxAttempts int
	Backoff     time.Duration
}

type ToolResult struct {
	Success    bool
	Output     []byte
	LatencyMs  int64
	Error      string
	TaintLevel TaintLevel
	// ImageParts 工具执行返回的图片内容（MCP type="image" content block 等）。
	// nil 表示无图片输出，现有工具无需修改。
	// sse.go 将图片追加到 toolResultParts 切片，各适配器已天然支持 protocol.ImagePart。
	ImageParts []ImagePart
}

// ============================================================================
// M8 Multi-Agent Orchestrator — 黑板类型
// 架构文档: docs/arch/08-Multi-Agent-Orchestrator-深度选型.md §1
// ============================================================================

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
}

type TaskStatus int

const (
	TaskPending TaskStatus = iota
	TaskClaimed
	TaskExecuting
	TaskSuspended
	TaskCompensating
	TaskDone
	TaskFailed
)

// TaskSnapshot Task 状态只读快照（避免拷贝含原子字段的 TaskEntry）。
type TaskSnapshot struct {
	ID     string
	Status TaskStatus
	Result []byte
}

type BlackboardEvent struct {
	Type      string
	TaskID    string
	AgentID   string
	Payload   []byte
	Timestamp int64
}

// ============================================================================
// M10 Knowledge & RAG — Connector 支持类型
// 架构文档: docs/arch/M10-Knowledge-RAG.md §1.2
// ============================================================================

type DocumentRef struct {
	URI         string
	Title       string
	SourceType  string // markdown | pdf | code | web | notion_page | gdoc
	ContentHash string
	ModifiedAt  int64
	Metadata    map[string]any
	Size        int64
}

type SyncDocument struct {
	URI      string
	Title    string
	Content  []byte
	Metadata map[string]string
}

type ChangeEvent struct {
	Type    string // created | updated | deleted
	Ref     *DocumentRef
	OldHash string
}

type SyncConfig struct {
	DefaultInterval int  // seconds
	SupportsWatch   bool // 是否支持基于事件的 Watch 模式
	MaxBatchSize    int
}

type PolicyReviewRequest struct {
	Principal string
	Action    string
	Resource  string
	Context   map[string]any
}

type PolicyReviewResult struct {
	Allowed bool
	Reason  string
	Etag    string
}

// ============================================================================
// TaintLevel — 全系统共享的污点置信度枚举
// 架构文档: docs/arch/11-Policy-Safety-深度选型.md §2.3
// 全局字典: docs/arch/00-Global-Dictionary.md §4 [TaintLevel]
// 传播规则: output = max(所有输入的 TaintLevel)，只升不降。
// ============================================================================

type TaintLevel int

const (
	TaintNone         TaintLevel = iota // 系统生成/常量
	TaintLow                            // 受信内部数据
	TaintMedium                         // LLM 摘要输出（硬地板，不可降为 Low）
	TaintHigh                           // 外部用户输入
	TaintUserReviewed                   // 人类显式确认
)

func (t TaintLevel) String() string {
	switch t {
	case TaintNone:
		return "none"
	case TaintLow:
		return "low"
	case TaintMedium:
		return "medium"
	case TaintHigh:
		return "high"
	case TaintUserReviewed:
		return "user_reviewed"
	default:
		return "unknown"
	}
}

// PropagateTaint 计算输出污点等级 = max(所有输入).
func PropagateTaint(inputs ...TaintLevel) TaintLevel {
	var max TaintLevel
	for _, t := range inputs {
		if t > max {
			max = t
		}
	}
	return max
}

// ============================================================================
// M3 Observability — 决策日志数据结构
// 架构文档: docs/arch/M03-Observability.md §10
// ============================================================================

// DecisionLogEntry 对应 006_decision_log.sql 表的数据结构
type DecisionLogEntry struct {
	SessionID    string
	AgentID      string
	DecisionType string
	Context      []byte // JSON
	Choice       string
	Alternatives []byte // JSON
	Reason       string
	Outcome      []byte // JSON
}

// ============================================================================
// 跨模块 IdempotencyKey 统一类型
// 架构文档: docs/arch/00-Global-Dictionary.md §9-bis, ROADMAP.md §6 (P2-03)
// ============================================================================

// IdempotencyKey 跨模块幂等键统一类型。
// 格式: {target_engine}:{entity_type}:{entity_id}:{operation}:{version}
// target_engine: "sqlite" / "surreal"
// entity_type:   "event" / "task" / "outbox" / "skill"
// entity_id:     实体唯一标识
// operation:     "create" / "update" / "delete" / "rollout"
// version:       数据版本号（int）
//
// 各模块使用规范:
//   - LLMFillEffect.IdempotencyKey — LLM 填槽结果的幂等标识（重放时跳过已执行的填槽）
//   - ToolCallRequest.IdempotencyKey — 工具调用的幂等标识（崩溃恢复时防止双重执行）
//   - Task.IdempotencyKey — 任务的幂等标识（M13 调度器去重）
//   - OutboxRecord.IdempotencyKey — Outbox 跨引擎投影的幂等标识
//   - Event.IdempotencyKey — EventLog 事件的幂等标识（M2 写入去重）
type IdempotencyKey string

// BuildIdempotencyKey 按规范格式构建幂等键。
func BuildIdempotencyKey(engine, entityType, entityID, operation string, version int) IdempotencyKey {
	return IdempotencyKey(engine + ":" + entityType + ":" + entityID + ":" + operation + ":" + itoa(version))
}

func itoa(i int) string {
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
