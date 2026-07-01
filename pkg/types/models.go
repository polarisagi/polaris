package types

import "time"

// ============================================================================
// M1 Inference Runtime — 推理层 POD
// 来源: internal/protocol/go §M1
// ============================================================================

// Usage LLM 调用的 Token 用量统计。
// 跨 M1（Provider）、M3（Observability）、M8（Blackboard token 记账）共用。
type Usage struct {
	InputTokens         int
	OutputTokens        int
	CacheHitTokens      int // Anthropic: cache_read_input_tokens
	CacheCreationTokens int // Anthropic: cache_creation_input_tokens（写入缓存消耗）
	ReasoningTokens     int // 扩展思考消耗的 token 数（不计入 OutputTokens）
}

// ProviderCapabilities LLM Provider 的能力声明（供 Router 路由决策）。
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

// StreamEvent LLM 流式输出的单个事件帧。
type StreamEvent struct {
	Type    StreamEventType
	Content string
	Usage   Usage
}

// ============================================================================
// M2 Storage Fabric — 存储层 POD
// 来源: internal/protocol/go §M2
// ============================================================================

// StoreCapabilities 存储引擎的能力声明（供 StorageRouter 路由决策）。
type StoreCapabilities struct {
	SupportsSQL       bool
	SupportsVector    bool
	SupportsGraph     bool
	SupportsFullText  bool
	SupportsStreaming bool
	Engine            string
}

// Op 单条批量写操作（Put / Delete）。
type Op struct {
	Key   []byte
	Value []byte
	Type  OpType
}

// ============================================================================
// M4 Agent Kernel — 执行层 POD
// 来源: internal/protocol/go §M4
// ============================================================================

// StepContext 单步执行上下文（供 StepScorer 打分，Best-of-N 剪枝）。
type StepContext struct {
	ToolName     string
	Input        []byte
	Output       []byte
	LatencyMs    int64
	TokensUsed   int
	SchemaPassed bool
}

// RetryPolicy 工具调用重试策略。
type RetryPolicy struct {
	MaxAttempts int
	Backoff     time.Duration
}

// ============================================================================
// M5/M10 — 混合检索层 POD
// 来源: internal/protocol/go §M5/M10
// ============================================================================

// SearchScope 限定检索范围（memory 层或 document_tree 层）。
type SearchScope struct {
	Type    string // "memory" | "document_tree"
	Subtree string // 限定检索子树（如 doc_node_id、memory_layer）
}

// RetrievalConfig BM25 + Vector + Graph 三路融合检索参数。
// M5 默认: FinalTopK=10, RerankTopM=30；M10 默认: FinalTopK=5, RerankTopM=50。
type RetrievalConfig struct {
	BM25Weight   float64
	VectorWeight float64
	GraphWeight  float64
	RRFK         int
	OversampleN  int
	RerankTopM   int
	FinalTopK    int
}

// ScoredFragment 单条混合检索结果（含证据类型和污点等级）。
type ScoredFragment struct {
	Content      string
	Score        float64
	Source       string
	Metadata     map[string]string
	EvidenceType EvidenceType // 证据来源类型（零值=未标注，兼容旧路径）
	TaintLevel   TaintLevel   // 来源数据污点等级，注入 Prompt 时须遵循 PropagateTaint 规则
}

// ScoredNode 图遍历结果：节点 ID + 激活能量（Spreading Activation）或跳数衰减分（BFS）。
// Score 由 SA 算法按边权重传播产生，物理意义明确，无需外部硬编码衰减系数。
type ScoredNode struct {
	ID    string
	Score float64 // SA: energy；BFS: hop-decay score
}

// ============================================================================
// M7 Tool & Action — 工具执行层 POD
// 来源: internal/protocol/go §M7 / internal/protocol/interfaces.go §M7
// ============================================================================

// SandboxSpec 沙箱执行规格（Sbx-L1/L2/L3 共用入参）。
type SandboxSpec struct {
	ImageOrBinary    []byte
	Args             []string
	Env              map[string]string
	StdinJSON        []byte
	CPUQuotaPct      int
	MemoryLimitMB    int
	WallClockTimeout int64 // seconds
	NetworkEgress    bool
}

