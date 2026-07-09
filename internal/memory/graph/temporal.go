package graph

import (
	"context"
	"fmt"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// TemporalExpirer 每小时扫描语义实体，将超过 valid_until 的记录置 status='expired'。
// 触发路径: cmd/polaris/main.go → 1h ticker → TemporalExpirer.ExpireStale()。
type TemporalExpirer struct {
	db protocol.SQLQuerier
}

// NewTemporalExpirer 创建时态过期器。db 必须非 nil。
func NewTemporalExpirer(db protocol.SQLQuerier) *TemporalExpirer {
	return &TemporalExpirer{db: db}
}

// ExpireStale 将 valid_until < now 且 status='active' 的实体置为 'expired'。
// 返回过期条目数量。
func (te *TemporalExpirer) ExpireStale(ctx context.Context) (int64, error) {
	now := time.Now().UnixMilli()
	result, err := te.db.ExecContext(ctx,
		`UPDATE semantic_entities
            SET status = 'expired', updated_at = ?
          WHERE status = 'active'
            AND valid_until IS NOT NULL
            AND valid_until < ?`,
		now, now,
	)
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "temporal_expirer: expire stale", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "temporal_expirer: rows affected", err)
	}
	return affected, nil
}

// SetValidWindow 为实体设置有效时间窗（写入时调用）。
// duration == 0 表示永久有效（valid_until = NULL）。
func SetValidWindow(validFromMs int64, duration time.Duration) (validFrom, validUntil int64) {
	validFrom = validFromMs
	if duration <= 0 {
		return validFrom, 0 // 0 代表 NULL
	}
	return validFrom, validFromMs + duration.Milliseconds()
}

// IsValidAt 检查时间戳是否在有效窗内（用于内存过滤，无需数据库）。
func IsValidAt(validFrom, validUntil int64, nowMs int64) bool {
	if validFrom > 0 && nowMs < validFrom {
		return false
	}
	if validUntil > 0 && nowMs > validUntil {
		return false
	}
	return true
}

// FormatValidWindow 调试用格式化。
func FormatValidWindow(validFrom, validUntil int64) string {
	from := "always"
	if validFrom > 0 {
		from = time.UnixMilli(validFrom).Format(time.RFC3339)
	}
	until := "forever"
	if validUntil > 0 {
		until = time.UnixMilli(validUntil).Format(time.RFC3339)
	}
	return fmt.Sprintf("[%s → %s]", from, until)
}
