package store

import (
	"context"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
)

// Layer classifies memory into four levels per the 2026 consensus.
type Layer string

// TaintLevel values for memory entries. Must match security.TaintLevel and types.TaintLevel.

// MemoryEntry is a unit of retrievable memory.
type MemoryEntry struct {
	ID                string         `json:"id"`
	Layer             Layer          `json:"layer"`
	Content           string         `json:"content"`
	Embedding         []float64      `json:"embedding,omitempty"`
	EmbedDim          int            `json:"embed_dim,omitempty"`
	OccurredAt        time.Time      `json:"occurred_at"`
	TaintLevel        int            `json:"taint_level"`
	TaintSource       string         `json:"taint_source,omitempty"`
	Meta              map[string]any `json:"meta,omitempty"`
	EmbedModelVersion string         `json:"embed_model_version"` // inv_M5_03: 跨版本检索触发 OnlineReindexer
}

// MemorySystem is the four-layer memory manager.
type MemorySystem interface {
	Write(ctx context.Context, entry *MemoryEntry) error
	Retrieve(ctx context.Context, query *RetrievalQuery) ([]MemoryEntry, error)
	Consolidate(ctx context.Context) error
	Forget(ctx context.Context) (int, error)
	Mem() protocol.MemorySystem // 返回四层 facade
}

// RetrievalQuery supports hybrid search across all layers.
type RetrievalQuery struct {
	Text      string    `json:"text"`
	Embedding []float64 `json:"embedding,omitempty"`
	EmbedDim  int       `json:"embed_dim,omitempty"`
	Layer     Layer     `json:"layer,omitempty"`
	TopK      int       `json:"top_k"`
	Strategy  string    `json:"strategy"` // "vector" | "fts" | "graph" | "hybrid"
	MaxTaint  int       `json:"max_taint,omitempty"`
}

// ============================================================================
// ImmutableCore — 写入经 M9 staging + M11 闸控
// ============================================================================

// NewMemImplWithGraph 创建含 SurrealDB 图遍历路径的 MemImpl（Tier1+）。
// graph 注入后：episodic 事件写入时自动建立图谱边；检索时激活 BM25+Simhash+Graph 三路融合。

// InjectRelevantMemory 提取与 query 相关的高价值实体与文档片段，组装为上下文供 LLM 注入。
