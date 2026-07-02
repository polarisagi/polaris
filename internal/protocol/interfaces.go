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
	"database/sql"
	"net"
	"time"

	"github.com/polarisagi/polaris/internal/observability/budget"
	"github.com/polarisagi/polaris/internal/protocol/pb"
	"github.com/polarisagi/polaris/pkg/types"
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
	Infer(ctx context.Context, messages []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error)
	StreamInfer(ctx context.Context, messages []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error)
	Capabilities() types.ProviderCapabilities
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
	BatchWrite(ctx context.Context, ops []types.Op) error
	Txn(ctx context.Context, fn func(tx Transaction) error) error
	Capabilities() types.StoreCapabilities
	Close() error
}

// StoreExtStats 为底层存储引擎可选的统计扩展。
// 由具体 Store 实现提供，上层通过类型断言进行安全调用。
type StoreExtStats interface {
	Stats() (string, error)
}

// StoreExtBackup 提供备份导入导出扩展。
type StoreExtBackup interface {
	ImportBackupRow(ctx context.Context, table string, row map[string]any) error
}

// StoreExtPreferences 提供偏好设置的存储扩展。
type StoreExtPreferences interface {
	LoadAllPreferences(ctx context.Context) (map[string]string, error)
	SetPreference(ctx context.Context, key, value string) error
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
	AppendDecision(ctx context.Context, entry *types.DecisionLogEntry) error
}

// EventWriter 异步写入事件
type EventWriter interface {
	WriteEvent(ctx context.Context, evType string, payload map[string]any) error
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
	Score(ctx context.Context, step types.StepContext) float64
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
	Fn func(ctx context.Context, sCtx StateContext) (types.State, error)
}

func (DeterministicEffect) IsLLMFill() bool { return false }

// LLMFillEffect LLM 协处理器副作用——重放时从 EventLog 录像取响应。
// PromptFn 必须是纯函数（同 StateContext → 同 prompt 字节，par_inv_03）。
type LLMFillEffect struct {
	SchemaRef      string                                                    // → spec/schemas.json
	PromptFn       func(sCtx StateContext) []types.Message                   // 纯函数
	OnSuccess      func(sCtx StateContext, fill []byte) (types.State, error) // LLM 产出 → 下一状态
	OnFailure      func(sCtx StateContext, err error) (types.State, error)   // LLM 失败 → 错误状态
	MaxRetry       int
	ModelPool      string // budget / standard / reasoning
	ThinkingMode   types.ThinkingMode
	IdempotencyKey types.IdempotencyKey
}

func (LLMFillEffect) IsLLMFill() bool { return true }

// StateMachine 是 M4 状态机的执行接口。
// 定义见 spec/state.yaml §par。LLM 不直接驱动状态变迁，Go 确定性推进。
type StateMachine interface {
	Initial() types.State
	Dispatch(ctx context.Context, sCtx StateContext, ev types.StateEvent) (next types.State, effects []Effect, err error)
}

// StateContext 穿越状态机各转移的共享上下文。
type StateContext struct {
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
	Search(ctx context.Context, query string, scope types.SearchScope, config types.RetrievalConfig) ([]types.ScoredFragment, error)
}

// ============================================================================
// M6 types.Skill Library — types.Skill Executor
// @consumer: M4(Agent Kernel - System 1 技能缓存命中后执行脚本)
// @producer: M7(types.Tool-Action - Rust 沙箱, [Sandbox] 权威实现)
// @arch: docs/arch/M06-types.Skill-Library.md §5
// ============================================================================

// SkillExecutor 执行 TypeScript/Python 技能脚本。
// 脚本由 M7 负责沙箱执行（M7 是沙箱的 CANONICAL SOURCE）。
type SkillExecutor interface {
	ExecuteSkill(ctx context.Context, skillID string, input []byte) ([]byte, error)
	ValidateSkill(scriptBytes []byte) error
}

// ============================================================================
// M7 types.Tool & Action — ToolRegistry
// @consumer: M4(Agent Kernel - DAG 节点执行时通过 ExecuteTool 调用工具),
//           M6(types.Skill Library - 注册新技能为工具),
//           M8(Orchestrator - Agent 能力发现时查询可用工具列表)
// @producer: pkg/action/ (ToolRegistry 实现, 包含 MCP Client/Server 注册)
// @arch: docs/arch/07-types.Tool-Action-Layer-深度选型.md §3
// ============================================================================

