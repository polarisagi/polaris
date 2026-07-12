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

// LLMSummarizer memory 包对 LLM 摘要/结构化推理能力的消费端接口（L1→L2 巩固时使用）。
//
// 2026-07-11 复核扩展：新增 InferRaw，供实体抽取（Stage 1）、用户画像合成
// （Stage 3.5）等需要自定义 prompt 结构（而非固定的"总结这段文本"模板）的调用方
// 使用，取代此前 consolidation_extract.go / consolidation_profile.go 直接持有
// protocol.Provider 并调用 safecall.Infer 的做法——那样等于绕开了本接口存在的
// 意义（internal/memory/CLAUDE.md 明令 memory 包 [MUST NOT] 持有 LLM Provider
// 的具体实现引用）。Summarize 和 InferRaw 的区别仅在于 prompt 是否由实现方渲染：
// Summarize 内部套用固定模板，InferRaw 直接透传调用方已经渲染好的完整 prompt。
type LLMSummarizer interface {
	Summarize(ctx context.Context, text string, maxTokens int) (string, error)
	// InferRaw 对外发送一段已渲染完成的 prompt，返回 LLM 原始文本响应（不做后处理/截断）。
	InferRaw(ctx context.Context, prompt string, maxTokens int) (string, error)
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
	// CheckQuota 预占式检查配额，通过即代表已占用 pendingWrite 份额；超限返回
	// vfs.ErrWorkspaceQuotaExhausted（未占用）。通过后若最终未 RegisterFile，
	// 调用方必须调用 ReleaseQuota 归还，否则配额永久泄漏（D-B6-01）。
	CheckQuota(pendingWrite int64) error
	// ReleaseQuota 归还 CheckQuota 预占但未通过 RegisterFile 登记的配额份额。
	ReleaseQuota(n int64)
	// WriteFile 将 data 写入相对路径 relPath（基于 RootDir），自动创建父目录。
	WriteFile(relPath string, data []byte) error
	// ReadFile 从相对路径 relPath 读取文件，最多读取 limit 字节。如果 limit <= 0，读取全部。
	ReadFile(relPath string, limit int64) ([]byte, error)
}
