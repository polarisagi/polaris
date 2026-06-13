// Package protocol 定义 polaris 跨模块共享接口契约。
// 所有接口由消费方定义，实现在各自模块中。
// 此文件是编译期契约检查的权威源——架构文档 docs/arch/ 中的接口描述
// 必须与此处定义一致，CI lint 检查不匹配。

// 注释规范:
//   @consumer: 此接口的调用方（Go import 该接口的模块）
//   @producer: 此接口的实现方（提供具体 struct 实现该接口的模块）
//   @arch:     关联的架构文档位置

package protocol

import (
	"context"
	"net"
	"time"

	"github.com/polarisagi/polaris/internal/protocol/pb"
)

// ============================================================================
// M1 Inference Runtime — Provider Interface
// @consumer: M4(Agent Kernel - LLM 调用的唯一入口), M9(PromptOptimizer), M10(Knowledge-RAG 摘要生成)
// @producer: pkg/substrate/inference/ (各 Provider 适配器实现)
// @arch: docs/arch/01-Inference-Runtime-深度选型.md §2
// ============================================================================

// Provider 是 LLM 厂商适配器的统一接口。
// 每个 Provider 实现负责: SSE 帧归一化（Anthropic SSE / OpenAI SSE / DeepSeek JSON 行流 → 统一 chan StreamEvent）、
// API Key JIT 从 CredentialVault 获取（使用后 subtle.ConstantTimeCopy + memclr 清零）、
// 结构化错误转换为 PolarisError（禁止暴露裸 error）。
type Provider interface {
	Infer(ctx context.Context, req *InferRequest) (*InferResponse, error)
	StreamInfer(ctx context.Context, req *InferRequest) (<-chan StreamEvent, error)
	Capabilities() ProviderCapabilities
	Tokenizer() TokenizerAdapter
	ModelID() string
}

// ============================================================================
// M2 Storage Fabric — Store Interface
// @consumer: M5(Memory System - 四层记忆物理存储), M10(Knowledge-RAG - 文档索引存储),
//           M12(Eval-Harness - 轨迹存储), M3(DecisionLog - 决策日志存储)
// @producer: pkg/substrate/storage/ ([Storage-SQLite] / [Storage-SurrealDB-Core] 引擎适配器)
// @arch: docs/arch/M02-Storage-Fabric.md §1.1
// ============================================================================

// Store 是所有存储引擎的统一接口。
// 引擎选择由 StorageRouter 路由规则决定，[Storage-SQLite] 兜底。
// 不同数据类型按访问模式路由到最匹配的引擎（向量/图/全文 → [Storage-SurrealDB-Core]，其余 → [Storage-SQLite]）。
type Store interface {
	Get(ctx context.Context, key []byte) ([]byte, error)
	Put(ctx context.Context, key, value []byte) error
	Delete(ctx context.Context, key []byte) error
	Scan(ctx context.Context, prefix []byte) (Iterator, error)
	BatchWrite(ctx context.Context, ops []Op) error
	Txn(ctx context.Context, fn func(tx Transaction) error) error
	Capabilities() StoreCapabilities
	Close() error
}

// StoreExtStats 为底层存储引擎可选的统计扩展。
// 由具体 Store 实现提供，上层通过类型断言进行安全调用。
type StoreExtStats interface {
	Stats() (string, error)
}

// StoreExtVector 为底层存储引擎可选的向量操作扩展。
type StoreExtVector interface {
	VecSetMode(mode int) error
}

// EventLogger 定义了将事件安全、串行写入 M2 events 表的契约。
type EventLogger interface {
	AppendEvent(ctx context.Context, ev *pb.Event) error
}

// DecisionLogger 定义了将架构决策写入 M3 decision_log 表的契约。
type DecisionLogger interface {
	AppendDecision(ctx context.Context, entry *DecisionLogEntry) error
}

// ============================================================================
// M4 Agent Kernel — StepScorer
// @consumer: M4(Agent Kernel - 执行步骤评分, Best-of-N 剪枝)
// @producer: pkg/cognition/step_scorer.go (RuleBasedScorer)
// @arch: docs/arch/04-Agent-Kernel-深度选型.md §5.5
// ============================================================================