// ToolRegistry 是工具发现、注册、执行的统一入口。
// 工具来源: Built-in(~20) | MCP(inf) | types.Skill(inf) | A2A(inf) | LLM-generated(临时, [Sandbox-L3])。
// 执行路径: ExecuteTool → Policy Gate(五阶段) → Sandbox → ToolResult。
type ToolRegistry interface {
	Register(tool types.Tool) error
	Lookup(name string) (types.Tool, error)
	List() []types.Tool
	ExecuteTool(ctx context.Context, name string, input []byte, taintLevel types.TaintLevel) (*types.ToolResult, error)
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
	PostTask(ctx context.Context, task *types.TaskEntry) error
	PostBatch(ctx context.Context, tasks []*types.TaskEntry) error
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
	PeekTask(ctx context.Context, taskID string) (*types.TaskSnapshot, error)
	Subscribe(ctx context.Context) (<-chan types.BlackboardEvent, error)
	// UpdateTaskTokens 记录本任务的 token 消耗（Gap-A, HE-Rule-1）。
	// 由 Worker.tryClaimAndExecute 在 AgentKernel.Run 返回后调用。
	// 幂等：多次调用以最后一次写入为准（覆盖，不累加）。
	UpdateTaskTokens(ctx context.Context, taskID string, tokensIn, tokensOut, cacheRead int, costUSD float64) error
	// CountByStatus 返回处于任一给定状态的任务数（活跃度信号，只读）。无参时返回 0。
	CountByStatus(statuses ...types.TaskStatus) int
	// MaxActivePriority 返回活跃任务（Claimed/Executing）的最高优先级（0=最高；无活跃任务返回 3=最低）。
	MaxActivePriority() int
}

// ============================================================================
// M11 Policy & Safety — PolicyGate (Cedar 策略引擎)
// @consumer: M7(types.Tool-Action - 工具调用前 Policy Gate 五阶段评估),
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
	Review(ctx context.Context, req types.PolicyReviewRequest) (types.PolicyReviewResult, error)
}

// PermissionMode 定义外部扩展调用的权限模式。

// PreferencesRepo 提供对系统偏好的访问。
type PreferencesRepo interface {
	GetPermissionMode(ctx context.Context) (types.PermissionMode, error)
	SetPermissionMode(ctx context.Context, mode types.PermissionMode) error
}

// ============================================================================
// M11 Policy & Safety — SafeDialer (统一安全拨号器)
// @consumer: M7(types.Tool Sandbox - 网络出口连接), M10(Connector - 远程数据源拉取),
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

// MemorySystem 是四层记忆的具体子系统集合。保留作为协议契约供内部使用。
type MemorySystem interface {
	Working() WorkingMemory
	Episodic() EpisodicMemory
	Semantic() SemanticMemory
	Procedural() ProceduralMemory
	Retriever() HybridRetriever
	Reflection() ReflectionMemory // 元认知反思层，M05 §3.4
	StoreStats() (string, error)
	SetVectorMode(mode int) error
	GetMemoryPressure() budget.ResourceBudget

	// TaskMermaidCanvas（M05 §11.3）：工具调用符号化画布，跨 Agent/Gateway 共享的
	// 当前任务执行状态追踪。TrackToolCall/TrackToolResult 由 agent 工具执行闭环调用，
	// RenderTaskCanvas 供 gateway 只读展示（GET /v1/agent/mmd-canvas）。
	TrackToolCall(toolUseID, toolName string)
	TrackToolResult(toolUseID string, success bool, summary string)
	RenderTaskCanvas() string
}

