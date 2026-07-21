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

// MemoryEntry is a unit of retrievable memory.
type MemoryEntry struct {
	ID         string    `json:"id"`
	Layer      Layer     `json:"layer"`
	Content    string    `json:"content"`
	Embedding  []float64 `json:"embedding,omitempty"`
	EmbedDim   int       `json:"embed_dim,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
	// TaintLevel 取值必须与 security.TaintLevel / types.TaintLevel 保持一致语义。
	TaintLevel        int            `json:"taint_level"`
	TaintSource       string         `json:"taint_source,omitempty"`
	Meta              map[string]any `json:"meta,omitempty"`
	EmbedModelVersion string         `json:"embed_model_version"` // inv_M5_03: 跨版本检索触发 OnlineReindexer
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

func NewMemImpl(store protocol.Store) *MemImpl {
	procedural := memstore.NewProceduralMem(nil)
	return &MemImpl{
		working:    memstore.NewWorkingMem(),
		episodic:   memstore.NewEpisodicMem(store),
		semantic:   memstore.NewSemanticMem(store, nil),
		procedural: procedural,
		retriever:  memretrieval.NewHybridRetriever(store),
		reflection: memstore.NewReflectionMem(store),
		taskCanvas: memgraph.NewTaskMermaidCanvas(),
	}
}

// 2026-07-14（ADR-0051）：NewMemImplWithGraph 删除——全仓零调用点。
// "只有 graph 没有 cognitive/db" 是幽灵 Tier 档位：graph 与 cognitive 同源自
// sb.SurrealStore，实际启动分级逻辑中二者总是同时具备或同时缺失，不存在只解锁
// graph 的中间状态。生产唯一使用 NewMemImpl（Tier0 基础）/NewMemImplWithDB
// （Tier0+SQL）/NewMemImplFull（Tier1+，graph+cognitive+db 同时注入）。

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
		taskCanvas: memgraph.NewTaskMermaidCanvas(),
	}
	if db != nil {
		sqlRefl := memstore.NewSQLReflectionMem(db)
		m.reflection = sqlRefl
		// NewWorkingMemWithBudget（≥8GB SurrealDB 全路径，32000 token 预算）：
		// 此前用 NewWorkingMemWithDB 不设 tokenBudget，AppendAndPage 的自动换页
		// 分支（超预算时压缩到 EpisodicMem）永远不触发，working memory 只能无界
		// 增长直到调用方自行处理，2026-07-14（ADR-0051 关联接线）激活真实的
		// 溢出保护。
		m.working = memstore.NewWorkingMemWithBudget(db, m.episodic, 32000)
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
		// NewWorkingMemWithBudget（<8GB 降级路径，8000 token 预算，见函数doc）：
		// 同上激活 working memory 溢出保护（ADR-0051 关联接线）。
		m.working = memstore.NewWorkingMemWithBudget(db, m.episodic, 8000)
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

// InjectDriftDetector 注入 M05 §12.3 漂移检测器（委托给内部 retriever），
// 激活 Search() 内的 anchor 采样。sampleRate<=0 或 dd==nil 时等效未启用。
// dd 参数类型为 memretrieval.DriftAnchorRecorder（消费方本地接口，HE-3）：
// L1（internal/memory）禁止 import L2（internal/learning），调用方以
// *surprise.DriftDetector 实例传入即可（其 RecordAnchor 方法签名与此精确匹配）。
func (m *MemImpl) InjectDriftDetector(dd memretrieval.DriftAnchorRecorder, sampleRate float64) {
	m.retriever.InjectDriftDetector(dd, sampleRate)
}

// InjectDriftRegistry 注入漂移降级状态注册表（委托给内部 retriever），
// 激活按 task_type 降级 VectorWeight 的判断。r 参数类型为
// memretrieval.DriftGate（消费方本地接口），调用方以
// *surprise.DriftDowngradeRegistry 实例传入即可。
func (m *MemImpl) InjectDriftRegistry(r memretrieval.DriftGate) {
	m.retriever.InjectDriftRegistry(r)
}

func (m *MemImpl) StoreStats() (string, error) {
	return m.semantic.StoreStats()
}

func (m *MemImpl) SetVectorMode(mode int) error {
	return m.semantic.SetVectorMode(mode)
}

func (m *MemImpl) GetMemoryPressure() *budget.ResourceBudget {
	var available int
	isConstrained := false
	if fg := probe.GlobalFeatureGate(); fg != nil {
		available = int(fg.GetAvailableMemoryMB())
		isConstrained = fg.HardwareTier() <= probe.Tier1
	} else {
		available = int(probe.ProbeAvailableMemoryMB())
	}
	return &budget.ResourceBudget{
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

// SetEpisodicBlobOverflowWriter 转发注入 episodic 层超限 Payload 落盘目标
// （通常为 *vfs.WorkspaceManager，实现 memstore.BlobOverflowWriter）。
// GR-5-001 补线：bootMemory 早于 bootTools 执行，构造 MemImpl 时 VFS 尚未就绪，
// 因此不在构造函数内完成注入，改由 bootTools 拿到 vfsWM 后经此方法原地补上
// （bootTools 本就以 *MemoryBundle 为入参，无需调整既有启动阶段执行顺序）。
func (m *MemImpl) SetEpisodicBlobOverflowWriter(w memstore.BlobOverflowWriter) {
	m.episodic.SetBlobOverflowWriter(w)
}

// TrackToolCall 记录一次工具调用开始（M05 §11.3 TaskMermaidCanvas），创建 pending 节点。
// taskCanvas 保证由构造器初始化，非 nil。
func (m *MemImpl) TrackToolCall(toolUseID, toolName string) {
	m.taskCanvas.TrackToolCall(toolUseID, toolName)
}

// TrackToolResult 将 pending 节点转为已完成节点（成功/失败），追加到画布并自动连边。
func (m *MemImpl) TrackToolResult(toolUseID string, success bool, summary string) {
	m.taskCanvas.TrackToolResult(toolUseID, success, summary)
}

// RenderTaskCanvas 生成当前任务的 Mermaid graph LR 文本，供 gateway 只读展示。
// 空画布（尚无工具调用）返回空字符串。
func (m *MemImpl) RenderTaskCanvas() string {
	return m.taskCanvas.Render()
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

	// taskCanvas 当前任务的工具调用符号化画布（M05 §11.3），跨 agent/gateway 共享单实例。
	taskCanvas *memgraph.TaskMermaidCanvas
}

const (
	LayerWorking    = "working"
	LayerEpisodic   = "episodic"
	LayerSemantic   = "semantic"
	LayerProcedural = "procedural"
)
