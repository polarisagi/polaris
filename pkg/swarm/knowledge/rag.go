package knowledge

import "context"

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

// Chunk is a retrievable unit: LeafChunk (256 tokens) for precision, ParentChunk (1024 tokens) for context.
type Chunk struct {
	ID            string    `json:"id"`
	Content       string    `json:"content"`
	Embedding     []float64 `json:"embedding,omitempty"`
	DocID         string    `json:"doc_id"`
	SectionPath   []string  `json:"section_path"`
	ParentChunkID string    `json:"parent_chunk_id,omitempty"`
	TaintLevel    int       `json:"taint_level"`
	TaintSource   string    `json:"taint_source,omitempty"`
	// inv_M10_03 lineage metadata — DDL NOT NULL，跨版本溯源必填
	SourceURI         string `json:"source_uri"`
	DocVersion        string `json:"doc_version"`
	ChunkSeq          int    `json:"chunk_seq"`
	ContentHash       string `json:"content_hash"`
	EmbedModelVersion string `json:"embed_model_version"`
}

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

// HybridRetriever runs BM25 + Dense + Graph Traversal, fused via Reciprocal Rank Fusion.
// 架构文档: docs/arch/M10-Knowledge-RAG.md §2.1
type HybridRetriever interface {
	Search(ctx context.Context, query *SearchQuery) ([]Chunk, error)
}

type SearchQuery struct {
	Text      string    `json:"text"`
	Embedding []float64 `json:"embedding,omitempty"`
	TopK      int       `json:"top_k"`
	Strategy  string    `json:"strategy"` // "hybrid" | "graph" | "fts" | "vector"
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
