package swarm

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
