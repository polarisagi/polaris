package types

import "time"

// ExplainBits 检索路径位掩码常量（唯一权威源）。internal/memory/retrieval 与
// internal/store/search 均引用这套常量，避免同一位定义在多个包内各自重复声明
// （Batch8 ExplainBits 归因修复：internal/knowledge 此前完全没有填充 ExplainBits，
// 修复时发现该位图之前只在 internal/memory/retrieval 包内私有定义，上提到这里
// 供跨层复用，同时 internal/store/search 处于比 internal/memory 更低的层次，
// 不能反向 import internal/memory/retrieval）。
const (
	BitBM25       uint8 = 1 << 0
	BitSimhash    uint8 = 1 << 1
	BitVector     uint8 = 1 << 2
	BitGraph      uint8 = 1 << 3
	BitReflection uint8 = 1 << 4
	BitDurative   uint8 = 1 << 5
	BitSemantic   uint8 = 1 << 6
)

type

// SearchScope 限定检索范围（memory 层或 document_tree 层）。
SearchScope struct {
	Type    string // "memory" | "document_tree"
	Subtree string // 限定检索子树（如 doc_node_id、memory_layer）
}

type

// RetrievalConfig BM25 + Vector + Graph 三路融合检索参数。
// M5 默认: FinalTopK=10, RerankTopM=30；M10 默认: FinalTopK=5, RerankTopM=50。
RetrievalConfig struct {
	BM25Weight   float64
	VectorWeight float64
	GraphWeight  float64
	RRFK         int
	OversampleN  int
	RerankTopM   int
	FinalTopK    int
	// AsOf 指定检索回溯时点（Unix 毫秒）。零值表示当前时间。
	// 底层将统一应用 `valid_from <= asOf AND (valid_until = 0 OR valid_until > asOf)` 过滤。
	AsOf int64
	// Tier0VectorScanLimit Tier0 SQLite 内存余弦扫描的最大行数（默认 500，上限建议 2000）。
	Tier0VectorScanLimit int
}

type

// ScoredFragment 单条混合检索结果（含证据类型和污点等级）。
ScoredFragment struct {
	Content      string
	Score        float64
	Source       string
	Metadata     map[string]string
	EvidenceType EvidenceType // 证据来源类型（零值=未标注，兼容旧路径）
	TaintLevel   TaintLevel   // 来源数据污点等级，注入 Prompt 时须遵循 PropagateTaint 规则
	// ExplainBits 检索路径位图（线上排障用，不进 Prompt）。
	// bit0=BM25 bit1=Simhash bit2=Vector bit3=Graph bit4=Reflection bit5=Durative bit6=Semantic
	ExplainBits uint8
}

type

// ScoredNode 图遍历结果：节点 ID + 激活能量（Spreading Activation）或跳数衰减分（BFS）。
// Score 由 SA 算法按边权重传播产生，物理意义明确，无需外部硬编码衰减系数。
ScoredNode struct {
	ID    string
	Score float64 // SA: energy；BFS: hop-decay score
}

type

// EpisodicQuery 情景记忆检索参数。
EpisodicQuery struct {
	SessionID     string
	Topics        []string
	Semantic      string // 语义搜索文本
	K             int
	MaxTaintLevel TaintLevel // 上限（含）；调用方必须显式设置
}

type

// ScoredEvent 情景记忆检索结果（带相关性分数）。
ScoredEvent struct {
	Score float64
	// Event 字段的类型为 interface{}，避免引入 internal/protocol/pb 依赖。
	// 实际使用时由 internal/protocol.Event 填充。
	Event any
}

type

// SalienceEvent 高显著性情景事件的轻量摘要（episodic_events 物化表投影）。
// 供后台维护 Agent（如 swarm.MemoryAgent）扫描生成耳语提示，避免调用方绕过
// MemoryFacade 直接执行裸 SQL（M04 §B2 跨模块通信通道）。
SalienceEvent struct {
	ID         int64
	SessionID  string
	Content    string
	Salience   float64
	OccurredAt int64
}

type

// ReflectionEntry 单条元认知反思记录（Mem-L1.5）。
ReflectionEntry struct {
	ID         string         `json:"id"`
	SessionID  string         `json:"session_id"`
	AgentID    string         `json:"agent_id,omitempty"`
	FailReason string         `json:"fail_reason,omitempty"` // 失败原因
	Strategy   string         `json:"strategy,omitempty"`    // 策略切换描述
	Decision   string         `json:"decision,omitempty"`    // 元决策内容
	Meta       map[string]any `json:"meta,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
}

type

// ReflectionQuery 反思记录查询参数。
ReflectionQuery struct {
	SessionID string
	AgentID   string
	TaskType  string // 跨会话按任务类型过滤
	Topic     string // 主题词过滤：匹配 Decision 或 Strategy 字段
	K         int    // 返回最近 K 条，0 = 不限
}

type

// CognitiveSearchResult SurrealDB FTS + HNSW 向量检索的单条结果。
CognitiveSearchResult struct {
	ID      string
	Score   float64
	Content string
}

type

// ReflectionModel is the output of the reflection phase (S_REFLECT→S_COMPLETE).
ReflectionModel struct {
	Success   bool     `json:"success"`
	Summary   string   `json:"summary"`
	Lessons   []string `json:"lessons,omitempty"`
	SkillName string   `json:"skill_name,omitempty"`
}

type

// Note 单条跨会话笔记。
Note struct {
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

type CoreMemoryBlock struct {
	AgentID    string
	SessionID  string
	BlockKey   string
	Content    string
	TaintLevel TaintLevel
	UpdatedAt  time.Time
}
