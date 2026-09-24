package search

import (
	"context"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// syncEmbedTimeout 同步适配器单次等待上限：调用方 ctx 可能无截止，
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
func (a *SyncBatcherAdapter) Embed(ctx context.Context, text string) []float32 {
	return a.embedWithPriority(ctx, text, PriorityHigh, "SyncBatcherAdapter")
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
func (a *BackgroundEmbedder) Embed(ctx context.Context, text string) []float32 {
	return (&SyncBatcherAdapter{batcher: a.batcher}).embedWithPriority(ctx, text, PriorityLow, "BackgroundEmbedder")
}

// EmbedBatch 低优先级批量嵌入：经批处理器 Low 通道按单批上限切块串行下发（ADR-0099）。
// 供扩展目录预计算等整批回填使用——此前它们直接调用底层引擎的 EmbedBatch，一次把
// 上百条文本压给串行后端，交互嵌入在后端排队 10s+。
func (a *BackgroundEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	res, err := a.batcher.Embed(ctx, texts, "", PriorityLow)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "BackgroundEmbedder.EmbedBatch", err)
	}
	vecs := make([][]float32, len(res))
	for i, r := range res {
		if r.Error != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "BackgroundEmbedder.EmbedBatch", r.Error)
		}
		vecs[i] = r.Vector
	}
	return vecs, nil
}

func (a *SyncBatcherAdapter) embedWithPriority(ctx context.Context, text string, priority int, who string) []float32 {
	if a.batcher == nil {
		return nil
	}

	callerCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, syncEmbedTimeout)
	defer cancel()
	res, err := a.batcher.Embed(ctx, []string{text}, "", priority)
	if err != nil {
		// 调用方主动放弃（其截止时间先到，如 Agent 召回 3s 预算）是预期的降级，不是嵌入
		// 失败：ADR-0099 让调用方 ctx 贯穿下来之后，按 WARN 记会把正常降级报成故障。
		if callerCtx.Err() != nil {
			slog.Debug("embed abandoned by caller", "adapter", who, "priority", priority, "err", callerCtx.Err())
			return nil
		}
		slog.Warn("embed failed", "adapter", who, "priority", priority, "err", err)
		return nil
	}

	if len(res) > 0 {
		return res[0].Vector
	}
	return nil
}