// MemoryFacade 记忆系统对外统一门面，屏蔽底层架构，供 Agent / Server 侧直接调用。
type MemoryFacade interface {
	// 基础控制
	StoreStats() (string, error)
	GetMemoryPressure() budget.ResourceBudget

	// Semantic 层调用
	SearchEntities(ctx context.Context, query string, topK int, maxTaint int) ([]types.Entity, error)
	GetUserProfile(ctx context.Context, userID string) (*types.UserProfile, error)

	// Episodic 层调用
	QueryEpisodicEvents(ctx context.Context, query types.EpisodicQuery) ([]types.ScoredEvent, error)
	AppendEpisodicEvent(ctx context.Context, event types.Event, taintLevel types.TaintLevel) error
	ArchiveEpisodic(ctx context.Context, sessionID string) error

	// Working 层调用
	AddWorkingContext(ctx context.Context, text string) error
	SetWorkingScratch(key string, val []byte)
	ImmutableCore() ImmutableCore // 返回 *store.ImmutableCore 或其他不可变核心

	// Reflection 层调用
	QueryReflections(ctx context.Context, q types.ReflectionQuery) ([]types.ReflectionEntry, error)
	AppendReflection(ctx context.Context, entry types.ReflectionEntry) error

	// 后台维护调用（供 swarm.MemoryAgent 等常驻 goroutine 使用，替代直接 import internal/memory/graph、
	// internal/memory/store 或裸 SQL，见 docs/specs/04-Module-Boundary.md §B2）
	ScanHighSalienceEvents(ctx context.Context, sinceID int64, minSalience float64, limit int) ([]types.SalienceEvent, error)
	PruneMemoryGraph(ctx context.Context) error

	// TaskMermaidCanvas（M05 §11.3）：agent 工具执行闭环调用 TrackToolCall/TrackToolResult
	// 记录当前任务的工具调用轨迹，gateway（GET /v1/agent/mmd-canvas）经 RenderTaskCanvas 只读展示。
	TrackToolCall(toolUseID, toolName string)
	TrackToolResult(toolUseID string, success bool, summary string)
	RenderTaskCanvas() string
}

// ReflectionMemory 元认知反思层（Mem-L1.5，插于 Episodic 与 Semantic 之间）。
// 存储失败原因、策略切换决策、元认知观察。
// 区别于 PersonaRefiner（PersonaRefiner 调整偏好，ReflectionMemory 记录元决策）。
// @consumer: M4(Agent Kernel - 每轮反思写入), M9(Self-Improve - 反思数据驱动蒸馏)
// @producer: pkg/cognition/memory/ (ReflectionMem 实现)
// @arch: docs/arch/M05-Memory-System.md §3.4
type ReflectionMemory interface {
	AppendReflection(ctx context.Context, entry types.ReflectionEntry) error
	QueryReflections(ctx context.Context, q types.ReflectionQuery) ([]types.ReflectionEntry, error)
}

// ReflectionEntry 单条元认知反思记录。

// 失败原因
// 策略切换描述
// 元决策内容

// ReflectionQuery 反思记录查询参数。

// 跨会话按任务类型过滤（M05 §3.4 S_PERCEIVE 注入）
// 主题词过滤：匹配 Decision 或 Strategy 字段
// 返回最近 K 条，0 = 不限

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
	Get(ctx context.Context, key string) (*types.Note, error)
	Set(ctx context.Context, key, content string, tags []string, expectedVersion int) error
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, tag string) ([]types.Note, error)
	ListByTask(ctx context.Context, taskID string) ([]types.Note, error)
	GC(ctx context.Context) (int, error) // 清理已过期 types.Note，返回删除条数
}

// AppRow represents a custom app.

// ImmutableCore — 永不裁剪的核心区，写入经 M9 staging + M11 闸控。
type ImmutableCore interface {
	Load(ctx context.Context, userID string, sessionID string) (types.ImmutableCoreView, error)
	// Fields 返回可写字段集合（ImmutableCoreFields）指针，供 gateway 等消费方组装系统提示词。
	// 取代此前 `.(*store.ImmutableCore)` 类型断言（M04 §B2）。
	Fields() *ImmutableCoreFields
	PrependToMessages(msgs []types.Message) []types.Message
}

// staging_candidates full_promotion ID

// info / warn / block

// ContextWindow — 上下文窗口管理。ImmutableCore 不参与压缩。
type ContextWindow interface {
	Append(msg types.Message)
	Compress(ctx context.Context, targetTokens int) error
	Tokens() int
	Messages() []types.Message
}

// ScratchPad — 任务级临时键值存储。
type ScratchPad interface {
	Set(key string, value any)
	Get(key string) (any, bool)
	Clear()
}

