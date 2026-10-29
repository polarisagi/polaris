package knowledge

import "context"

// ScoredID 向量检索结果（ID + 相关性分数）。
type ScoredID struct {
	ID    string
	Score float64
}

// 本文件声明 knowledge 包对外部模块的消费端接口（Consumer-side Interfaces）。
//
// knowledge 包（RAG + 知识图谱）需要以下外部能力：
//   1. VectorStore  — 向量存储（Upsert/KNN）
//   2. EmbedEngine  — 文本向量化
//   3. LLMSummarizer — 社区摘要生成（GraphRAG 阶段）
//   4. ChunkRepo    — Chunk 持久化存储
//
// @consumer: knowledge/rag_impl.go, knowledge/graphrag/, knowledge/connector/
// @producer: 各具体模块由 cli.go/bootstrap 注入

// VectorStore knowledge 包对向量存储的消费端接口。
// 实现：store/search.VectorIndex 或 SurrealDB VecSearch（Tier1+）
type VectorStore interface {
	// Upsert 插入或更新一个向量（id 为 Chunk.ID）。
	Upsert(id string, vec []float32) error
	// KNN 按向量检索最近 k 个邻居，返回 (id, score) 对。
	KNN(query []float32, k int) ([]ScoredID, error)
	// Delete 删除指定 id 的向量。
	Delete(id string) error
}

// EmbedEngine knowledge 包对向量化引擎的消费端接口。
// 实现：llm.DynamicEmbedder（通过 DependencyMap["EmbedEngine"] 注入）
type EmbedEngine interface {
	// Embed 将文本转换为浮点向量（维度由 Dim() 返回）。
	Embed(ctx context.Context, text string) ([]float32, error)
	// Dim 返回向量维度（用于存储层预分配）。
	Dim() int
}

// ChunkRepo knowledge 包对 Chunk 持久化存储的消费端接口。
// 实现：store/repo.SQLiteRAGChunkRepo
type ChunkRepo interface {
	// Save 保存 Chunk（source + content + embedding_ref）。
	Save(ctx context.Context, chunk *Chunk) error
	// ListBySource 查询指定来源的所有 Chunk（用于删除时清理）。
	ListBySource(ctx context.Context, source string) ([]*Chunk, error)
	// FTSSearch 全文搜索，返回最相关的 k 个 Chunk。
	FTSSearch(ctx context.Context, query string, k int) ([]*Chunk, error)
}

// GraphWriter knowledge 包对知识图谱写入的消费端接口（GraphRAG 阶段）。
// 实现：SurrealDB 图存储（Tier1+）；nil 时跳过图谱构建。
type GraphWriter interface {
	// Relate 在知识图谱中建立实体关系（"entity_a" --[relation]--> "entity_b"）。
	Relate(fromID, relation, toID string, weight float64) error
}
