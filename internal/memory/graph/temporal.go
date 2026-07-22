package graph

import (
	"context"
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