// EpisodicMemory (Mem-L1) — 事件表 + 向量投影。
type EpisodicMemory interface {
	Append(ctx context.Context, ev types.Event, taint types.TaintLevel) error
	Query(ctx context.Context, q types.EpisodicQuery) ([]types.ScoredEvent, error)
	MarkCold(ctx context.Context, sessionID string, before time.Time) (int, error)
	// ScanHighSalience 按显著性阈值 + 高水位标记扫描物化表 episodic_events。
	// 供后台维护 Agent（如 swarm.MemoryAgent）生成耳语提示，替代绕过本接口的裸 SQL。
	ScanHighSalience(ctx context.Context, sinceID int64, minSalience float64, limit int) ([]types.SalienceEvent, error)
}

// 语义搜索文本

// UserProfile 用户画像（L3 Persona 等价物）。
// 由 M5 ConsolidationPipeline Stage 3.5 每 50 条新事件自动合成，随使用演化。
// 来源收敛: supermemory User Profile + TencentDB L3 Persona。

// 默认 'default'
// 低频变化事实（角色/技能/偏好）
// 近 7d 行为摘要（最多 20 条）
// 工具频率/编码风格/沟通习惯
// 累计合成次数
// 最后消费事件的 Unix 毫秒时间戳

// SemanticMemory (Mem-L2) — 文档/实体/关系图。
type SemanticMemory interface {
	StoreDocument(ctx context.Context, doc types.Document, taint types.TaintLevel) error
	StoreChunks(ctx context.Context, docID string, chunks []types.Chunk, taint types.TaintLevel) error
	GetDocument(ctx context.Context, id string) (*types.Document, error)
	Archive(ctx context.Context, id string, reason string) error
	UpsertFact(ctx context.Context, entity types.Entity, taint types.TaintLevel) error
	UpsertRelation(ctx context.Context, rel types.Relation, taint types.TaintLevel) error
	GetEntity(ctx context.Context, entityType, name string) (*types.Entity, error)

	// 生命周期接口 — 信念修正与知识演化（缺口 1）
	ListActiveEntities(ctx context.Context, entityType string, limit int) ([]types.Entity, error)
	SearchEntities(ctx context.Context, query string, limit int) ([]types.Entity, error)
	MarkEntitySuperseded(ctx context.Context, oldDBID int64, newDBID int64) error

	// 用户画像接口 — L3 Persona 合成与查询（缺口 3）
	UpsertUserProfile(ctx context.Context, profile types.UserProfile) error
	GetUserProfile(ctx context.Context, profileKey string) (*types.UserProfile, error)
	StoreStats() (string, error)
	SetVectorMode(mode int) error
}

// ProceduralMemory (Mem-L3) — 技能索引，委托 M6 SkillRegistry。
type ProceduralMemory interface {
	Skills() SkillRegistry
}

// ============================================================================
// M6 types.Skill Library — types.Skill 注册与选择
// @consumer: M4(Agent Kernel - System 1 技能路由),
//           M9(Self-Improve - Logic Collapse 入库)
// @producer: pkg/cognition/skill/ (SkillRegistry + SkillSelector 实现)
// @arch: docs/arch/M06-types.Skill-Library.md
// ============================================================================

// SkillRegistry 是技能注册表。
// 未签名 skill 不可加载（cosign 验证失败 → signature_valid=false，Registry 拒绝返回）。
type SkillRegistry interface {
	Register(ctx context.Context, meta types.SkillMeta) error
	Get(ctx context.Context, name, version string) (*types.SkillMeta, error)
	List(ctx context.Context, filter types.SkillFilter) ([]types.SkillMeta, error)
	Deprecate(ctx context.Context, name, version string, reason string) error
}

// SkillSelector — 启发式 + 向量 + 排序公式。不调 LLM。
type SkillSelector interface {
	Select(ctx context.Context, hint types.TaskHint) ([]types.SkillMeta, error)
}

// ============================================================================
// M7 types.Tool & Action — Sandbox + types.Tool Executor
// @consumer: M4(Agent Kernel - DAG 节点执行),
//           M6(types.Skill Library - Wasm 技能执行)
// @producer: pkg/action/sandbox/ (SandboxProvider 实现)
// @arch: docs/arch/M07-types.Tool-Action-Layer.md
// ============================================================================

// SandboxProvider 是分级沙箱抽象（Sbx-L1/L2/L3）。
type SandboxProvider interface {
	Level() int // 1=InProc, 2=Wasmtime, 3=gVisor/microVM
	Run(ctx context.Context, spec types.SandboxSpec) (*types.SandboxResult, error)
}

