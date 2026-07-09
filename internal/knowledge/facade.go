package knowledge

import "context"

// KnowledgeFacade 知识库模块对外统一接口（RAG + 知识图谱）。
//
// 问题背景：
//
//	当前 knowledge 包对外暴露了 RAGEngine/KnowledgeBase/HybridRetriever 三个入口，
//	上层代码（agent、gateway）需要了解哪个入口适合哪种查询。
//
// 解决方案：
//   - KnowledgeFacade 是 knowledge 包对外的统一入口接口
//   - 上层模块依赖此接口，不直接操作 RAGEngine/HybridRetriever
//   - 内部自动路由：简单查询走 BM25+向量，复杂推理走图谱遍历
//
// @consumer: agent/agent.go（KnowledgeSearcher 接口），gateway/server/server.go
// @producer: knowledge.RAGEngine（由 cli.go/bootstrap 构造注入）
type KnowledgeFacade interface {
	// Search 混合检索（BM25 + 向量 + 图谱，按 query 复杂度自动路由）。
	// 返回最相关的 k 个 Chunk（含来源文档信息）。
	// Chunk 类型即包内 `type Chunk = graphrag.Chunk` 别名。
	Search(ctx context.Context, query string, k int) ([]*Chunk, error)

	// Ingest 将文档 URL/路径 摄入知识库（切分 + 向量化 + 图谱构建）。
	// 异步执行，返回 jobID 供状态查询。
	Ingest(ctx context.Context, source string) (jobID string, err error)

	// IngestStatus 查询摄入任务状态（pending/running/done/failed）。
	IngestStatus(ctx context.Context, jobID string) (string, error)

	// Delete 删除指定来源的所有 Chunk（知识库更新场景）。
	Delete(ctx context.Context, source string) error
}
