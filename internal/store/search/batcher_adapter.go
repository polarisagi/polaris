package search

import (
	"context"
	"log/slog"
)

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

	// Priority 1 for sync single text (High)
	res, err := a.batcher.Embed(context.Background(), []string{text}, "", 1)
	if err != nil {
		slog.Warn("SyncBatcherAdapter: embed failed", "err", err)
		return nil
	}

	if len(res) > 0 {
		return res[0].Vector
	}
	return nil
}