// seconds

// ToolExecutor — 工具执行器，含 DryRun 保护。
type ToolExecutor interface {
	Execute(ctx context.Context, call types.ToolCallRequest) (*types.ToolResult, error)
	ExecuteDryRun(ctx context.Context, call types.ToolCallRequest) (*types.ToolResult, error)
	Cancel(ctx context.Context, callID string) error
	// RecordAudit 写入工具调用的全链路审计记录。
	RecordAudit(ctx context.Context, toolName string, payload []byte) error
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
	Submit(ctx context.Context, c types.StagingCandidate) (string, error)
	GetStage(ctx context.Context, id string) (string, error)
	Promote(ctx context.Context, id string) error // 通过当前阶段 → 下一阶段
	Reject(ctx context.Context, id string, reason string) error
	Rollback(ctx context.Context, id string, reason string) error
}

// skill / lora / prompt / config / source_patch / user_preference
// Evo-L0..L4

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
	List(ctx context.Context) ([]*types.DocumentRef, error)
	Fetch(ctx context.Context, ref *types.DocumentRef) (*types.SyncDocument, error)
	Watch(ctx context.Context) (<-chan types.ChangeEvent, error)
	SyncConfig() types.SyncConfig
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
	RunSuite(ctx context.Context, suite string, candidateID string) (*types.EvalRunReport, error)
	RunReplay(ctx context.Context, sessionID string) (*types.ReplayReport, error)
	Cancel(ctx context.Context, runID string) error
}

// 一票否决计数
// SkippedLowFalsifiability 是因 FalsifiabilityScore < 阈值而跳过 L4 评分的用例数（Gap-B）。
// 该比例 = SkippedLowFalsifiability / TotalCases，反映 eval 套件的可评分质量。

// 必须为零（g_inv_08）

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
	Submit(ctx context.Context, task types.Task) (string, error)
	Get(ctx context.Context, id string) (*types.Task, error)
	Cancel(ctx context.Context, id string) error
	Subscribe(ctx context.Context, taskID string) (<-chan types.TaskEvent, error)
}

// submitted / started / progress / completed / failed / cancelled

// HITL 是人工审批网关。
type HITL interface {
	Prompt(ctx context.Context, p types.HITLPrompt) (*types.HITLResponse, error)
	Respond(ctx context.Context, checkpointID string, response types.HITLResponse) error
	Pending(ctx context.Context) ([]types.HITLPrompt, error)
}

// DecisionEtag 决策时刻的 Cedar policy etag，auto_approve 前校验原子性（M13 §2.4）。

// ============================================================================
// Agent 控制接口
// ============================================================================

// InterruptAction 中断处理语义。

// 恢复执行（回到被中断的状态）
// 重新规划（新意图 → S_PERCEIVE）
// 终止任务 → S_FAILED

// AgentController 供 gateway 调用的 Agent 控制接口（consumer-side）
type AgentController interface {
	AgentID() string
	SetTaskIntent(intent []byte)
	SendIntent(trigger types.AgentTrigger) error
	SurpriseIndex() float64
	Memory() MemoryFacade
	Interrupt(req types.InterruptRequest)
	SetPreferences(map[string]string)
	CurrentState() types.AgentState
	ConfigInfo() map[string]any
}

// ============================================================================
// 轨迹与审计接口
// ============================================================================

// Trajectory 记录单次状态转移的详情。

// TrajectoryStoreReader 提供近期行为轨迹的读取能力。
type TrajectoryStoreReader interface {
	GetRecent(ctx context.Context, n int) ([]types.Trajectory, error)
}

// AuditLogger 提供审计日志记录能力。
type AuditLogger interface {
	Log(ctx context.Context, action string, meta map[string]any) error
}

// ============================================================================
// Storage Access — SQLQuerier 窄接口（database/sql 最小公约数）
// @consumer: pkg/cognition/ pkg/swarm/ pkg/action/ pkg/edge/ pkg/substrate/security/
// @producer: *sql.DB（天然满足，无需适配）
// @arch: docs/upgrade/repo-interface-migration.md §1.1 层A
// ============================================================================

