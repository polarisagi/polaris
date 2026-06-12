package memory

import (
	"context"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
)

// Layer classifies memory into four levels per the 2026 consensus.
type Layer string

const (
	LayerWorking    Layer = "working"
	LayerEpisodic   Layer = "episodic"
	LayerSemantic   Layer = "semantic"
	LayerProcedural Layer = "procedural"
)

// TaintLevel values for memory entries. Must match policy.TaintLevel and protocol.TaintLevel.
const (
	TaintNone     = 0
	TaintLow      = 1
	TaintMedium   = 2
	TaintHigh     = 3
	TaintCritical = 4
)

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
	Mem() protocol.Memory // 返回四层 facade
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

type ImmutableCore struct {
	AgentName            string            `json:"agent_name"`
	AgentRole            string            `json:"agent_role"`
	ModelID              string            `json:"model_id"`
	BuiltinTools         string            `json:"builtin_tools"`
	InstalledPlugins     string            `json:"installed_plugins"`
	UserPreferences      map[string]string `json:"user_preferences"`
	GlobalGoal           string            `json:"global_goal"`
	SystemPromptTemplate string            `json:"system_prompt_template"`

	// 三层系统提示词组装字段（stable + volatile）

	// SoulMDContent 用户自定义身份文件内容（~/.polarisagi/polaris/config/SOUL.md）。
	// 非空时替换 DefaultPolarisIdentity 作为 stable 层首段。
	SoulMDContent string `json:"soul_md_content,omitempty"`

	// ModelGuidance 模型专属工具调用引导，由 M13 Interface 层按 ModelID 注入到 stable 层。
	ModelGuidance string `json:"model_guidance,omitempty"`

	// PlatformHint 平台感知提示词，由 M13 Interface 层按接入平台注入到 stable 层末尾。
	// 取值来自 memory.PlatformHints 映射（cli/webui/api/cron）。
	PlatformHint string `json:"platform_hint,omitempty"`

	// VolatileBlock 易变信息区（时间戳/会话 ID/模型信息），每轮刷新。
	// 精确到天而非分钟，确保同一天内 prefix cache 不失效。
	VolatileBlock string `json:"volatile_block,omitempty"`

	// CustomInstructions 用户追加的行为指令（stable 层末尾，追加而非覆盖身份）。
	// 来源：~/.polarisagi/polaris/config/prompts/custom_instructions.md 或 Web UI 编辑。
	// DB 删除不影响（文件持久化），factory reset 时才清空。
	CustomInstructions string `json:"custom_instructions,omitempty"`
}

func NewImmutableCore() *ImmutableCore {
	return &ImmutableCore{
		AgentName:       "Polaris", // default name
		AgentRole:       "一个开源自托管 AI Agent",
		UserPreferences: make(map[string]string),
	}
}

// ============================================================================
// MemImpl — protocol.Memory 的四层具体实现
// ============================================================================

type MemImpl struct {
	working    *WorkingMem
	episodic   *EpisodicMem
	semantic   *SemanticMem
	procedural *ProceduralMem
	retriever  *HybridRetrieverImpl
	reflection protocol.ReflectionMemory // KV 实现或 SQL 实现，由构造器决定
}

func NewMemImpl(store protocol.Store) *MemImpl {
	procedural := &ProceduralMem{skills: nil}
	return &MemImpl{
		working:    NewWorkingMem(),
		episodic:   NewEpisodicMem(store),
		semantic:   NewSemanticMem(store, nil),
		procedural: procedural,
		retriever:  NewHybridRetriever(store),
		reflection: NewReflectionMem(store),
	}
}

// NewMemImplWithGraph 创建含 SurrealDB 图遍历路径的 MemImpl（Tier1+）。
// graph 注入后：episodic 事件写入时自动建立图谱边；检索时激活 BM25+Simhash+Graph 三路融合。
func NewMemImplWithGraph(store protocol.Store, graph GraphTraverser) *MemImpl {
	indexer := NewEpisodicGraphIndexer(graph)
	procedural := &ProceduralMem{skills: nil}
	return &MemImpl{
		working:    NewWorkingMem(),
		episodic:   NewEpisodicMemWithGraph(store, indexer),
		semantic:   NewSemanticMem(store, nil),
		procedural: procedural,
		retriever:  NewHybridRetrieverWithGraph(store, graph),
		reflection: NewReflectionMem(store),
	}
}