// StepScorer 对执行步骤实时打分。
// 权重: toolSuccess=0.4, schemaCheck=0.3, latency=0.2, tokenEfficiency=0.1。
// 双路径输出: Best-of-N 剪枝 + 低分标记 MEMF 候选。
type StepScorer interface {
	Score(ctx context.Context, step StepContext) float64
}

// ============================================================================
// M4 Agent Kernel — Effect 系统（编译期区分确定性/LLM 路径）
// @arch: docs/arch/M04-Agent-Kernel.md §1, spec/state.yaml §par
// ============================================================================

// Effect 是状态转移的副作用抽象。
// 关键设计: IsLLMFill() 方法在编译期区分两类执行路径——
//   - DeterministicEffect: 重放时正常执行
//   - LLMFillEffect: 重放时从 EventLog 录像取响应，不重新调 LLM（g_inv_08）
type Effect interface {
	IsLLMFill() bool
}

// DeterministicEffect 确定性副作用——纯函数，重放时正常执行。
type DeterministicEffect struct {
	Fn func(ctx context.Context, sCtx StateContext) (State, error)
}

func (DeterministicEffect) IsLLMFill() bool { return false }

// LLMFillEffect LLM 协处理器副作用——重放时从 EventLog 录像取响应。
// PromptFn 必须是纯函数（同 StateContext → 同 prompt 字节，par_inv_03）。
type LLMFillEffect struct {
	SchemaRef      string                                              // → spec/schemas.json
	PromptFn       func(sCtx StateContext) []Message                   // 纯函数
	OnSuccess      func(sCtx StateContext, fill []byte) (State, error) // LLM 产出 → 下一状态
	OnFailure      func(sCtx StateContext, err error) (State, error)   // LLM 失败 → 错误状态
	MaxRetry       int
	ModelPool      string // budget / standard / reasoning
	IdempotencyKey IdempotencyKey
}

func (LLMFillEffect) IsLLMFill() bool { return true }

// StateMachine 是 M4 状态机的执行接口。
// 定义见 spec/state.yaml §par。LLM 不直接驱动状态变迁，Go 确定性推进。
type StateMachine interface {
	Initial() State
	Dispatch(ctx context.Context, sCtx StateContext, ev StateEvent) (next State, effects []Effect, err error)
}

type State string
type StateEvent struct {
	Type    string
	Payload any
}

// StateContext 穿越状态机各转移的共享上下文。
type StateContext struct {
	AgentID       string
	SessionID     string
	MaxTaintLevel TaintLevel // 继承自上下文请求的最高污点等级 (Taint Washing Fix)
	Mem           Memory
	Tools         ToolRegistry
	Provider      Provider
	Policy        PolicyGate
	Preferences   map[string]string // 从 DB 加载的用户偏好配置
}

// ============================================================================
// M5/M10 — HybridRetriever 共享引擎
// @consumer: M5(Memory System - episodic_events + semantic_entities 检索, scope=memory),
//           M10(Knowledge-RAG - doc_nodes 检索, scope=document_tree)
// @producer: pkg/substrate/hybrid_retrieve.go (RRF 融合 + Rerank 引擎)
// @arch: docs/arch/05-Memory-System-深度选型.md §7,
//        docs/arch/10-Knowledge-RAG-深度选型.md §2.2
// ============================================================================

// HybridRetriever 是 BM25 + Dense Vector + Graph Traversal 三路融合检索的统一接口。
// M5 与 M10 共享底层 RRF 融合 + Rerank 引擎，检索范围和配置参数各自独立。
// 检索配置差异: M5 FinalTopK=10, RerankTopM=30; M10 FinalTopK=5, RerankTopM=50。
type HybridRetriever interface {
	Search(ctx context.Context, query string, scope SearchScope, config RetrievalConfig) ([]ScoredFragment, error)
}

