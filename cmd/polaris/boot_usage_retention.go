package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// llmCallPruner 是 llm_calls 保留期清理所需的最小写接口（实现：repo.SQLiteLLMCallRepository）。
type llmCallPruner interface {
	PruneLLMCalls(ctx context.Context, beforeMs int64) (int64, error)
}

// pruneLLMCallsByRetention 删除 created_at 早于 now-retentionDays 的 llm_calls 行（ADR-0105 决策八）。
// retentionDays <= 0 表示不清理（配置校验已排除负数，0 是显式的"永久保留"）。
// 阈值边界用 AddDate 按日历日回退而非固定 24h×N：夏令时切换日也保证"N 个自然日前"。
func pruneLLMCallsByRetention(ctx context.Context, p llmCallPruner, retentionDays int, now time.Time) (int64, error) {
	if p == nil || retentionDays <= 0 {
		return 0, nil
	}
	cutoff := now.AddDate(0, 0, -retentionDays)
	n, err := p.PruneLLMCalls(ctx, cutoff.UnixMilli())
	if err != nil {
		return n, apperr.Wrap(apperr.CodeStorageUnavailable, "boot: llm_calls 保留期清理失败", err)
	}
	if n > 0 {
		slog.Info("polaris: llm_calls pruned by retention", "rows", n, "retention_days", retentionDays)
	}
	return n, nil
}