// NewMemImplFull 创建全功能 MemImpl（SurrealDB FTS+HNSW + Graph + SQL 全路径）。
// 适用于 Tier1+（SurrealDB 可用时由 main.go 调用）。
// cognitive 注入后：episodic 写入同步 FTS 索引，检索走 SurrealDB BM25+HNSW，图遍历启用。
// db 同时驱动 SQLReflectionMem、SQLNotesStore、HybridRetrieverWithCognitive 全路径。
func NewMemImplFull(store protocol.Store, graph GraphTraverser, cognitive CognitiveSearcher, db DBAccessor) *MemImpl {
	indexer := NewEpisodicGraphIndexer(graph)
	procedural := &ProceduralMem{skills: nil}
	m := &MemImpl{
		working:    NewWorkingMem(),
		episodic:   NewEpisodicMemWithCognitive(store, indexer, cognitive),
		semantic:   NewSemanticMem(store, nil),
		procedural: procedural,
	}
	if db != nil {
		sqlDB := db.DB()
		if sqlDB != nil {
			sqlRefl := NewSQLReflectionMem(sqlDB)
			m.reflection = sqlRefl
			m.working = NewWorkingMemWithDB(sqlDB)
			m.retriever = NewHybridRetrieverWithCognitive(store, graph, nil, sqlRefl, cognitive)
			return m
		}
	}
	m.reflection = NewReflectionMem(store)
	m.retriever = NewHybridRetrieverWithCognitive(store, graph, nil, nil, cognitive)
	return m
}

// NewMemImplWithDB 创建含全 SQL 持久化的 MemImpl。
// db 非 nil 时同时切换：
//   - reflection → SQLReflectionMem（reflection_memory 表，索引加速跨会话查询）
//   - working.notes → SQLNotesStore（notes 表，跨 Session 笔记持久化）
//   - retriever → NewHybridRetrieverFull（第 4 路走 SQL 接口而非 KV 前缀扫描）
func NewMemImplWithDB(store protocol.Store, db DBAccessor) *MemImpl {
	m := NewMemImpl(store)
	if db != nil {
		sqlDB := db.DB()
		if sqlDB != nil {
			sqlRefl := NewSQLReflectionMem(sqlDB)
			m.reflection = sqlRefl
			m.working = NewWorkingMemWithDB(sqlDB)
			m.retriever = NewHybridRetrieverFull(store, nil, nil, sqlRefl)
		}
	}
	return m
}

func (m *MemImpl) Working() protocol.WorkingMemory       { return m.working }
func (m *MemImpl) Episodic() protocol.EpisodicMemory     { return m.episodic }
func (m *MemImpl) Semantic() protocol.SemanticMemory     { return m.semantic }
func (m *MemImpl) Procedural() protocol.ProceduralMemory { return m.procedural }
func (m *MemImpl) Retriever() protocol.HybridRetriever   { return m.retriever }
func (m *MemImpl) Reflection() protocol.ReflectionMemory { return m.reflection }

func (m *MemImpl) StoreStats() (string, error) {
	return m.semantic.StoreStats()
}

func (m *MemImpl) SetVectorMode(mode int) error {
	return m.semantic.SetVectorMode(mode)
}

func (m *MemImpl) InjectSkillRegistry(sr protocol.SkillRegistry) {
	m.procedural.skills = sr
}

// 编译期接口合规验证
var (
	_ protocol.Memory           = (*MemImpl)(nil)
	_ protocol.WorkingMemory    = (*WorkingMem)(nil)
	_ protocol.EpisodicMemory   = (*EpisodicMem)(nil)
	_ protocol.SemanticMemory   = (*SemanticMem)(nil)
	_ protocol.ProceduralMemory = (*ProceduralMem)(nil)
	_ protocol.HybridRetriever  = (*HybridRetrieverImpl)(nil)
	_ protocol.ReflectionMemory = (*ReflectionMem)(nil)
)
