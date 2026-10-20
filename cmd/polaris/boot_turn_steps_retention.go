package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// turnStepsPruner 是步骤保留期清理所需的最小写接口（实现：repo.SQLiteChatTurnRepository）。
type turnStepsPruner interface {
	PurgeTurnStepsBefore(ctx context.Context, beforeMs int64) (int64, error)
}

// pruneTurnStepsByRetention 删除 created_at 早于 now-retentionDays 的步骤行（ADR-0118）。
// retentionDays <= 0 表示永久保留。只删步骤：消息、用量账本不受影响，回看历史回合时
// 摘要行随之消失，界面退化为不显示时间线。
func pruneTurnStepsByRetention(ctx context.Context, p turnStepsPruner, retentionDays int, now time.Time) (int64, error) {
	if p == nil || retentionDays <= 0 {
		return 0, nil
	}
	n, err := p.PurgeTurnStepsBefore(ctx, now.AddDate(0, 0, -retentionDays).UnixMilli())
	if err != nil {
		return n, apperr.Wrap(apperr.CodeStorageUnavailable, "boot: 会话步骤保留期清理失败", err)
	}
	if n > 0 {
		slog.Info("polaris: chat_turn_steps pruned by retention", "rows", n, "retention_days", retentionDays)
	}
	return n, nil
}
