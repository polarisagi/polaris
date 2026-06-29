package memory

import "context"

// 本文件声明 memory 包对外部模块的消费端接口（Consumer-side Interfaces）。
// 设计原则（参照 polaris-agent internal/memory/provider.go）：
//   - memory 包只引用此处声明的接口，禁止直接 import store/vfs/llm 等具体包
//   - 外部包实现这些接口并在构造时注入
//   - 接口名以 "Provider" 或 "Broker" 结尾，语义清晰

// VFSProvider memory 包对 VFS 的消费端接口（Blob 读写）。
// 由 vfs.WorkspaceManager 实现，在 vfs/provider.go 中的 BlobStore 接口与此等价。
// 在 memory 包内单独定义，防止循环 import vfs 包。
type VFSProvider interface {
	WriteBlob(data []byte) (ref string, err error)
	ReadBlob(ctx context.Context, ref string) ([]byte, error)
}

// EmbedProvider memory 包对向量化引擎的消费端接口。
// 由 llm.DynamicEmbedder 实现，供 semantic/episodic 存储层使用。
type EmbedProvider interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	Dim() int
}

// CognitiveStoreProvider memory 包对 SurrealDB 认知存储的消费端接口（Tier1+ 专用）。
// nil 时自动降级 Tier0 路径（纯 SQLite + BM25）。
type CognitiveStoreProvider interface {
	FTSIndex(docID, text string) error
	VecUpsert(id string, embedding []float32) error
	VecKNN(query []float32, k int) ([]KNNResult, error)
	FTSSearch(query string, k int) ([]KNNResult, error)
	GraphRelate(fromID, edgeType, toID string, weight float64) error
}

// KNNResult 向量/全文检索结果。
type KNNResult struct {
	ID    string
	Score float64
}

// LLMSummarizer memory 包对 LLM 摘要能力的消费端接口（L1→L2 巩固时使用）。
type LLMSummarizer interface {
	Summarize(ctx context.Context, text string, maxTokens int) (string, error)
}
