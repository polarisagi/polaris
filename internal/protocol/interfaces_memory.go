package protocol

import (
	"context"
	"time"

	"github.com/polarisagi/polaris/internal/observability/budget"
	"github.com/polarisagi/polaris/pkg/types"
)

type

// MemorySystem 是四层记忆的具体子系统集合。保留作为协议契约供内部使用。
MemorySystem interface {
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

type

// MemoryFacade 记忆系统对外统一门面，屏蔽底层架构，供 Agent / Server 侧直接调用。
MemoryFacade interface {
	// 基础控制
	StoreStats() (string, error)
	GetMemoryPressure() budget.ResourceBudget

	// Semantic 层调用
	SearchEntities(ctx context.Context, query string, topK int, maxTaint int) ([]types.Entity, error)
	GetUserProfile(ctx context.Context, userID string) (*types.UserProfile, error)

	// Episodic 层调用
	ListEpisodicEvents(ctx context.Context, query types.EpisodicQuery) ([]types.ScoredEvent, error)
	AppendEpisodicEvent(ctx context.Context, event types.Event, taintLevel types.TaintLevel) error
	ArchiveEpisodic(ctx context.Context, sessionID string) error

	// Working 层调用
	AddWorkingContext(ctx context.Context, text string) error
	SetWorkingScratch(key string, val []byte)
	ImmutableCore() ImmutableCore // 返回 *store.ImmutableCore 或其他不可变核心
	// ListCoreMemory 读取核心工作记忆块（UP-03）：ZoneCoreMemory 注入的唯一数据源，
	// 由 agentctx 在 Perceive/Plan 组装时调用。底层 CoreMemory 未配置时返回 (nil, nil)。
	ListCoreMemory(ctx context.Context, agentID, sessionID string) ([]types.CoreMemoryBlock, error)

	// Reflection 层调用
	ListReflections(ctx context.Context, q types.ReflectionQuery) ([]types.ReflectionEntry, error)
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

type

// ReflectionMemory 元认知反思层（Mem-L1.5，插于 Episodic 与 Semantic 之间）。
// 存储失败原因、策略切换决策、元认知观察。
// 区别于 PersonaRefiner（PersonaRefiner 调整偏好，ReflectionMemory 记录元决策）。
// @consumer: M4(Agent Kernel - 每轮反思写入), M9(Self-Improve - 反思数据驱动蒸馏)
// @producer: pkg/cognition/memory/ (ReflectionMem 实现)
// @arch: docs/arch/M05-Memory-System.md §3.4
ReflectionMemory interface {
	AppendReflection(ctx context.Context, entry types.ReflectionEntry) error
	ListReflections(ctx context.Context, q types.ReflectionQuery) ([]types.ReflectionEntry, error)
}

type

// WorkingMemory (Mem-L0) — 进程内，非持久化（Context + Scratch）+ 跨会话持久化（Notes）。
// Notes() 可由 SQLNotesStore 实现跨会话持久化，其余字段仍为进程内状态。
WorkingMemory interface {
	Immutable() ImmutableCore
	CoreMemory() CoreMemory
	Context() ContextWindow
	Scratch() ScratchPad
	Notes() NotesStore // M05 §2.2 跨会话轻量笔记，SQL 持久化
}

type

// NotesStore 跨会话轻量笔记存储（M05 §2.2）。
// DDL 权威源：internal/protocol/schema/023_notes.sql
// @consumer: M4(S_PERCEIVE 注入), M13(API read/write)
// @producer: pkg/cognition/memory/ (SQLNotesStore / InMemNotesStore)
NotesStore interface {
	Get(ctx context.Context, key string) (*types.Note, error)
	Set(ctx context.Context, key, content string, tags []string, expectedVersion int) error
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, tag string) ([]types.Note, error)
	ListByTask(ctx context.Context, taskID string) ([]types.Note, error)
	GC(ctx context.Context) (int, error) // 清理已过期 types.Note，返回删除条数
}

type

// CoreMemory 核心工作记忆区（ZoneCoreMemory），M05 §3.1。
// LLM 可通过工具显式编辑的内容，具有持久化能力和单块上限。
CoreMemory interface {
	Get(ctx context.Context, agentID, sessionID, blockKey string) (*types.CoreMemoryBlock, error)
	Set(ctx context.Context, agentID, sessionID, blockKey, content string, taintLevel types.TaintLevel) error
	Delete(ctx context.Context, agentID, sessionID, blockKey string) error
	List(ctx context.Context, agentID, sessionID string) ([]types.CoreMemoryBlock, error)
}

type

// ImmutableCore — 永不裁剪的核心区，写入经 M9 staging + M11 闸控。
ImmutableCore interface {
	Load(ctx context.Context, userID string, sessionID string) (types.ImmutableCoreView, error)
	// Fields 返回可写字段集合（ImmutableCoreFields）指针，供 gateway 等消费方组装系统提示词。
	// 取代此前 `.(*store.ImmutableCore)` 类型断言（M04 §B2）。
	Fields() *ImmutableCoreFields
	PrependToMessages(msgs []types.Message) []types.Message
}

type

// ContextWindow — 上下文窗口管理。ImmutableCore 不参与压缩。
ContextWindow interface {
	Append(msg types.Message)
	Compress(ctx context.Context, targetTokens int) error
	Tokens() int
	Messages() []types.Message
}

type

// ScratchPad — 任务级临时键值存储。
ScratchPad interface {
	Set(key string, value any)
	Get(key string) (any, bool)
	Clear()
}

type

// EpisodicMemory (Mem-L1) — 事件表 + 向量投影。
EpisodicMemory interface {
	Append(ctx context.Context, ev types.Event, taint types.TaintLevel) error
	Query(ctx context.Context, q types.EpisodicQuery) ([]types.ScoredEvent, error)
	MarkCold(ctx context.Context, sessionID string, before time.Time) (int, error)
	// ScanHighSalience 按显著性阈值 + 高水位标记扫描物化表 episodic_events。
	// 供后台维护 Agent（如 swarm.MemoryAgent）生成耳语提示，替代绕过本接口的裸 SQL。
	ScanHighSalience(ctx context.Context, sinceID int64, minSalience float64, limit int) ([]types.SalienceEvent, error)
}

type

// SemanticMemory (Mem-L2) — 文档/实体/关系图。
SemanticMemory interface {
	StoreDocument(ctx context.Context, doc types.Document, taint types.TaintLevel) error
	StoreChunks(ctx context.Context, docID string, chunks []types.Chunk, taint types.TaintLevel) error
	GetDocument(ctx context.Context, id string) (*types.Document, error)
	Archive(ctx context.Context, id string, reason string) error
	UpsertFact(ctx context.Context, entity types.Entity, taint types.TaintLevel) error
	UpsertRelation(ctx context.Context, rel types.Relation, taint types.TaintLevel) error
	GetEntity(ctx context.Context, entityType, name string) (*types.Entity, error)

	// 生命周期接口 — 信念修正与知识演化（缺口 1）
	ListActiveEntities(ctx context.Context, entityType string, limit int, asOf int64) ([]types.Entity, error)
	SearchEntities(ctx context.Context, query string, limit int, asOf int64) ([]types.Entity, error)
	MarkEntitySuperseded(ctx context.Context, oldDBID int64, newDBID int64) error

	// 用户画像接口 — L3 Persona 合成与查询（缺口 3）
	UpsertUserProfile(ctx context.Context, profile types.UserProfile) error
	GetUserProfile(ctx context.Context, profileKey string) (*types.UserProfile, error)
	StoreStats() (string, error)
	SetVectorMode(mode int) error
}

type

// ProceduralMemory (Mem-L3) — 技能索引，委托 M6 SkillRegistry。
ProceduralMemory interface {
	Skills() SkillRegistry
}

type

// GraphTraverser consumer-side 接口：Tier1+ 图遍历路径（由 SurrealDBCoreStore 实现）。
// consumer-side 定义，防止包循环依赖。
//
// 两种遍历模式：
//   - GraphTraverse: BFS 有界宽度优先，适用于精确邻居枚举
//   - SpreadingActivation: 能量扩散遍历，多种子 + 边权重传播，适用于关联发现
//
// SpreadingActivation 是 HybridRetriever 图路径的首选算法（替代硬编码衰减的 BFS）。
GraphTraverser interface {
	GraphTraverse(startID, edgeType string, maxDepth int) ([]string, error)
	GraphRelate(fromID, edgeType, toID string, weight float64) error
	// SpreadingActivation 多种子能量扩散图遍历。
	//   energyDecay:        每跳衰减系数（推荐 0.7）
	//   dormancyThreshold:  休眠阈值，energy ≤ 此值的节点停止扩散（推荐 0.05）
	//   fanOutLimit:        每节点最大邻居扩散数（防扇出爆炸，推荐 10）
	// nil SurrealDB 时实现方应返回空切片而非 error。
	SpreadingActivation(startIDs []string, maxDepth int, energyDecay, dormancyThreshold float64, fanOutLimit int) ([]types.ScoredNode, error)
}

type

// CognitiveSearcher consumer-side 接口：SurrealDB FTS + HNSW 向量检索与索引写入（Tier1+）。
// consumer-side 定义于 memory 包，防止与 substrate/storage 循环依赖。
// nil 时自动降级 Tier0 路径（纯 Go BM25 + SQLite BLOB 内存余弦）。
CognitiveSearcher interface {
	FTSIndex(docID, text string) error
	FTSDelete(docID string) error
	VecUpsert(id string, embedding []float32) error
	VecDelete(id string) error
	VecKNN(query []float32, k int) ([]types.CognitiveSearchResult, error)
	FTSSearch(query string, k int) ([]types.CognitiveSearchResult, error)
	GraphRelate(fromID, edgeType, toID string, weight float64) error
}
