package memory

import (
	"context"

	"github.com/polarisagi/polaris/internal/vfs"
)

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

// WorkspaceProvider memory 包对任务隔离工作区目录管理的消费端接口（M05 §11.3 Stage 1）。
// 由 vfs.WorkspaceManager 实现；ToolRefOffloader 只持有本接口，不持有具体
// *vfs.WorkspaceManager struct（模块边界 R3：消费端只引用窄接口，不 import 具体
// stateful 实现）。方法签名中的 vfs.WorkspaceFile 是纯数据 DTO（零方法），
// 与 STTTranscriber.Transcribe 引用 stt.Result 是同一类允许的例外，不算破例。
type WorkspaceProvider interface {
	// Create 为 taskID 创建隔离工作区目录，幂等，返回绝对路径。
	Create(taskID string) (string, error)
	// GetRootDir 返回工作区根目录（用于拼接相对路径）。
	GetRootDir() string
	// RegisterFile 登记文件到 taskID 的 manifest，供 quota/GC 感知。
	RegisterFile(taskID string, f vfs.WorkspaceFile)
	// CheckQuota 写入前检查配额，超限返回 vfs.ErrWorkspaceQuotaExhausted。
	CheckQuota(pendingWrite int64) error
	// WriteFile 将 data 写入相对路径 relPath（基于 RootDir），自动创建父目录。
	WriteFile(relPath string, data []byte) error
	// ReadFile 从相对路径 relPath 读取文件，最多读取 limit 字节。如果 limit <= 0，读取全部。
	ReadFile(relPath string, limit int64) ([]byte, error)
}
