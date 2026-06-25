// Package plugin — EmbeddingIndexer 在市场同步后对 extension_catalog 条目进行
// FTS 索引（SurrealDB BM25）和向量 Upsert（SurrealDB HNSW），
// 激活 search_extension 工具的 FTSSearch + VecKNN 路径。
//
// 架构文档: docs/arch/M13-bis(Extension Registry) §3（预计算节点）
// 调用时机: insertMarketplaceEntries 成功写库后异步触发（不阻塞同步主流程）。
package plugin

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/polarisagi/polaris/internal/store/search"
)

// CognitiveIndexer 认知存储写接口（消费方定义，防包循环）。
// *store.SurrealDBCoreStore 通过 cmd/polaris/adapters_surreal.go 的适配器满足此接口。
type CognitiveIndexer interface {
	FTSIndex(docID, text string) error
	VecUpsert(id string, embedding []float32) error
}

// EmbeddingIndexer 市场条目向量预计算器。
// Embedder 为 nil 时仅做 FTS 索引（BM25 仍可用），跳过向量写入。
type EmbeddingIndexer struct {
	cognitive CognitiveIndexer
	embedder  search.Embedder // 可 nil
}

// NewEmbeddingIndexer 构造预计算器。cognitive 必须非 nil；embedder 可为 nil。
func NewEmbeddingIndexer(cognitive CognitiveIndexer, embedder search.Embedder) *EmbeddingIndexer {
	return &EmbeddingIndexer{cognitive: cognitive, embedder: embedder}
}

// IndexEntries 对给定条目批量做 FTS + VecUpsert。
// 异步调用：出错只记录日志，不影响调用方。
// docID 格式：`ext_{catalog_id}`（与 search_extension 工具的 ScoredResult.ID 前缀一致）。
func (idx *EmbeddingIndexer) IndexEntries(ctx context.Context, entries []CatalogEntry) {
	if idx.cognitive == nil {
		return
	}

	// 构造批量文本（name + description，与 search_extension 的检索语义一致）
	texts := make([]string, len(entries))
	for i, e := range entries {
		texts[i] = e.Name + " " + e.Description
	}

	// 批量 Embedding（减少 HTTP 往返）
	var vecs [][]float32
	if idx.embedder != nil {
		if be, ok := idx.embedder.(interface {
			EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)
		}); ok {
			var err error
			vecs, err = be.EmbedBatch(ctx, texts)
			if err != nil {
				slog.Warn("plugin: EmbedBatch failed, skipping vector upsert", "err", err)
				vecs = nil
			}
		} else {
			// 逐条降级（不支持批量的 Embedder）
			vecs = make([][]float32, len(entries))
			for i, t := range texts {
				vecs[i] = idx.embedder.Embed(t)
			}
		}
	}

	for i, e := range entries {
		docID := fmt.Sprintf("ext_%s", e.ID)

		// FTS 索引（BM25，总是写）
		if err := idx.cognitive.FTSIndex(docID, texts[i]); err != nil {
			slog.Warn("plugin: FTSIndex failed", "id", docID, "err", err)
		}

		// 向量 Upsert（需 Embedder 可用且向量非 nil）
		if vecs != nil && i < len(vecs) && vecs[i] != nil {
			if err := idx.cognitive.VecUpsert(docID, vecs[i]); err != nil {
				slog.Warn("plugin: VecUpsert failed", "id", docID, "err", err)
			}
		}
	}
	slog.Info("plugin: extension catalog indexed", "count", len(entries))
}

// CatalogEntry 预计算所需的最小条目字段。
type CatalogEntry struct {
	ID          string
	Name        string
	Description string
}