// SandboxResult 沙箱执行结果。
type SandboxResult struct {
	Output     []byte
	ExitCode   int
	LatencyMs  int64
	MemoryPeak int64
}

// ToolResult 工具调用的统一返回结构。
type ToolResult struct {
	Success    bool
	Output     []byte
	LatencyMs  int64
	Error      string
	TaintLevel TaintLevel
	// Suspended 表示工具执行使当前任务挂起（如 spawn_planner）。
	Suspended bool
	// ImageParts 工具执行返回的图片内容（MCP type="image" content block 等）。
	// nil 表示无图片输出，现有工具无需修改。
	ImageParts []ImagePart
}

// ImagePart 多模态图片内容块（工具结果、LLM 消息均可携带）。
// 注意：不含任何方法，与 internal/protocol/go 中的同名类型语义相同。
type ImagePart struct {
	Type      string // "image"
	MediaType string // "image/jpeg" | "image/png" | "image/webp" | "image/gif"
	Data      []byte // base64 decoded raw bytes
	URL       string // 互斥于 Data，远程 URL 路径
	Width     int    // 可选，0=未知；token 计算用
	Height    int    // 可选，0=未知；token 计算用
	Detail    string // "low" | "high" | "auto"，空串等同 "auto"
}

// ============================================================================
// M8 Multi-Agent Orchestrator — 黑板 POD
// 来源: internal/protocol/go §M8
// ============================================================================

// TaskSnapshot Task 状态只读快照（避免拷贝含原子字段的 TaskEntry）。
type TaskSnapshot struct {
	ID     string
	Status TaskStatus
	Result []byte
}

// BlackboardEvent 黑板事件通知（Subscribe 订阅时返回）。
type BlackboardEvent struct {
	Type      string
	TaskID    string
	AgentID   string
	Payload   []byte
	Timestamp int64
}

// VerificationResult 验证阶段的结构化产出。
type VerificationResult struct {
	Verdict  VerificationVerdict
	Findings []VerificationFinding
	// Summary 为 Verifier Agent 的整体评述（可作为下一轮重规划的输入）。
	Summary string
}

// VerificationFinding 单条验证发现。
type VerificationFinding struct {
	Verdict     VerificationVerdict
	Description string
	// EvidencePath 指向支撑此发现的代码/文件路径（可选）。
	EvidencePath string
}

// ============================================================================
// M9 Self-Improvement — Staging POD
// 来源: internal/protocol/interfaces.go §M9
// ============================================================================

// StagingCandidate 自改善候选提交载荷（7 阶段 Staging 流水线入参）。
type StagingCandidate struct {
	Type           string // skill / lora / prompt / config / source_patch / user_preference
	EvolutionLevel string // Evo-L0..L4
	SourceWorker   string
	PayloadPath    string
}

// ============================================================================
// M12 Eval Harness — 评测结果 POD
// 来源: internal/protocol/interfaces.go §M12
// ============================================================================

// EvalRunReport Eval Suite 运行报告。
type EvalRunReport struct {
	Suite      string `json:"suite"`
	TotalCases int    `json:"total_cases"`
	PassCount  int    `json:"pass_count"`
	FailCount  int    `json:"fail_count"`
	P0Fail     int    `json:"p0_fail"`
	P1Fail     int    `json:"p1_fail"`
	P0Count    int    `json:"p0_count"`
	SafetyFail int    `json:"safety_fail"` // 一票否决计数
	// SkippedLowFalsifiability 是因 FalsifiabilityScore < 阈值而跳过 L4 评分的用例数（Gap-B）。
	SkippedLowFalsifiability int    `json:"skipped_low_falsifiability,omitempty"`
	Status                   string `json:"status"`
}

// ReplayReport 重放一致性报告（g_inv_08: 重放不得触发新 LLM 调用）。
type ReplayReport struct {
	SessionID       string
	Consistent      bool
	DivergentOffset int64
	NewLLMCalls     int // 必须为零（g_inv_08）
}

