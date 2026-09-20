package search

import (
	"context"
	"log/slog"
	"time"
)

// syncEmbedTimeout 同步适配器单次等待上限：Embedder 接口无 ctx，
// 批处理队列拥堵或后端挂起时不得让调用方无限阻塞（GR-1.1-008）。
const syncEmbedTimeout = 30 * time.Second

// SyncBatcherAdapter implements the synchronous search.Embedder interface
// using the asynchronous EmbeddingBatcher.
type SyncBatcherAdapter struct {
	batcher *EmbeddingBatcher
}

func NewSyncBatcherAdapter(batcher *EmbeddingBatcher) *SyncBatcherAdapter {
	return &SyncBatcherAdapter{batcher: batcher}
}

// Embed implements search.Embedder.
func (a *SyncBatcherAdapter) Embed(text string) []float32 {
	if a.batcher == nil {
		return nil
	}

	// 同步调用方在等结果（交互式路径），走 High 队列。
	ctx, cancel := context.WithTimeout(context.Background(), syncEmbedTimeout)
	defer cancel()
	res, err := a.batcher.Embed(ctx, []string{text}, "", PriorityHigh)
	if err != nil {
		slog.Warn("SyncBatcherAdapter: embed failed", "err", err)
		return nil
	}

	if len(res) > 0 {
		return res[0].Vector
	}
	return nil
}
