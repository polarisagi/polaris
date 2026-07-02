package memory

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"

	memgraph "github.com/polarisagi/polaris/internal/memory/graph"
	memretrieval "github.com/polarisagi/polaris/internal/memory/retrieval"
	memstore "github.com/polarisagi/polaris/internal/memory/store"

	"github.com/polarisagi/polaris/internal/observability/budget"
	"github.com/polarisagi/polaris/internal/observability/probe"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// Layer classifies memory into four levels per the 2026 consensus.
type Layer string

// TaintLevel values for memory entries. Must match security.TaintLevel and types.TaintLevel.

// MemoryEntry is a unit of retrievable memory.
type MemoryEntry struct {
	ID                string         `json:"id"`
	Layer             Layer          `json:"layer"`
	Content           string         `json:"content"`
	Embedding         []float64      `json:"embedding,omitempty"`
	EmbedDim          int            `json:"embed_dim,omitempty"`
	OccurredAt        time.Time      `json:"occurred_at"`
	TaintLevel        int            `json:"taint_level"`
	TaintSource       string         `json:"taint_source,omitempty"`
	Meta              map[string]any `json:"meta,omitempty"`
	EmbedModelVersion string         `json:"embed_model_version"` // inv_M5_03: 跨版本检索触发 OnlineReindexer
}

// MemorySystem is the four-layer memory manager.
type MemorySystem interface {
	Write(ctx context.Context, entry *MemoryEntry) error
	Retrieve(ctx context.Context, query *RetrievalQuery) ([]MemoryEntry, error)
	Consolidate(ctx context.Context) error
	Forget(ctx context.Context) (int, error)
	Mem() protocol.MemorySystem // 返回四层 facade
}

// RetrievalQuery supports hybrid search across all layers.
type RetrievalQuery struct {
	Text      string    `json:"text"`
	Embedding []float64 `json:"embedding,omitempty"`
	EmbedDim  int       `json:"embed_dim,omitempty"`
	Layer     Layer     `json:"layer,omitempty"`
	TopK      int       `json:"top_k"`
	Strategy  string    `json:"strategy"` // "vector" | "fts" | "graph" | "hybrid"
	MaxTaint  int       `json:"max_taint,omitempty"`
}

// ============================================================================
// ImmutableCore — 写入经 M9 staging + M11 闸控
// ============================================================================

func NewMemImpl(store protocol.Store) *MemImpl {
	procedural := memstore.NewProceduralMem(nil)
	return &MemImpl{
		working:    memstore.NewWorkingMem(),
		episodic:   memstore.NewEpisodicMem(store),
		semantic:   memstore.NewSemanticMem(store, nil),
		procedural: procedural,
		retriever:  memretrieval.NewHybridRetriever(store),
		reflection: memstore.NewReflectionMem(store),
	}
}

// NewMemImplWithGraph 创建含 SurrealDB 图遍历路径的 MemImpl（Tier1+）。
// graph 注入后：episodic 事件写入时自动建立图谱边；检索时激活 BM25+Simhash+Graph 三路融合。
func NewMemImplWithGraph(store protocol.Store, graph protocol.GraphTraverser) *MemImpl {
	indexer := memgraph.NewEpisodicGraphIndexer(graph)
	procedural := memstore.NewProceduralMem(nil)
	return &MemImpl{
		working:    memstore.NewWorkingMem(),
		episodic:   memstore.NewEpisodicMemWithGraph(store, indexer),
		semantic:   memstore.NewSemanticMem(store, nil),
		procedural: procedural,
		retriever:  memretrieval.NewHybridRetrieverWithGraph(store, graph),
		reflection: memstore.NewReflectionMem(store),
	}
}

// InjectRelevantMemory 提取与 query 相关的高价值实体与文档片段，组装为上下文供 LLM 注入。
func (m *MemImpl) InjectRelevantMemory(ctx context.Context, sessionID string, query string) (string, error) {
	if query == "" {
		return "", nil
	}
	cfg := types.RetrievalConfig{
		FinalTopK:    10,
		RerankTopM:   30,
		BM25Weight:   0.3,
		VectorWeight: 0.5,
		GraphWeight:  0.2,
	}
	frags, err := m.retriever.Search(ctx, query, types.SearchScope{Type: "memory"}, cfg)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "MemImpl.InjectRelevantMemory", err)
	}

	if len(frags) == 0 {
		return "", nil
	}

	var sb strings.Builder
	for _, f := range frags {
		fmt.Fprintf(&sb, "- %s\n", f.Content)
	}
	return sb.String(), nil
}

