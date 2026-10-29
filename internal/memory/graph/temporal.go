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

// ExpireStale 将 valid_until < now 且 status='active' 的实体与关系边均置为
// 'expired'（ADR-0083 将双时态扩展到关系边后，关系边与实体共用同一到期语义，
// 不新增独立 ticker）。返回两者合计过期条目数量。
func (te *TemporalExpirer) ExpireStale(ctx context.Context) (int64, error) {
	now := time.Now().UnixMilli()

	entityResult, err := te.db.ExecContext(ctx,
		`UPDATE semantic_entities
            SET status = 'expired', updated_at = ?
          WHERE status = 'active'
            AND valid_until IS NOT NULL
            AND valid_until < ?`,
		now, now,
	)
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "temporal_expirer: expire stale entities", err)
	}
	entityAffected, err := entityResult.RowsAffected()
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "temporal_expirer: entity rows affected", err)
	}

	relationResult, err := te.db.ExecContext(ctx,
		`UPDATE semantic_relations
            SET status = 'expired', updated_at = ?
          WHERE status = 'active'
            AND valid_until IS NOT NULL
            AND valid_until < ?`,
		now, now,
	)
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "temporal_expirer: expire stale relations", err)
	}
	relationAffected, err := relationResult.RowsAffected()
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "temporal_expirer: relation rows affected", err)
	}

	return entityAffected + relationAffected, nil
}