// ============================================================================
// M5 Memory — 记忆层 POD
// 来源: internal/protocol/interfaces.go §M5 Memory
// ============================================================================

// EpisodicQuery 情景记忆检索参数。
type EpisodicQuery struct {
	SessionID     string
	Topics        []string
	Semantic      string // 语义搜索文本
	K             int
	MaxTaintLevel TaintLevel // 上限（含）；调用方必须显式设置
}

// ScoredEvent 情景记忆检索结果（带相关性分数）。
type ScoredEvent struct {
	Score float64
	// Event 字段的类型为 interface{}，避免引入 internal/protocol/pb 依赖。
	// 实际使用时由 internal/protocol.Event 填充。
	Event any
}

// ReflectionEntry 单条元认知反思记录（Mem-L1.5）。
type ReflectionEntry struct {
	ID         string         `json:"id"`
	SessionID  string         `json:"session_id"`
	AgentID    string         `json:"agent_id,omitempty"`
	FailReason string         `json:"fail_reason,omitempty"` // 失败原因
	Strategy   string         `json:"strategy,omitempty"`    // 策略切换描述
	Decision   string         `json:"decision,omitempty"`    // 元决策内容
	Meta       map[string]any `json:"meta,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
}

// ReflectionQuery 反思记录查询参数。
type ReflectionQuery struct {
	SessionID string
	AgentID   string
	TaskType  string // 跨会话按任务类型过滤
	Topic     string // 主题词过滤：匹配 Decision 或 Strategy 字段
	K         int    // 返回最近 K 条，0 = 不限
}

// UserProfile 用户画像（L3 Persona）。
// 由 M5 ConsolidationPipeline Stage 3.5 每 50 条新事件自动合成，随使用演化。
type UserProfile struct {
	ProfileKey         string         `json:"profile_key"`         // 默认 'default'
	StableFacts        map[string]any `json:"stable_facts"`        // 低频变化事实（角色/技能/偏好）
	RecentActivity     []string       `json:"recent_activity"`     // 近 7d 行为摘要（最多 20 条）
	BehavioralPatterns map[string]any `json:"behavioral_patterns"` // 工具频率/编码风格/沟通习惯
	SynthesisCount     int            `json:"synthesis_count"`     // 累计合成次数
	LastEventTS        int64          `json:"last_event_ts"`       // 最后消费事件的 Unix 毫秒时间戳
}

// ImmutableCoreView ImmutableCore 加载结果（永不裁剪的核心区快照）。
type ImmutableCoreView struct {
	UserPrefs   []UserPreference
	SessionGoal string
	SafetyRules []SafetyRule
}

// UserPreference 用户偏好条目（ImmutableCore 的一条记录）。
type UserPreference struct {
	Dimension      string
	PreferenceText string
	Confidence     float64
	ProvenanceID   string // staging_candidates full_promotion ID
}

// SafetyRule 安全规则条目（ImmutableCore 的一条记录）。
type SafetyRule struct {
	RuleText string
	Severity string // info / warn / block
	Scope    string
}

// ============================================================================
// M6 Skill Library — 技能层 POD
// 来源: internal/protocol/interfaces.go §M6
// ============================================================================

// SkillBenchmarks 技能评测基准数据。
type SkillBenchmarks struct {
	PassRate     float64
	AvgLatencyMs float64
	AvgTokens    float64
}

// SkillFilter 技能列表过滤参数。
type SkillFilter struct {
	Capabilities      []string
	RiskLevelMax      string
	IncludeDeprecated bool
}

// TaskHint 技能选择启发（供 SkillSelector 使用）。
type TaskHint struct {
	TaskType           string
	CapabilitiesNeeded []string
	ComplexityScore    float64
}

// ============================================================================
// M10 Knowledge & RAG — 文档连接器 POD
// 来源: internal/protocol/go §M10
// ============================================================================

// DocumentRef 外部文档引用（Connector 返回的文档列表条目）。
type DocumentRef struct {
	URI         string
	Title       string
	SourceType  string // markdown | pdf | code | web | notion_page | gdoc
	ContentHash string
	ModifiedAt  int64
	Metadata    map[string]any
	Size        int64
}

// SyncDocument 拉取到的文档内容。
type SyncDocument struct {
	URI      string
	Title    string
	Content  []byte
	Metadata map[string]string
}

// ChangeEvent 文档变更事件（Watch 模式下返回）。
type ChangeEvent struct {
	Type    string // created | updated | deleted
	Ref     *DocumentRef
	OldHash string
}

// SyncConfig 数据源同步配置声明。
type SyncConfig struct {
	DefaultInterval int  // seconds
	SupportsWatch   bool // 是否支持基于事件的 Watch 模式
	MaxBatchSize    int
}

// PolicyReviewRequest 策略评审请求（Cedar 策略引擎入参）。
type PolicyReviewRequest struct {
	Principal string
	Action    string
	Resource  string
	Context   map[string]any
}

// PolicyReviewResult 策略评审结果（Cedar 策略引擎出参）。
type PolicyReviewResult struct {
	Allowed bool
	Reason  string
	Etag    string
}

// ============================================================================
// M13 Interface & Scheduler — 调度层 POD
// 来源: internal/protocol/interfaces.go §M13
// ============================================================================

// HITLPrompt 人工审批请求（HITL 网关发出）。
type HITLPrompt struct {
	ID             string
	CheckpointType string
	PromptText     string
	Options        []HITLOption
	DeadlineNs     int64
	RiskLevel      int
	TaintLevel     TaintLevel
	// DecisionEtag 决策时刻的 Cedar policy etag，auto_approve 前校验原子性。
	DecisionEtag string `json:"decision_etag,omitempty"`
}

// HITLOption HITL 审批选项。
type HITLOption struct {
	Key   string
	Label string
}

// HITLResponse 用户对 HITL 审批的响应。
type HITLResponse struct {
	OptionKey string
	UserID    string
	Approved  bool
	Reason    string
}

// TaskEvent 任务调度事件通知（Subscribe 订阅时返回）。
type TaskEvent struct {
	TaskID string
	State  string // submitted / started / progress / completed / failed / cancelled
	Detail map[string]any
}

// ============================================================================
// 轨迹与审计 POD
// 来源: internal/protocol/interfaces.go §轨迹与审计
// ============================================================================

// Trajectory 记录单次状态转移的详情（供 TrajectoryStoreReader 读取）。
type Trajectory struct {
	State       AgentState
	Action      string
	Observation string
}

// ============================================================================
// 认知搜索结果 POD
// 来源: internal/protocol/interfaces.go §CognitiveSearchResult
// ============================================================================

// CognitiveSearchResult SurrealDB FTS + HNSW 向量检索的单条结果。
type CognitiveSearchResult struct {
	ID      string
	Score   float64
	Content string
}

// ============================================================================
// Domain Repositories — DB 行映射 POD（Layer B）
// 来源: internal/protocol/interfaces.go §Domain Repositories
// 架构文档: docs/upgrade/repo-interface-migration.md §1.1 层B
// ============================================================================

// ChatSessionRow 对应 chat_sessions 表一行。
type ChatSessionRow struct {
	ID             string
	Title          string
	ThrashingIndex float64
	CreatedAt      string
	UpdatedAt      string
	MessageCount   int
}

// ChatMessageRow 对应 chat_messages 表一行。
type ChatMessageRow struct {
	ID         int64
	SessionID  string
	Role       string
	Content    string
	ToolCalls  string
	FileOffset int64
	FileLength int64
	CreatedAt  string
	UpdatedAt  string
}

// ProviderRow 对应 providers 表一行。
type ProviderRow struct {
	ID        string
	Name      string
	Type      string
	BaseURL   string
	APIKey    string
	ProjectID string
	Location  string
	SAKeyJSON string
	Enabled   bool
	CatalogID string
	CreatedAt string
	UpdatedAt string
}

// ProviderModelRow 对应 provider_models 表一行。
type ProviderModelRow struct {
	ID         string
	ProviderID string
	ModelID    string
	Name       string
	Role       string
	Enabled    bool
	CreatedAt  string
	UpdatedAt  string
}

// CronJobRow 对应 cron_jobs 表一行。
type CronJobRow struct {
	ID              string
	Name            string
	Prompt          string
	Schedule        string
	SessionID       string
	Enabled         bool
	LastRunAt       string
	NextRunAt       string
	FailureCount    int
	CircuitOpen     bool
	LastError       string
	CircuitOpenedAt string
	CreatedAt       string
}

// ExtInstanceRow 对应 extension_instances 表一行。
type ExtInstanceRow struct {
	ID          string
	ExtType     string
	Origin      string
	CatalogID   string
	Name        string
	Publisher   string
	TrustTier   int
	RuntimeID   string
	InstallPath string
	Config      string
	Status      string
	ErrorMsg    string
	CreatedAt   string
	UpdatedAt   string
}

// ExtCatalogRow 对应 extension_catalog 表一行。
type ExtCatalogRow struct {
	ID            string
	MarketplaceID string
	Type          string
	Name          string
	Description   string
	Publisher     string
	TrustTier     int
	URL           string
	Payload       string
	UpdatedAt     string
}

// MCPServerRow 对应 mcp_servers 表一行。
type MCPServerRow struct {
	ID              string
	Name            string
	Transport       string
	Command         string
	Args            string
	Env             string
	URL             string
	Enabled         bool
	Timeout         int
	TrustTier       int
	CatalogID       string
	PluginID        string
	WorkDir         string
	RequiresNetwork bool
	CreatedAt       string
	UpdatedAt       string
}

// AuditEventRow 审计日志单条记录。
type AuditEventRow struct {
	ID        string
	Action    string
	Actor     string
	Resource  string
	Meta      string // JSON
	CreatedAt string
}

// AppRow 对应 apps 表一行（自定义 App）。
type AppRow struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	URL         string `json:"url"`
	Publisher   string `json:"publisher"`
	Enabled     bool   `json:"enabled"`
	TrustTier   int    `json:"trust_tier"`
	CatalogID   string `json:"catalog_id"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// TokenCostAgg 按任务聚合的 Token 费用统计。
type TokenCostAgg struct {
	Pool         string
	TotalInput   int64
	TotalOutput  int64
	TotalCacheRd int64
	TotalCostUSD float64
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

// --- AUTO-MIGRATED FROM internal/protocol ---

// OutboxEvent is a row in the outbox table — the single source of truth for
// all async projections (graph build, vector index, skill deploy, event dispatch).
//
// The id column MUST be an AUTOINCREMENT integer to guarantee monotonic physical
// write order. UUIDv7 is broken for cursor polling because its random suffix
// causes lexicographic inversion under same-millisecond concurrent inserts.
//
// Worker polling uses: SELECT * FROM outbox WHERE id > :cursor AND committed_at > :last_scan
// The committed_at guard handles the case where an uncommitted row causes
// AUTOINCREMENT to skip a value before commit.
type OutboxEvent struct {
	ID          int64        `json:"id"`         // AUTOINCREMENT, monotonic
	EventID     string       `json:"event_id"`   // logical Event.ID
	EventType   string       `json:"event_type"` // "graph_build" | "vector_index" | "skill_deploy"
	Payload     []byte       `json:"payload"`
	CommittedAt int64        `json:"committed_at"` // unix nano, set on INSERT
	ClaimedBy   string       `json:"claimed_by,omitempty"`
	RetryCount  int          `json:"retry_count"`
	MaxRetries  int          `json:"max_retries"`
	Status      OutboxStatus `json:"status"`
}

// Event is the unit of structured coordination on the blackboard.
// Natural language content goes in Payload; coordination metadata is typed.
type Event struct {
	ID                string        `json:"id"`
	Type              EventType     `json:"type"`
	Status            EventStatus   `json:"status"`
	TaskID            string        `json:"task_id"`
	AgentID           string        `json:"agent_id,omitempty"`
	Payload           []byte        `json:"payload,omitempty"`
	ReasoningState    []byte        `json:"reasoning_state,omitempty"`
	EmbedModelVersion string        `json:"embed_model_version,omitempty"`
	TaintLevel        TaintLevel    `json:"taint_level,omitempty"`
	CreatedAt         time.Time     `json:"created_at"`
	TTL               time.Duration `json:"ttl,omitempty"`
}

// HeuristicGeneratedPayload Reflexion 生成启发式规则后的事件 payload。
// 对应 EventType = EventHeuristicGenerated。
// 发布方在步骤3（GeneratedHeuristic 写入后）发布；订阅方更新 ErrorPatternMemory。
type HeuristicGeneratedPayload struct {
	TaskID    string `json:"task_id"`
	TaskType  string `json:"task_type"`
	Heuristic string `json:"heuristic"`  // GeneratedHeuristic 内容
	AvoidRule string `json:"avoid_rule"` // 从 Cause 提取的规避规则
	CreatedAt int64  `json:"created_at"`
}

// EvalCompletedPayload Eval Suite 运行完成后的事件 payload。
// 对应 EventType = EventEvalCompleted。
// 发布方在 RunSuite 返回后发布；订阅方更新 prompt_versions.score 并决定是否触发 Rollout。
type EvalCompletedPayload struct {
	Suite            string  `json:"suite"`        // "training" | "validation"
	CandidateID      string  `json:"candidate_id"` // prompt_versions.id，空表示基线评测
	PassRate         float64 `json:"pass_rate"`    // 0.0~1.0
	P0PassRate       float64 `json:"p0_pass_rate"` // P0用例通过率
	BlockDeploy      bool    `json:"block_deploy"` // safety_fail>0 时为 true
	WarnDeploy       bool    `json:"warn_deploy"`  // P1用例有失败时为 true（不阻断但需关注）
	SafetyViolations int     `json:"safety_violations"`
	P95LatencyMs     float64 `json:"p95_latency_ms"`
	BaselineP95Ms    float64 `json:"baseline_p95_ms"`
	RunID            string  `json:"run_id"`
	CreatedAt        int64   `json:"created_at"`
}

// TaskModel represents the structured output of the perception phase.
// LLM fills this slot during the S_PERCEIVE→S_PLAN transition.
type TaskModel struct {
	Goal        string   `json:"goal"`
	Context     string   `json:"context"`
	Constraints []string `json:"constraints,omitempty"`
	Priority    int      `json:"priority"`
}

// DAGModel represents the compiled execution plan.
// LLM fills this slot during the S_PLAN→S_VALIDATE transition.
type DAGModel struct {
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

// ReflectionModel is the output of the reflection phase (S_REFLECT→S_COMPLETE).
type ReflectionModel struct {
	Success   bool     `json:"success"`
	Summary   string   `json:"summary"`
	Lessons   []string `json:"lessons,omitempty"`
	SkillName string   `json:"skill_name,omitempty"`
}

type InferRequest struct {
	Model           string
	Messages        []Message
	Tools           []ToolSchema
	MaxTokens       int
	Temperature     float64
	Thinking        *ThinkingConfig
	ResponseFormat  *ResponseFormat // 支持强制 JSON Schema / GBNF 等结构化约束
	ReasoningEffort ReasoningEffort
	ThinkingMode    ThinkingMode // TTC 推理深度控制（None=不传，High=最大扩展思考）
	ThinkingBudget  int
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

// InferOptions Provider 调用的可选参数集合。
type InferOptions struct {
	ThinkingMode    ThinkingMode // 默认 ThinkingDisabled
	MaxTokens       int          // 0 = 使用模型默认值
	Model           string
	Tools           []ToolSchema
	ResponseFormat  *ResponseFormat
	Temperature     float64
	TopP            float64
	ReasoningEffort ReasoningEffort
	ThinkingBudget  int
}

// InferOption 函数选项模式，用于构造 InferOptions。
type InferOption func(*InferOptions)

// WithThinkingMode 设置思考模式。
func WithThinkingMode(mode ThinkingMode) InferOption {
	return func(o *InferOptions) { o.ThinkingMode = mode }
}

// WithThinkingBudget 设置扩展思考的 token 预算
func WithThinkingBudget(budget int) InferOption {
	return func(o *InferOptions) { o.ThinkingBudget = budget }
}

// WithMaxTokens 设置最大输出 token 数。
func WithMaxTokens(n int) InferOption {
	return func(o *InferOptions) { o.MaxTokens = n }
}

// WithModel 设置覆盖使用的 Model。
func WithModel(model string) InferOption {
	return func(o *InferOptions) { o.Model = model }
}

// WithTools 设置提供的工具列表。
func WithTools(tools []ToolSchema) InferOption {
	return func(o *InferOptions) { o.Tools = tools }
}

// ProviderResponse Provider 完整响应，包含思考内容和最终答案。
type ProviderResponse struct {
	Content          string          // 最终回答
	ReasoningContent string          // CoT 思考内容（thinking mode 时有值）
	ToolCalls        []InferToolCall // 工具调用（若模型发起）；用现有 ToolCall 类型
	Usage            Usage           // Token 用量；用现有 Usage 类型（若存在）
	Model            string          // 添加以兼容现有使用
	FinishReason     string          // 添加以兼容现有使用
}

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
	TrustTier    TrustTier
	Source       ToolSource
	SourceURI    string
	UndoFn       string // 补偿工具的名称 (ISSUE-03)
	Timeout      time.Duration
	RetryPolicy  *RetryPolicy
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

// PipelineStageSpec 定义流水线中的一个阶段。
type PipelineStageSpec struct {
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

// VerificationPolicy 控制流水线中对抗性验证阶段的行为。
// 当 PipelineDescriptor.VerificationPolicy != nil 时，
// PipelineOrchestrator 在最后执行阶段完成后自动追加验证阶段。
type VerificationPolicy struct {
	// Capability 验证 Agent 的能力标签（默认 "verify"）。
	Capability string
	// Adversarial 为 true 时，向 Verifier 注入对抗性初始假设：
	// "目标未达成，直到代码证据证明"。
	Adversarial bool
	// BlockOnFail 为 true 时，BLOCKER 级别结果阻止流水线推进到 Done。
	BlockOnFail bool
}

// PipelineDescriptor 定义一条多阶段专家 Agent 流水线。
// PipelineOrchestrator 按 Stages 顺序依次执行，每阶段的 Result 作为
// 下一阶段的 ContextPayload 传递，实现精确上下文隔离。
type PipelineDescriptor struct {
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

// PropagateTaint 计算输出污点等级 = max(所有输入).

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

// WithResponseFormat 设置响应格式
func WithResponseFormat(fmt *ResponseFormat) InferOption {
	return func(o *InferOptions) { o.ResponseFormat = fmt }
}

// WithTemperature 设置温度
func WithTemperature(temp float64) InferOption {
	return func(o *InferOptions) { o.Temperature = temp }
}

// WithTopP 设置 TopP
func WithTopP(topP float64) InferOption {
	return func(o *InferOptions) { o.TopP = topP }
}

// WithReasoningEffort 设置思考深度
func WithReasoningEffort(effort ReasoningEffort) InferOption {
	return func(o *InferOptions) { o.ReasoningEffort = effort }
}

type State string

type StateEvent struct {
	Type    string
	Payload any
}

// SagaStep 记录单个执行步骤的补偿信息
type SagaStep struct {
	NodeID   string
	ToolName string
	UndoFn   string
	Args     []byte
}

// Note 单条跨会话笔记。
type Note struct {
	Key       string     `json:"key"`
	Content   string     `json:"content"`
	Version   int        `json:"version"`
	Tags      []string   `json:"tags,omitempty"`
	UpdatedAt time.Time  `json:"updated_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

type Entity struct {
	ID              string
	Name            string
	Type            string
	Embedding       []float32
	SourceDocID     string
	SourceChunkID   string
	OccurrenceCount int
	TaintLevel      TaintLevel
	SyncVersion     int64
	Properties      map[string]any
	SourceEventID   int64
	Version         int
	Confidence      float64

	// 生命周期字段（信念修正 + 知识演化）
	// 来源: supermemory temporal belief revision + PruneMem lifecycle governance
	DBID         int64  // 数据库自增 ID，供 MarkEntitySuperseded 使用
	Status       string // 'active'(默认) | 'superseded' | 'expired' | 'merged'
	SupersededBy int64  // status='superseded' 时指向新版本实体的 DBID

	// 时态知识图谱字段（来源: Zep/Graphiti temporal belief revision）
	// DDL: semantic_entities.valid_from / valid_until / source_type
	ValidFrom  int64  // 事实生效起始时间（Unix 毫秒）；0 = 从创建即生效
	ValidUntil int64  // 事实失效时间（Unix 毫秒）；0 = 永久有效
	SourceType string // 'llm_extract' | 'rule_extract' | 'user_stated' | 'agent_inferred'
}

type Relation struct {
	FromEntityID  string
	ToEntityID    string
	RelationType  string
	Description   string
	Confidence    float64
	SourceDocID   string
	TaintLevel    TaintLevel
	Weight        float64
	Properties    map[string]any
	SourceEventID int64

	// DB 主键（UpsertRelation 写入时必填；由 upsertSemantic 在 UpsertFact 后查询填充）
	// DDL: semantic_relations.source_id / target_id（INTEGER FK → semantic_entities.id）
	FromDBID int64 // 来源实体的数据库自增 ID
	ToDBID   int64 // 目标实体的数据库自增 ID
}

type Document struct {
	ID         string
	SourceType string // episodic / kb_doc / kb_code / kb_web / kb_api
	SourceURI  string
	Version    string
	Title      string
	Taint      TaintLevel
	Archived   bool
}

type Chunk struct {
	ID           string
	DocID        string
	Text         string
	EmbedModel   string
	EmbedVersion string
	Taint        TaintLevel
}

type SkillMeta struct {
	Name            string
	Version         string // semver
	Runtime         string // script (default) / builtin
	RiskLevel       string // low / medium / high
	Sandbox         int    // Sbx-L1=1 / Sbx-L3=3
	Capabilities    []string
	ExecMode        string    // tool / ambient
	AmbientPriority string    // always / auto / index_only
	Trust           TrustTier // 替代 SignatureValid bool（ADR-0016 §2.1）
	Idempotent      bool
	Benchmarks      SkillBenchmarks
	Instructions    string // SKILL.md 全文，供 LLM tool_use 返回
	Deprecated      bool
	ScriptPath      string // marketplace 安装路径（extension_instances.install_path + "/src/index.ts"）
	// DependsOn 此技能执行前必须可用的其他技能名列表（skill:{slug} 格式）。
	// Register 时会对 DependsOn ∪ ComposesOf 做 DFS 环检测，发现环返回错误。
	DependsOn []string
	// ComposesOf 此技能聚合包含的子技能列表（超集关系）。
	ComposesOf []string
	// PluginID 是来源插件的 plugins.id（"pl_xxx"）；独立安装的技能为空。
	PluginID string
}

type Skill struct {
	Name      string
	Version   int64
	Signature []byte
	Content   string
	Trust     TrustTier
}

type ToolCallRequest struct {
	ID             string
	ToolName       string
	Args           []byte
	InputTaint     TaintLevel
	CapabilityID   string
	SandboxLevel   int
	DeadlineNs     int64
	IdempotencyKey IdempotencyKey
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

// InterruptRequest 用户中断请求。
type InterruptRequest struct {
	Reason   string // 中断原因（供 Audit 记录）
	Action   InterruptAction
	Redirect string // Action=InterruptRedirect 时的新意图文本
}