// NewMemImplFull 创建全功能 MemImpl（SurrealDB FTS+HNSW + Graph + SQL 全路径）。
// 适用于 Tier1+（SurrealDB 可用时由 main.go 调用）。
// cognitive 注入后：episodic 写入同步 FTS 索引，检索走 SurrealDB BM25+HNSW，图遍历启用。
// db 同时驱动 SQLReflectionMem、SQLNotesStore、HybridRetrieverWithCognitive 全路径。
func NewMemImplFull(store protocol.Store, graph protocol.GraphTraverser, cognitive protocol.CognitiveSearcher, db protocol.SQLQuerier) *MemImpl {
	indexer := memgraph.NewEpisodicGraphIndexer(graph)
	procedural := memstore.NewProceduralMem(nil)
	m := &MemImpl{
		working:    memstore.NewWorkingMem(),
		episodic:   memstore.NewEpisodicMemWithCognitive(store, indexer, cognitive),
		semantic:   memstore.NewSemanticMemWithCognitive(store, nil, cognitive),
		procedural: procedural,
	}
	if db != nil {
		sqlRefl := memstore.NewSQLReflectionMem(db)
		m.reflection = sqlRefl
		m.working = memstore.NewWorkingMemWithDB(db)
		m.retriever = memretrieval.NewHybridRetrieverWithCognitive(store, graph, memstore.NewDurativeMemoryManager(m.episodic, nil, store), sqlRefl, cognitive, m.semantic)
		return m
	}
	m.reflection = memstore.NewReflectionMem(store)
	m.retriever = memretrieval.NewHybridRetrieverWithCognitive(store, graph, memstore.NewDurativeMemoryManager(m.episodic, nil, store), nil, cognitive, m.semantic)
	return m
}

// NewMemImplWithDB 创建含全 SQL 持久化的 MemImpl。
// db 非 nil 时同时切换：
//   - reflection → SQLReflectionMem（reflection_memory 表，索引加速跨会话查询）
//   - working.notes → SQLNotesStore（notes 表，跨 Session 笔记持久化）
//   - retriever → memretrieval.NewHybridRetrieverFull（第 4 路走 SQL 接口而非 KV 前缀扫描）
func NewMemImplWithDB(store protocol.Store, db protocol.SQLQuerier) *MemImpl {
	m := NewMemImpl(store)
	if db != nil {
		sqlRefl := memstore.NewSQLReflectionMem(db)
		m.reflection = sqlRefl
		m.working = memstore.NewWorkingMemWithDB(db)
		m.retriever = memretrieval.NewHybridRetrieverFull(store, nil, memstore.NewDurativeMemoryManager(m.episodic, nil, store), sqlRefl)
	}
	return m
}

func (m *MemImpl) Working() protocol.WorkingMemory       { return m.working }
func (m *MemImpl) Episodic() protocol.EpisodicMemory     { return m.episodic }
func (m *MemImpl) Semantic() protocol.SemanticMemory     { return m.semantic }
func (m *MemImpl) Procedural() protocol.ProceduralMemory { return m.procedural }
func (m *MemImpl) Retriever() protocol.HybridRetriever   { return m.retriever }
func (m *MemImpl) Reflection() protocol.ReflectionMemory { return m.reflection }

// InjectEmbedder 激活向量检索路径（委托给内部 retriever，外部不感知具体类型）。
func (m *MemImpl) InjectEmbedder(e memretrieval.Embedder) {
	m.retriever.InjectEmbedder(e)
}

func (m *MemImpl) StoreStats() (string, error) {
	return m.semantic.StoreStats()
}

func (m *MemImpl) SetVectorMode(mode int) error {
	return m.semantic.SetVectorMode(mode)
}

func (m *MemImpl) GetMemoryPressure() budget.ResourceBudget {
	var available int
	isConstrained := false
	if fg := probe.GlobalFeatureGate(); fg != nil {
		available = int(fg.GetAvailableMemoryMB())
		isConstrained = fg.HardwareTier() <= probe.Tier1
	} else {
		available = int(probe.ProbeAvailableMemoryMB())
	}
	return budget.ResourceBudget{
		AvailableMB:   available,
		IsConstrained: isConstrained,
	}
}

// ConfigureWorkingMemBudget sets the token budget and episodic memory for WorkingMem paging.
func (m *MemImpl) ConfigureWorkingMemBudget(budget int) {
	m.working.SetTokenBudget(budget)
	m.working.SetEpisodic(m.episodic)
}

func (m *MemImpl) InjectSkillRegistry(sr protocol.SkillRegistry) {
	m.procedural.SetSkills(sr)
}

// 编译期接口合规验证
var (
	_ protocol.MemorySystem     = (*MemImpl)(nil)
	_ protocol.WorkingMemory    = (*memstore.WorkingMem)(nil)
	_ protocol.EpisodicMemory   = (*memstore.EpisodicMem)(nil)
	_ protocol.SemanticMemory   = (*memstore.SemanticMem)(nil)
	_ protocol.ProceduralMemory = (*memstore.ProceduralMem)(nil)
	_ protocol.HybridRetriever  = (*memretrieval.HybridRetrieverImpl)(nil)
	_ protocol.ReflectionMemory = (*memstore.ReflectionMem)(nil)
)

// 四层记忆类型定义。
// 架构文档: docs/arch/05-Memory-System-深度选型.md §1-5

// ImmutableCore 不可变核心区（永不裁剪）。

// ============================================================================
// MemImpl — protocol.MemorySystem 的四层具体实现
// ============================================================================

type MemImpl struct {
	working    *memstore.WorkingMem
	episodic   *memstore.EpisodicMem
	semantic   *memstore.SemanticMem
	procedural *memstore.ProceduralMem
	retriever  *memretrieval.HybridRetrieverImpl
	reflection protocol.ReflectionMemory // KV 实现或 SQL 实现，由构造器决定
}

const (
	LayerWorking    = "working"
	LayerEpisodic   = "episodic"
	LayerSemantic   = "semantic"
	LayerProcedural = "procedural"
)