// ============================================================================
// M6 Skill Library — Skill Executor
// @consumer: M4(Agent Kernel - System 1 技能缓存命中后执行脚本)
// @producer: M7(Tool-Action - Rust 沙箱, [Sandbox] 权威实现)
// @arch: docs/arch/M06-Skill-Library.md §5
// ============================================================================

// SkillExecutor 执行 TypeScript/Python 技能脚本。
// 脚本由 M7 负责沙箱执行（M7 是沙箱的 CANONICAL SOURCE）。
type SkillExecutor interface {
	ExecuteSkill(ctx context.Context, skillID string, input []byte) ([]byte, error)
	ValidateSkill(scriptBytes []byte) error
}

// ============================================================================
// M7 Tool & Action — ToolRegistry
// @consumer: M4(Agent Kernel - DAG 节点执行时通过 ExecuteTool 调用工具),
//           M6(Skill Library - 注册新技能为工具),
//           M8(Orchestrator - Agent 能力发现时查询可用工具列表)
// @producer: pkg/action/ (ToolRegistry 实现, 包含 MCP Client/Server 注册)
// @arch: docs/arch/07-Tool-Action-Layer-深度选型.md §3
// ============================================================================

// ToolRegistry 是工具发现、注册、执行的统一入口。
// 工具来源: Built-in(~20) | MCP(inf) | Skill(inf) | A2A(inf) | LLM-generated(临时, [Sandbox-L3])。
// 执行路径: ExecuteTool → Policy Gate(五阶段) → Sandbox → ToolResult。
type ToolRegistry interface {
	Register(tool Tool) error
	Lookup(name string) (Tool, error)
	List() []Tool
	ExecuteTool(ctx context.Context, name string, input []byte, taintLevel TaintLevel) (*ToolResult, error)
}

// ============================================================================
// M8 Multi-Agent Orchestrator — Blackboard
// @consumer: M4(Agent Kernel - PostTask 发布子任务, ClaimTask CAS 认领),
//           M9(Self-Improve - Auto-Curriculum 课程任务的 PostTask),
//           M13(Interface-Scheduler - 用户交互任务入口)
// @producer: pkg/swarm/blackboard.go (单机黑板 + CAS 原子认领)
// @arch: docs/arch/08-Multi-Agent-Orchestrator-深度选型.md §1
// ============================================================================

// Blackboard 是多 Agent 协调黑板。
// 所有 Agent 间通信走 schema event（禁止 P2P 自然语言），自然语言仅作 payload content。
// 常量: DefaultLeaseTTL=60s, HeartbeatInterval=15s(±5s jitter), ReaperScanInterval=1s。
// 优先级: 0=用户交互, 1=前台辅助, 2=后台优化, 3=Auto-Curriculum。
type Blackboard interface {
	PostTask(ctx context.Context, task *TaskEntry) error
	PostBatch(ctx context.Context, tasks []*TaskEntry) error
	ClaimTask(ctx context.Context, taskID, agentID string) (bool, error)
	StartExecution(ctx context.Context, taskID, agentID string) error
	CompleteTask(ctx context.Context, taskID, agentID string, result []byte) error
	FailTask(ctx context.Context, taskID, agentID string, errBytes []byte) error
	RenewLease(ctx context.Context, taskID, agentID string) error
	SuspendForHITL(ctx context.Context, taskID, agentID string, timeout int64) error
	ResumeFromHITL(ctx context.Context, taskID, agentID string, approved bool) error
	BeginCompensation(ctx context.Context, taskID, agentID string) error
	EndCompensation(ctx context.Context, taskID, agentID string) error
	SideEffectPreCheck(ctx context.Context, taskID, agentID string, claimedVersion int32) error
	PeekTask(ctx context.Context, taskID string) (*TaskSnapshot, error)
	Subscribe(ctx context.Context) (<-chan BlackboardEvent, error)
	// UpdateTaskTokens 记录本任务的 token 消耗（Gap-A, HE-Rule-1）。
	// 由 Worker.tryClaimAndExecute 在 AgentKernel.Run 返回后调用。
	// 幂等：多次调用以最后一次写入为准（覆盖，不累加）。
	UpdateTaskTokens(ctx context.Context, taskID string, tokensIn, tokensOut, cacheRead int, costUSD float64) error
}