// SQLQuerier 是 *sql.DB 与 *sql.Tx 共同满足的最小 SQL 接口。
// 非存储层包（pkg/cognition/ pkg/swarm/ 等）必须接受此接口而非裸 *sql.DB，
// 以保持层边界。调用方构造时直接传入 *sql.DB，Go 结构化类型自动满足。
type SQLQuerier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// BlackboardDB 是 SQLiteBlackboard 所需的最小 *sql.DB 接口。
// 在 SQLQuerier 基础上扩展事务管理与健康检查，供 M8 多 Agent 协调层使用。
// *sql.DB 自动满足此接口（Go 结构化类型系统），调用方可直接传入 *sql.DB。
type BlackboardDB interface {
	SQLQuerier
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
	PingContext(ctx context.Context) error
}

// ============================================================================
// Domain Repositories — 领域专用读写契约（Layer B）
// @consumer: pkg/gateway/server/, pkg/extensions/, pkg/action/tool/
// @producer: pkg/substrate/storage/repo_*.go
// @arch: docs/upgrade/repo-interface-migration.md §1.1 层B
// ============================================================================

// --- Row 数据类型 ---

// ChatSessionRow 对应 chat_sessions 表一行。

// ChatMessageRow 对应 chat_messages 表一行。

// ProviderRow 对应 providers 表一行。

// ProviderModelRow 对应 provider_models 表一行。

// CronJobRow 对应 cron_jobs 表一行。

// ExtInstanceRow 对应 extension_instances 表一行。

// ExtCatalogRow 对应 extension_catalog 表一行。

// MCPServerRow 对应 mcp_servers 表一行。

// AuditEventRow 是审计日志的单条记录。

// JSON

// TokenCostAgg 是按任务聚合的 Token 费用统计。

// --- Repository 接口 ---

// ChatRepository 会话与消息的完整读写契约。
// @consumer: pkg/gateway/server/
// @producer: pkg/substrate/storage/repo_chat.go
type ChatRepository interface {
	// Sessions
	CreateSession(ctx context.Context, row types.ChatSessionRow) error
	GetSession(ctx context.Context, id string) (*types.ChatSessionRow, error)
	ListSessions(ctx context.Context, limit int) ([]types.ChatSessionRow, error)
	UpdateSessionTitle(ctx context.Context, id, title string) error
	UpdateSessionThrashingIndex(ctx context.Context, id string, idx float64) error
	DeleteSession(ctx context.Context, id string) error

	// Messages
	AppendMessage(ctx context.Context, row types.ChatMessageRow) error
	ListMessages(ctx context.Context, sessionID string, limit int) ([]types.ChatMessageRow, error)
	SearchMessages(ctx context.Context, query string, limit int) ([]types.ChatMessageRow, error)

	// Additional mutations required by gateway/server
	RestoreSession(ctx context.Context, id, title string, thrashing float64, createdAt, updatedAt string) error
	RestoreMessage(ctx context.Context, id, sessionID, role, content, createdAt string) error
	TouchSession(ctx context.Context, id string) error
	ClearNonSystemMessages(ctx context.Context, sessionID string) error
	ReplaceSessionMessages(ctx context.Context, sessionID string, msgs []types.ChatMessageRow) error
}

// ProviderRepository Provider 配置读写契约。
// @consumer: pkg/gateway/server/, pkg/substrate/inference/
// @producer: pkg/substrate/storage/repo_provider.go
type ProviderRepository interface {
	ListProviders(ctx context.Context) ([]types.ProviderRow, error)
	GetProvider(ctx context.Context, id string) (*types.ProviderRow, error)
	CreateProvider(ctx context.Context, p types.ProviderRow) error
	UpdateProvider(ctx context.Context, id string, p types.ProviderRow) error
	DeleteProvider(ctx context.Context, id string) error
	UpsertModel(ctx context.Context, row types.ProviderModelRow) error
	ListModels(ctx context.Context, providerID string) ([]types.ProviderModelRow, error)
	DeleteModel(ctx context.Context, id string) error
	DeleteModelsByProvider(ctx context.Context, providerID string) error

	ClearModelRoles(ctx context.Context, targetRoles []string, exceptID string) error
	SetModelRole(ctx context.Context, id string, role string) error
	// SeedIfEmpty 仅在 providers 表为空时插入默认配置；幂等。
	SeedIfEmpty(ctx context.Context, rows []types.ProviderRow, models []types.ProviderModelRow) error
	// SeedFromEnv 启动时根据环境变量插入或更新凭据。返回 (inserted_bool, error)
	SeedFromEnv(ctx context.Context, p types.ProviderRow) (bool, error)
	UpdateProviderAPIKey(ctx context.Context, id, apiKey, updatedAt string) error
	SeedModelFromEnv(ctx context.Context, m types.ProviderModelRow) error
}

