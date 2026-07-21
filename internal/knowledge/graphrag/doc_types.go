package graphrag

// 知识 RAG 类型定义。
// 架构文档: docs/arch/10-Knowledge-RAG-深度选型.md §1

// DocNode 文档树节点。
type DocNode struct {
	ID             string
	DocumentID     string
	NodeType       string // document | chapter | section | paragraph | table | code_block
	Level          int
	SectionPath    []string
	SeqIndex       int
	Content        string
	ContentHash    string
	TopicSentence  string
	SectionSummary string
	DocAbstract    string
	Embedding      []float32
	ParentID       string
	ChildrenIDs    []string
}

// LeafChunk 叶子分块（~256 tokens 精确检索）。
type LeafChunk struct {
	ID        string
	NodeID    string
	Content   string
	Embedding []float32
	StartChar int
	EndChar   int
}

// ParentChunk 父级分块（~1024 tokens 上下文合成）。
type ParentChunk struct {
	ID        string
	NodeID    string
	Content   string
	LeafIDs   []string
	Embedding []float32
}

// ChunkProvenance 来源追踪元数据。
type ChunkProvenance struct {
	SourceID       string
	SourceURI      string
	SourceType     string
	DocVersion     int
	AuthorityTier  int // 1=官方, 2=社区受信, 3=公共知识库, 4=用户上传
	IngestedAt     int64
	EmbeddingModel string
	ContentHash    string
}

// Connector 多源文档连接器接口。 (已迁移至 internal/protocol)

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
	// TaintHMAC 是跨边界密码学验证签名（inv_M11_02，见 009_rag_chunks.sql taint_hmac
	// 列注释），仅 knowledge 包内部读写路径使用，非本类型对外契约的一部分。
	TaintHMAC string `json:"-"`
	// inv_M10_03 lineage metadata — DDL NOT NULL，跨版本溯源必填
	SourceURI         string `json:"source_uri"`
	DocVersion        string `json:"doc_version"`
	ChunkSeq          int    `json:"chunk_seq"`
	ContentHash       string `json:"content_hash"`
	EmbedModelVersion string `json:"embed_model_version"`
	ChunkType         string `json:"chunk_type"`
	ChunkIndex        int    `json:"chunk_index"`
}