// ============================================================================
// M11 Policy & Safety — PolicyGate (Cedar 策略引擎)
// @consumer: M7(Tool-Action - 工具调用前 Policy Gate 五阶段评估),
//           M4(Agent-Kernel - S_VALIDATE L1 确定性校验),
//           M8(Orchestrator - deny-by-default 策略)
// @producer: pkg/governance/policy/engine.go (Cedar CGO FFI, deny-by-default + forbid-overrides-permit)
// @arch: docs/arch/11-Policy-Safety-深度选型.md §3
// ============================================================================

// PolicyGate 是 Cedar 策略引擎的 Go 接口。
// 原则: deny-by-default + forbid 无条件优先于 permit。
// FFI 调用失败 → deny（fail-closed）。Evaluate 超时 >10ms → deny + 计数器递增。
// 连续 10 次 Evaluate 失败 → KillSwitch Stage 1 THROTTLE。
type PolicyGate interface {
	IsAuthorized(ctx context.Context, principal, action, resource string, context map[string]any) (bool, error)
	Review(ctx context.Context, req PolicyReviewRequest) (PolicyReviewResult, error)
}

// PermissionMode 定义外部扩展调用的权限模式。
type PermissionMode string

const (
	ModeDefault    PermissionMode = "default"
	ModeAutoReview PermissionMode = "auto_review"
	ModeFullAccess PermissionMode = "full_access"
)

// PreferencesRepo 提供对系统偏好的访问。
type PreferencesRepo interface {
	GetPermissionMode(ctx context.Context) (PermissionMode, error)
	SetPermissionMode(ctx context.Context, mode PermissionMode) error
}

// ============================================================================
// M11 Policy & Safety — SafeDialer (统一安全拨号器)
// @consumer: M7(Tool Sandbox - 网络出口连接), M10(Connector - 远程数据源拉取),
//           M13(Interface-Scheduler - HTTP/gRPC/WebSocket 出站连接),
//           M1(Inference - LLM API 调用网络出口)
// @producer: M11(Policy-Safety - 唯一实现, 封装 SSRFGuard 五阶段校验)
// @arch: docs/arch/11-Policy-Safety-深度选型.md §6
// ============================================================================

// SafeDialer 是统一安全拨号器。
// 强制所有出站连接（HTTP/gRPC/WebSocket）使用，封装 SSRFGuard 五阶段校验:
//
//	Phase 0: Capability Token 出口强制
//	Phase 1: DNS 解析
//	Phase 2: blockedCIDRs 校验（内网地址段 + loopback 阻止）
//	Phase 3: 50ms TOCTOU 延迟后二次 DNS 解析 + 重新 CIDR 校验
//	Phase 3.5: 响应 IP 数 >20 → 拒绝
//	Phase 4: DNS TOCTOU 消除 —— 覆写 DialContext 锁定验证后的 IP
//
// M11 导出此接口，CI safe_dialer_lint 扫描裸 net.Dial/grpc.Dial/http.Get → ERROR。
type SafeDialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// ============================================================================
// M5 Memory System — 四层记忆接口
// @consumer: M4(Agent Kernel - 上下文检索、ImmutableCore 加载),
//           M10(Knowledge-RAG - 文档索引写入、实体存储)
// @producer: pkg/cognition/memory/ (四层物理实现)
// @arch: docs/arch/M05-Memory-System.md
// ============================================================================

// Memory 是四层记忆的统一 facade（Mem-L0..L3 + 元认知反思层）。
type Memory interface {
	Working() WorkingMemory
	Episodic() EpisodicMemory
	Semantic() SemanticMemory
	Procedural() ProceduralMemory
	Retriever() HybridRetriever
	Reflection() ReflectionMemory // 元认知反思层，M05 §3.4
	StoreStats() (string, error)
	SetVectorMode(mode int) error
}