// CronRepository 定时任务读写契约。
// @consumer: pkg/action/tool/cron_tools.go, pkg/gateway/server/ (cron.go)
// @producer: pkg/substrate/storage/repo_cron.go
type CronRepository interface {
	ListCronJobs(ctx context.Context) ([]types.CronJobRow, error)
	GetCronJob(ctx context.Context, id string) (*types.CronJobRow, error)
	CreateCronJob(ctx context.Context, row types.CronJobRow) error
	UpdateCronJob(ctx context.Context, row types.CronJobRow) error
	DeleteCronJob(ctx context.Context, id string) error
	// UpdateCircuitBreaker 更新断路器状态（failure_count / circuit_open / last_error）。
	UpdateCircuitBreaker(ctx context.Context, id string, failureCount int, circuitOpen bool, lastError, circuitOpenedAt string) error
	// UpdateLastRun 更新最近执行时间与下次执行时间。
	UpdateLastRun(ctx context.Context, id, lastRunAt, nextRunAt string) error
}

// ExtensionRepository 扩展安装与目录读写契约。
// @consumer: pkg/extensions/native/, pkg/extensions/marketplace/, pkg/extensions/mcp/
// @producer: pkg/substrate/storage/repo_extension.go
type ExtensionRepository interface {
	// extension_instances
	UpsertInstance(ctx context.Context, row types.ExtInstanceRow) error
	GetInstance(ctx context.Context, id string) (*types.ExtInstanceRow, error)
	UpdateInstanceStatus(ctx context.Context, id, status, errorMsg string) error
	UpdateInstanceInstallPath(ctx context.Context, id, installPath string) error
	ListInstances(ctx context.Context) ([]types.ExtInstanceRow, error)
	DeleteInstance(ctx context.Context, id string) error

	// extension_catalog
	GetCatalogEntry(ctx context.Context, id string) (*types.ExtCatalogRow, error)
	SearchCatalog(ctx context.Context, query string, limit int) ([]types.ExtCatalogRow, error)
	ListCatalogByIDs(ctx context.Context, ids []string) ([]types.ExtCatalogRow, error)
	ReplaceMarketplaceCatalog(ctx context.Context, marketplaceID string, entries []types.ExtCatalogRow) (int, error)
	DeleteOrphanCatalogEntries(ctx context.Context, activeMarketplaceIDs []any) error
	SeedMarketplace(ctx context.Context, row Marketplace) error
	CreateMarketplace(ctx context.Context, row Marketplace) error
	UpdateMarketplace(ctx context.Context, id string, row Marketplace) error
	UpdateMarketplaceSortOrder(ctx context.Context, id string, sortOrder int) error
	DeleteMarketplace(ctx context.Context, id string) (bool, error)
	GetMaxMarketplaceSortOrder(ctx context.Context) (int, error)
	SeedCatalogEntry(ctx context.Context, row types.ExtCatalogRow) error

	// apps
	UpsertApp(ctx context.Context, row types.AppRow) error
	DeleteApp(ctx context.Context, id string) error

	// plugins
	UpsertPlugin(ctx context.Context, id, name, version, displayName, description, publisher, homepage, installPath string, enabled, trustTier int, catalogID, mcpPolicy, manifest, createdAt, updatedAt string) error
	UpdatePluginStatus(ctx context.Context, id string, enabled int, mcpPolicy string, now string) error
	SetPluginComponentsEnabled(ctx context.Context, pluginID string, enabled int, now string) error
	UpdatePluginMCPServerEnabled(ctx context.Context, pluginID, serverID string, enabled int, now string) error
	// mcp_servers
	ListMCPServers(ctx context.Context) ([]types.MCPServerRow, error)
	GetMCPServer(ctx context.Context, id string) (*types.MCPServerRow, error)
	UpsertMCPServer(ctx context.Context, row types.MCPServerRow) error
	UpdateMCPServer(ctx context.Context, id string, fields map[string]any) error
	DeleteMCPServer(ctx context.Context, id string) error

	// Catalog Sync

	// UninstallCleanup 卸载扩展时清理关联数据（mcp_servers/skills/apps/plugins）
	UninstallCleanup(ctx context.Context, id, runtimeID, extType string) error
	// DeleteInstancesByPluginID 按 plugin_id 删除所有关联实例
	DeleteInstancesByPluginID(ctx context.Context, pluginID string) error
	// DeleteCatalogEntry 删除目录条目（非 builtin）
	DeleteCatalogEntry(ctx context.Context, id string) error
	// IsCatalogBuiltin 检查目录条目是否为内置
	IsCatalogBuiltin(ctx context.Context, id string) (bool, error)
}

