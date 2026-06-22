package repo

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// SQLiteAuditRepository 实现 protocol.AuditRepository。
// 操作 events 表（审计记录存储为 topic='audit.policy' 的事件）。
// @arch: docs/upgrade/repo-interface-migration.md §3.5
type SQLiteAuditRepository struct {
	db *sql.DB
}

var _ protocol.AuditRepository = (*SQLiteAuditRepository)(nil)

// NewSQLiteAuditRepository 创建 SQLiteAuditRepository。
func NewSQLiteAuditRepository(db *sql.DB) *SQLiteAuditRepository {
	return &SQLiteAuditRepository{db: db}
}

// AppendAuditEvent 将审计事件写入 events 表（topic='audit.policy'）。
func (r *SQLiteAuditRepository) AppendAuditEvent(ctx context.Context, row types.AuditEventRow) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO events (id, topic, actor, type, payload, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		row.ID, "audit.policy", row.Actor, row.Action,
		row.Meta, time.Now().UnixMicro())
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteAuditRepository.AppendAuditEvent", err)
	}
	return nil
}

// ListAuditEvents 查询审计事件列表（降序，分页）。
// before 为空时不过滤时间。
func (r *SQLiteAuditRepository) ListAuditEvents(ctx context.Context, limit int, before string) ([]types.AuditEventRow, error) {
	var rows *sql.Rows
	var err error
	if before != "" {
		rows, err = r.db.QueryContext(ctx,
			`SELECT id, type, actor, topic, payload, created_at
			FROM events
			WHERE topic = 'audit.policy' AND created_at < ?
			ORDER BY created_at DESC LIMIT ?`,
			before, limit)
	} else {
		rows, err = r.db.QueryContext(ctx,
			`SELECT id, type, actor, topic, payload, created_at
			FROM events
			WHERE topic = 'audit.policy'
			ORDER BY created_at DESC LIMIT ?`,
			limit)
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteAuditRepository.ListAuditEvents", err)
	}
	defer rows.Close()

	var result []types.AuditEventRow
	for rows.Next() {
		var row types.AuditEventRow
		var createdAt int64
		var resource string // topic 字段
		if err := rows.Scan(&row.ID, &row.Action, &row.Actor, &resource, &row.Meta, &createdAt); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteAuditRepository.ListAuditEvents scan", err)
		}
		row.Resource = resource
		row.CreatedAt = fmt.Sprintf("%d", createdAt)
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteAuditRepository.ListAuditEvents rows", err)
	}
	return result, nil
}

// DeleteAuditEventsBefore 删除 before 时间前的审计事件。
// before 为 UnixMicro 时间戳字符串。
func (r *SQLiteAuditRepository) DeleteAuditEventsBefore(ctx context.Context, before string) (int64, error) {
	result, err := r.db.ExecContext(ctx,
		`DELETE FROM events WHERE topic = 'audit.policy' AND created_at < ?`,
		before)
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "SQLiteAuditRepository.DeleteAuditEventsBefore", err)
	}
	n, _ := result.RowsAffected()
	return n, nil
}