// ReflectionMemory 元认知反思层（Mem-L1.5，插于 Episodic 与 Semantic 之间）。
// 存储失败原因、策略切换决策、元认知观察。
// 区别于 PersonaRefiner（PersonaRefiner 调整偏好，ReflectionMemory 记录元决策）。
// @consumer: M4(Agent Kernel - 每轮反思写入), M9(Self-Improve - 反思数据驱动蒸馏)
// @producer: pkg/cognition/memory/ (ReflectionMem 实现)
// @arch: docs/arch/M05-Memory-System.md §3.4
type ReflectionMemory interface {
	AppendReflection(ctx context.Context, entry ReflectionEntry) error
	QueryReflections(ctx context.Context, q ReflectionQuery) ([]ReflectionEntry, error)
}

// ReflectionEntry 单条元认知反思记录。
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
	TaskType  string // 跨会话按任务类型过滤（M05 §3.4 S_PERCEIVE 注入）
	Topic     string // 主题词过滤：匹配 Decision 或 Strategy 字段
	K         int    // 返回最近 K 条，0 = 不限
}

// WorkingMemory (Mem-L0) — 进程内，非持久化（Context + Scratch）+ 跨会话持久化（Notes）。
// Notes() 可由 SQLNotesStore 实现跨会话持久化，其余字段仍为进程内状态。
type WorkingMemory interface {
	Immutable() ImmutableCore
	Context() ContextWindow
	Scratch() ScratchPad
	Notes() NotesStore // M05 §2.2 跨会话轻量笔记，SQL 持久化
}

// NotesStore 跨会话轻量笔记存储（M05 §2.2）。
// DDL 权威源：internal/protocol/schema/023_notes.sql
// @consumer: M4(S_PERCEIVE 注入), M13(API read/write)
// @producer: pkg/cognition/memory/ (SQLNotesStore / InMemNotesStore)
type NotesStore interface {
	Get(ctx context.Context, key string) (*Note, error)
	Set(ctx context.Context, key, content string, tags []string) error
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, tag string) ([]Note, error)
	ListByTask(ctx context.Context, taskID string) ([]Note, error)
	GC(ctx context.Context) (int, error) // 清理已过期 Note，返回删除条数
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

// ImmutableCore — 永不裁剪的核心区，写入经 M9 staging + M11 闸控。
type ImmutableCore interface {
	Load(ctx context.Context, userID string, sessionID string) (ImmutableCoreView, error)
	PrependToMessages(msgs []Message) []Message
}

type ImmutableCoreView struct {
	UserPrefs   []UserPreference
	SessionGoal string
	SafetyRules []SafetyRule
}

type UserPreference struct {
	Dimension      string
	PreferenceText string
	Confidence     float64
	ProvenanceID   string // staging_candidates full_promotion ID
}

type SafetyRule struct {
	RuleText string
	Severity string // info / warn / block
	Scope    string
}

// ContextWindow — 上下文窗口管理。ImmutableCore 不参与压缩。
type ContextWindow interface {
	Append(msg Message)
	Compress(ctx context.Context, targetTokens int) error
	Tokens() int
	Messages() []Message
}

// ScratchPad — 任务级临时键值存储。
type ScratchPad interface {
	Set(key string, value any)
	Get(key string) (any, bool)
	Clear()
}

// EpisodicMemory (Mem-L1) — 事件表 + 向量投影。
type EpisodicMemory interface {
	Append(ctx context.Context, ev Event) error
	Query(ctx context.Context, q EpisodicQuery) ([]ScoredEvent, error)
	MarkCold(ctx context.Context, sessionID string, before time.Time) (int, error)
}

type EpisodicQuery struct {
	SessionID string
	Topics    []string
	Semantic  string // 语义搜索文本
	K         int
}

type ScoredEvent struct {
	Event Event
	Score float64
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

	// 生命周期字段（信念修正 + 知识演化）
	// 来源: supermemory temporal belief revision + PruneMem lifecycle governance
	DBID         int64  // 数据库自增 ID，供 MarkEntitySuperseded 使用
	Status       string // 'active'(默认) | 'superseded' | 'expired' | 'merged'
	SupersededBy int64  // status='superseded' 时指向新版本实体的 DBID
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
}