// AuditRepository 审计日志读写契约。
// @consumer: pkg/substrate/security/audit_trail.go
// @producer: pkg/substrate/storage/repo_audit.go
type AuditRepository interface {
	AppendAuditEvent(ctx context.Context, row types.AuditEventRow) error
	ListAuditEvents(ctx context.Context, limit int, before string) ([]types.AuditEventRow, error)
	DeleteAuditEventsBefore(ctx context.Context, before string) (int64, error)
}

// TaskReadRepository 任务表只读契约（写路径由 Blackboard 的 CAS 持有 *sql.DB）。
// @consumer: pkg/cognition/kernel/agent.go, pkg/edge/scheduler/cost_report.go
// @producer: pkg/substrate/storage/repo_task.go
type TaskReadRepository interface {
	GetTaskProviderSuspendCount(ctx context.Context, taskID string) (int, error)
	GetTaskIntentTaint(ctx context.Context, taskID string) (int, error)
	AggregateTokenCosts(ctx context.Context, startISO, endISO string) ([]types.TokenCostAgg, error)
}

// GraphTraverser consumer-side 接口：Tier1+ 图遍历路径（由 SurrealDBCoreStore 实现）。
// consumer-side 定义，防止包循环依赖。
//
// 两种遍历模式：
//   - GraphTraverse: BFS 有界宽度优先，适用于精确邻居枚举
//   - SpreadingActivation: 能量扩散遍历，多种子 + 边权重传播，适用于关联发现
//
// SpreadingActivation 是 HybridRetriever 图路径的首选算法（替代硬编码衰减的 BFS）。
type GraphTraverser interface {
	GraphTraverse(startID, edgeType string, maxDepth int) ([]string, error)
	GraphRelate(fromID, edgeType, toID string, weight float64) error
	// SpreadingActivation 多种子能量扩散图遍历。
	//   energyDecay:        每跳衰减系数（推荐 0.7）
	//   dormancyThreshold:  休眠阈值，energy ≤ 此值的节点停止扩散（推荐 0.05）
	//   fanOutLimit:        每节点最大邻居扩散数（防扇出爆炸，推荐 10）
	// nil SurrealDB 时实现方应返回空切片而非 error。
	SpreadingActivation(startIDs []string, maxDepth int, energyDecay, dormancyThreshold float64, fanOutLimit int) ([]types.ScoredNode, error)
}

// CognitiveSearchResult 认知检索结果（consumer-side 类型，避免引入 substrate/storage 依赖）。

// CognitiveSearcher consumer-side 接口：SurrealDB FTS + HNSW 向量检索与索引写入（Tier1+）。
// consumer-side 定义于 memory 包，防止与 substrate/storage 循环依赖。
// nil 时自动降级 Tier0 路径（纯 Go BM25 + SQLite BLOB 内存余弦）。
type CognitiveSearcher interface {
	FTSIndex(docID, text string) error
	FTSDelete(docID string) error
	VecUpsert(id string, embedding []float32) error
	VecDelete(id string) error
	VecKNN(query []float32, k int) ([]types.CognitiveSearchResult, error)
	FTSSearch(query string, k int) ([]types.CognitiveSearchResult, error)
	GraphRelate(fromID, edgeType, toID string, weight float64) error
}

// AgentInvoker 用于触发 Agent 会话。
type AgentInvoker interface {
	InvokeAgent(ctx context.Context, intent string, opts ...any) (string, error)
}

// Reranker 用于对检索结果进行重排序。
type Reranker interface {
	Rerank(ctx context.Context, query string, docs []types.CognitiveSearchResult) ([]types.CognitiveSearchResult, error)
}
