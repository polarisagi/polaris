package knowledge

import (
	"context"

	"github.com/polarisagi/polaris/internal/knowledge/graphrag"
)

// DocumentRef refers to a document from a connector.
type DocumentRef struct {
	URI         string `json:"uri"`
	Title       string `json:"title"`
	SourceType  string `json:"source_type"` // markdown|pdf|code|web|notion_page|gdoc
	ContentHash string `json:"content_hash"`
	UpdatedAt   int64  `json:"updated_at"`
}

// Document is the fetched content of a DocumentRef.
type Document struct {
	Ref      DocumentRef
	Raw      []byte
	Metadata map[string]string
}

// ChangeEvent represents a watched document change.
type ChangeEvent struct {
	Type string // "add", "update", "delete"
	Ref  DocumentRef
}

// SyncConfig defines connector sync behaviors.
type SyncConfig struct {
	DefaultInterval int
	SupportsWatch   bool
	MaxBatchSize    int
}

// Connector is the multi-source adapter.
// 架构文档: docs/arch/M10-Knowledge-RAG.md §1.2
type Connector interface {
	ID() string
	Name() string
	List(ctx context.Context) ([]*DocumentRef, error)
	Fetch(ctx context.Context, ref *DocumentRef) (*Document, error)
	Watch(ctx context.Context) (<-chan ChangeEvent, error)
	SyncConfig() SyncConfig
}

// DocTree models a document as a hierarchical structure.
type DocTree struct {
	Document   *DocNode `json:"document"`
	SourceURL  string   `json:"source_url,omitempty"`
	SourcePath string   `json:"source_path,omitempty"`
}

type DocNode struct {
	ID       string     `json:"id"`
	Title    string     `json:"title"`
	Level    int        `json:"level"`
	Summary  string     `json:"summary,omitempty"`
	Content  string     `json:"content,omitempty"`
	Children []*DocNode `json:"children,omitempty"`
}

// TaintLevel values for chunks and entities. Must match protocol/policy TaintLevel.
// 0=none, 1=low, 2=medium, 3=high, 4=critical.
const (
	TaintNone     = 0
	TaintLow      = 1
	TaintMedium   = 2
	TaintHigh     = 3
	TaintCritical = 4
)

// Chunk is aliased to graphrag.Chunk to prevent import cycles.
type Chunk = graphrag.Chunk

// Entity is a node in the knowledge graph extracted from documents.
// Must carry TaintLevel to prevent Taint Washing via entity extraction.
type Entity struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Type        string         `json:"type"`
	Properties  map[string]any `json:"properties,omitempty"`
	Embedding   []float64      `json:"embedding,omitempty"`
	SourceChunk string         `json:"source_chunk_id"`
	TaintLevel  int            `json:"taint_level"`
}

// Relation is an edge between two entities in the knowledge graph.
type Relation struct {
	ID         string         `json:"id"`
	Subject    string         `json:"subject_id"`
	Predicate  string         `json:"predicate"`
	Object     string         `json:"object_id"`
	Properties map[string]any `json:"properties,omitempty"`
	TaintLevel int            `json:"taint_level"`
}

type SearchQuery struct {
	Text      string    `json:"text"`
	Embedding []float64 `json:"embedding,omitempty"`
	TopK      int       `json:"top_k"`
	Strategy  string    `json:"strategy"` // "hybrid" | "graph" | "fts" | "vector"
	DocScope  string    `json:"doc_scope"`
}

// IngestionPipeline processes documents into the knowledge index.
type IngestionPipeline interface {
	Ingest(ctx context.Context, doc *Document, initialTaint int) (*DocTree, error)
	Delete(ctx context.Context, uri string) error
}

// GraphRAG provides entity-relationship graph + community detection for global queries.
type GraphRAG interface {
	BuildGraph(ctx context.Context, docID string) error
	DetectCommunities(ctx context.Context) error
	GlobalSearch(ctx context.Context, query string) ([]Chunk, error)
	LocalSearch(ctx context.Context, query string, entityID string) ([]Chunk, error)
}

// AugmentedContext 是 ContextExpander 的输出，包含叶块及其上下文。
type AugmentedContext struct {
	Primary     Chunk  // 命中的 LeafChunk
	Parent      *Chunk // 父块（ParentChunk），nil 表示已是顶层
	PrevSibling *Chunk // 前一个兄弟 LeafChunk，nil 表示无
	NextSibling *Chunk // 后一个兄弟 LeafChunk，nil 表示无
}

// SubQuery 是 QueryPlanner 分解出的子查询。
type SubQuery struct {
	Text        string  // 子查询文本
	TargetScope string  // 检索范围（docID 或 ""=全局）
	Weight      float64 // 合并时的权重（0~1，归一化后使用）
}

// KnowledgeBaseSearchRequest 是 KnowledgeBase.Search 的统一入口参数。
type KnowledgeBaseSearchRequest struct {
	Query    string
	TopK     int
	DocScope string // 限定检索范围（docID），"" 表示全局
	TaintMax int    // 最高允许的 TaintLevel
}
