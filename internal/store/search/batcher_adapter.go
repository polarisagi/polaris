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

// AsyncBatcherAdapter implements the knowledge.VectorEmbedder interface.
// Note: We create a separate type to avoid method name collision on "Embed".
type AsyncBatcherAdapter struct {
	batcher *EmbeddingBatcher
}

func NewAsyncBatcherAdapter(batcher *EmbeddingBatcher) *AsyncBatcherAdapter {
	return &AsyncBatcherAdapter{batcher: batcher}
}

// Embed implements knowledge.VectorEmbedder.
func (a *AsyncBatcherAdapter) Embed(ctx context.Context, text string) ([]float32, error) {
	if a.batcher == nil {
		return nil, nil
	}

	// Priority 1 for single queries (High)
	res, err := a.batcher.Embed(ctx, []string{text}, "", 1)
	if err != nil {
		return nil, err
	}

	if len(res) > 0 {
		return res[0].Vector, nil
	}
	return nil, nil
}