// UserProfile 用户画像（L3 Persona 等价物）。
// 由 M5 ConsolidationPipeline Stage 3.5 每 50 条新事件自动合成，随使用演化。
// 来源收敛: supermemory User Profile + TencentDB L3 Persona。
type UserProfile struct {
	ProfileKey         string         `json:"profile_key"`         // 默认 'default'
	StableFacts        map[string]any `json:"stable_facts"`        // 低频变化事实（角色/技能/偏好）
	RecentActivity     []string       `json:"recent_activity"`     // 近 7d 行为摘要（最多 20 条）
	BehavioralPatterns map[string]any `json:"behavioral_patterns"` // 工具频率/编码风格/沟通习惯
	SynthesisCount     int            `json:"synthesis_count"`     // 累计合成次数
	LastEventTS        int64          `json:"last_event_ts"`       // 最后消费事件的 Unix 毫秒时间戳
}

// SemanticMemory (Mem-L2) — 文档/实体/关系图。
type SemanticMemory interface {
	StoreDocument(ctx context.Context, doc Document) error
	StoreChunks(ctx context.Context, docID string, chunks []Chunk) error
	GetDocument(ctx context.Context, id string) (*Document, error)
	Archive(ctx context.Context, id string, reason string) error
	UpsertFact(ctx context.Context, entity Entity) error
	UpsertRelation(ctx context.Context, rel Relation) error
	GetEntity(ctx context.Context, entityType, name string) (*Entity, error)

	// 生命周期接口 — 信念修正与知识演化（缺口 1）
	ListActiveEntities(ctx context.Context, entityType string, limit int) ([]Entity, error)
	MarkEntitySuperseded(ctx context.Context, oldDBID int64, newDBID int64) error

	// 用户画像接口 — L3 Persona 合成与查询（缺口 3）
	UpsertUserProfile(ctx context.Context, profile UserProfile) error
	GetUserProfile(ctx context.Context, profileKey string) (*UserProfile, error)
	StoreStats() (string, error)
	SetVectorMode(mode int) error
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

// ProceduralMemory (Mem-L3) — 技能索引，委托 M6 SkillRegistry。
type ProceduralMemory interface {
	Skills() SkillRegistry
}

// ============================================================================
// M6 Skill Library — Skill 注册与选择
// @consumer: M4(Agent Kernel - System 1 技能路由),
//           M9(Self-Improve - Logic Collapse 入库)
// @producer: pkg/cognition/skill/ (SkillRegistry + SkillSelector 实现)
// @arch: docs/arch/M06-Skill-Library.md
// ============================================================================

// SkillRegistry 是技能注册表。
// 未签名 skill 不可加载（cosign 验证失败 → signature_valid=false，Registry 拒绝返回）。
type SkillRegistry interface {
	Register(ctx context.Context, meta SkillMeta) error
	Get(ctx context.Context, name, version string) (*SkillMeta, error)
	List(ctx context.Context, filter SkillFilter) ([]SkillMeta, error)
	Deprecate(ctx context.Context, name, version string, reason string) error
}

type SkillMeta struct {
	Name         string
	Version      string // semver
	Runtime      string // script (default) / builtin
	RiskLevel    string // low / medium / high
	Sandbox      int    // Sbx-L1=1 / Sbx-L3=3
	Capabilities []string
	ExecMode     string    // tool / ambient
	Trust        TrustTier // 替代 SignatureValid bool（ADR-0016 §2.1）
	Idempotent   bool
	Benchmarks   SkillBenchmarks
	Instructions string // SKILL.md 全文，供 LLM tool_use 返回
	Deprecated   bool
	ScriptPath   string // marketplace 安装路径（extension_instances.install_path + "/src/index.ts"）
	// PluginID 是来源插件的 plugins.id（"pl_xxx"）；独立安装的技能为空。
	PluginID string
}

type SkillBenchmarks struct {
	PassRate     float64
	AvgLatencyMs float64
	AvgTokens    float64
}

type SkillFilter struct {
	Capabilities      []string
	RiskLevelMax      string
	IncludeDeprecated bool
}

// SkillSelector — 启发式 + 向量 + 排序公式。不调 LLM。
type SkillSelector interface {
	Select(ctx context.Context, hint TaskHint) ([]SkillMeta, error)
}

type TaskHint struct {
	TaskType           string
	CapabilitiesNeeded []string
	ComplexityScore    float64
}

// ============================================================================
// M7 Tool & Action — Sandbox + Tool Executor
// @consumer: M4(Agent Kernel - DAG 节点执行),
//           M6(Skill Library - Wasm 技能执行)
// @producer: pkg/action/sandbox/ (SandboxProvider 实现)
// @arch: docs/arch/M07-Tool-Action-Layer.md
// ============================================================================

// SandboxProvider 是分级沙箱抽象（Sbx-L1/L2/L3）。
type SandboxProvider interface {
	Level() int // 1=InProc, 2=Wasmtime, 3=gVisor/microVM
	Run(ctx context.Context, spec SandboxSpec) (*SandboxResult, error)
}

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

type SandboxResult struct {
	Output     []byte
	ExitCode   int
	LatencyMs  int64
	MemoryPeak int64
}

// ToolExecutor — 工具执行器，含 DryRun 保护。
type ToolExecutor interface {
	Execute(ctx context.Context, call ToolCallRequest) (*ToolResult, error)
	ExecuteDryRun(ctx context.Context, call ToolCallRequest) (*ToolResult, error)
	Cancel(ctx context.Context, callID string) error
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

// ============================================================================
// M9 Self-Improvement — Staging Manager
// @consumer: M9(Self-Improve - 7 worker 产出候选),
//           M11(Policy-Safety - schema_validate 阶段),
//           M12(Eval-Harness - initial_eval 阶段)
// @producer: pkg/swarm/staging/ (StagingManager 实现)
// @arch: docs/arch/M09-Self-Improvement-Engine.md
// ============================================================================

// StagingManager 驱动 7 阶段流水线。
type StagingManager interface {
	Submit(ctx context.Context, c StagingCandidate) (string, error)
	GetStage(ctx context.Context, id string) (string, error)
	Promote(ctx context.Context, id string) error // 通过当前阶段 → 下一阶段
	Reject(ctx context.Context, id string, reason string) error
	Rollback(ctx context.Context, id string, reason string) error
}

type StagingCandidate struct {
	Type           string // skill / lora / prompt / config / source_patch / user_preference
	EvolutionLevel string // Evo-L0..L4
	SourceWorker   string
	PayloadPath    string
}

// ============================================================================
// M10 Knowledge & RAG — Connector
// @consumer: M10(Knowledge-RAG - 外部数据源接入)
// @producer: pkg/swarm/ (各 Connector 实现)
// @arch: docs/arch/M10-Knowledge-RAG.md §1.2
// ============================================================================

// Connector 是外部数据源的标准化接入接口。
type Connector interface {
	ID() string
	Name() string
	List(ctx context.Context) ([]*DocumentRef, error)
	Fetch(ctx context.Context, ref *DocumentRef) (*SyncDocument, error)
	Watch(ctx context.Context) (<-chan ChangeEvent, error)
	SyncConfig() SyncConfig
}

// ============================================================================
// M12 Eval Harness — EvalRunner
// @consumer: M9(Self-Improve - staging 阶段 3-5),
//           M4(Agent Kernel - verify_step 轻量评测)
// @producer: pkg/governance/eval/ (EvalRunner 实现)
// @arch: docs/arch/M12-Eval-Harness.md
// ============================================================================

// EvalRunner 执行评测套件。
// safety case 一票否决: newly_failing safety → reject（无视整体 pass_rate）。
type EvalRunner interface {
	RunSuite(ctx context.Context, suite string, candidateID string) (*EvalRunReport, error)
	RunReplay(ctx context.Context, sessionID string) (*ReplayReport, error)
	Cancel(ctx context.Context, runID string) error
}

type EvalRunReport struct {
	Suite      string `json:"suite"`
	TotalCases int    `json:"total_cases"`
	PassCount  int    `json:"pass_count"`
	FailCount  int    `json:"fail_count"`
	SafetyFail int    `json:"safety_fail"` // 一票否决计数
	// SkippedLowFalsifiability 是因 FalsifiabilityScore < 阈值而跳过 L4 评分的用例数（Gap-B）。
	// 该比例 = SkippedLowFalsifiability / TotalCases，反映 eval 套件的可评分质量。
	SkippedLowFalsifiability int    `json:"skipped_low_falsifiability,omitempty"`
	Status                   string `json:"status"`
}

type ReplayReport struct {
	SessionID       string
	Consistent      bool
	DivergentOffset int64
	NewLLMCalls     int // 必须为零（g_inv_08）
}

// EvalAPI 暴露给自进化引擎的内部只读数据接口
type EvalAPI interface {
	// GetTrainingCases 获取用于训练和优化的评测用例。
	// signature 必须是用 agentRole 对应 Ed25519 私钥对请求参数及时间戳的签名。
	GetTrainingCases(ctx context.Context, agentRole string, signature []byte) ([]any, error) // 返回 []governance.EvalCase

	// GetValidationCases 获取用于泛化验证的评测用例。
	GetValidationCases(ctx context.Context, agentRole string, signature []byte) ([]any, error) // 返回 []governance.EvalCase
}

// ============================================================================
// M13 Interface & Scheduler — Scheduler + HITL
// @consumer: M8(Orchestrator - 任务调度),
//           M4(Agent Kernel - 异步任务提交)
// @producer: pkg/edge/scheduler/ (Scheduler 实现)
// @arch: docs/arch/M13-Interface-Scheduler.md
// ============================================================================

// Scheduler 是任务调度器。
// CAS 抢占: UPDATE tasks SET state='running', worker_id=? WHERE id=? AND state='pending'
type Scheduler interface {
	Submit(ctx context.Context, task Task) (string, error)
	Get(ctx context.Context, id string) (*Task, error)
	Cancel(ctx context.Context, id string) error
	Subscribe(ctx context.Context, taskID string) (<-chan TaskEvent, error)
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

type TaskEvent struct {
	TaskID string
	State  string // submitted / started / progress / completed / failed / cancelled
	Detail map[string]any
}

// HITL 是人工审批网关。
type HITL interface {
	Prompt(ctx context.Context, p HITLPrompt) (*HITLResponse, error)
	Respond(ctx context.Context, checkpointID string, response HITLResponse) error
	Pending(ctx context.Context) ([]HITLPrompt, error)
}

type HITLPrompt struct {
	ID             string
	CheckpointType string
	PromptText     string
	Options        []HITLOption
	DeadlineNs     int64
}

type HITLOption struct {
	Key   string
	Label string
}

type HITLResponse struct {
	OptionKey string
	UserID    string
}

// ============================================================================
// Agent 控制接口
// ============================================================================

// InterruptAction 中断处理语义。
type InterruptAction int

const (
	InterruptResume   InterruptAction = iota // 恢复执行（回到被中断的状态）
	InterruptRedirect                        // 重新规划（新意图 → S_PERCEIVE）
	InterruptAbort                           // 终止任务 → S_FAILED
)

// InterruptRequest 用户中断请求。
type InterruptRequest struct {
	Reason   string // 中断原因（供 Audit 记录）
	Action   InterruptAction
	Redirect string // Action=InterruptRedirect 时的新意图文本
}

// AgentController 供 gateway 调用的 Agent 控制接口（consumer-side）
type AgentController interface {
	AgentID() string
	SetTaskIntent(intent []byte)
	SendIntent(trigger AgentTrigger) error
	SurpriseIndex() float64
	Memory() Memory
	Interrupt(req InterruptRequest)
	SetPreferences(map[string]string)
	CurrentState() AgentState
	ConfigInfo() map[string]any
}
