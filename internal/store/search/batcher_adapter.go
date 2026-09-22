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
	return a.embedWithPriority(text, PriorityHigh, "SyncBatcherAdapter")
}

// BackgroundEmbedder 与 SyncBatcherAdapter 共用同一个 EmbeddingBatcher，
// 但把请求投进 **Low** 队列，供批量/周期性后台工作（重索引、知识全量同步、
// 插件向量回填）使用。
//
// [2026-09-22] EmbeddingBatcher 自带 High/Low 双队列、20% 反饥饿槽位与 Low
// 排队超 100ms 自动升级的完整调度，但全仓检索显示 **PriorityLow 从未被任何
// 调用方使用过**——所有嵌入请求，无论是用户一次检索还是 148 个扩展的批量回填，
// 都从 SyncBatcherAdapter 走 PriorityHigh 挤在同一条队列里。于是该调度机制
// 在生产中等同于不存在，实测表现为交互式 Knowledge.Search 连续 30 秒超时。
type BackgroundEmbedder struct {
	batcher *EmbeddingBatcher
}

// NewBackgroundEmbedder 基于同一个 batcher 构造低优先级嵌入器。
func NewBackgroundEmbedder(batcher *EmbeddingBatcher) *BackgroundEmbedder {
	return &BackgroundEmbedder{batcher: batcher}
}

// Embed implements search.Embedder（低优先级）。
func (a *BackgroundEmbedder) Embed(text string) []float32 {
	return (&SyncBatcherAdapter{batcher: a.batcher}).embedWithPriority(text, PriorityLow, "BackgroundEmbedder")
}

func (a *SyncBatcherAdapter) embedWithPriority(text string, priority int, who string) []float32 {
	if a.batcher == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), syncEmbedTimeout)
	defer cancel()
	res, err := a.batcher.Embed(ctx, []string{text}, "", priority)
	if err != nil {
		slog.Warn("embed failed", "adapter", who, "priority", priority, "err", err)
		return nil
	}

	if len(res) > 0 {
		return res[0].Vector
	}
	return nil
}
