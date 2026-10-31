package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/pb"
	"github.com/polarisagi/polaris/pkg/types"
)

// SQLiteAuditRepository 实现 protocol.AuditRepository。
// 操作 events 表（审计记录存储为 topic='audit.policy' 的事件）。
// @arch: docs/arch/M02-Storage-Fabric.md
type SQLiteAuditRepository struct {
	db          *sql.DB
	eventLogger protocol.EventLogger
}

var _ protocol.AuditRepository = (*SQLiteAuditRepository)(nil)

// NewSQLiteAuditRepository 创建 SQLiteAuditRepository。
func NewSQLiteAuditRepository(db *sql.DB, el protocol.EventLogger) *SQLiteAuditRepository {
	return &SQLiteAuditRepository{db: db, eventLogger: el}
}

// AppendAuditEvent 将审计事件写入 events 表（topic='audit.policy'）。
func (r *SQLiteAuditRepository) AppendAuditEvent(ctx context.Context, row types.AuditEventRow) error {
	ev := &pb.Event{
		Id:        row.ID,
		Topic:     "audit.policy",
		Actor:     row.Actor,
		Type:      row.Action,
		Payload:   []byte(row.Meta),
		CreatedAt: time.Now().UnixMilli(),
	}
	if err := r.eventLogger.AppendEvent(ctx, ev); err != nil {
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
			ORDER BY created_at DESC, rowid DESC LIMIT ?`,
			before, limit)
	} else {
		rows, err = r.db.QueryContext(ctx,
			`SELECT id, type, actor, topic, payload, created_at
			FROM events
			WHERE topic = 'audit.policy'
			ORDER BY created_at DESC, rowid DESC LIMIT ?`,
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

// ListAuditTail 按 events rowid 倒序读取审计尾窗；beforeID 使用唯一 ID 游标，
// 不依赖毫秒时间戳，因此同毫秒内的记录不会被分页跳过。
func (r *SQLiteAuditRepository) ListAuditTail(ctx context.Context, limit int, beforeID string) ([]types.AuditEventRow, error) {
	if limit <= 0 {
		return nil, apperr.New(apperr.CodeInvalidInput, "SQLiteAuditRepository.ListAuditTail: limit must be positive")
	}
	var rows *sql.Rows
	var err error
	if beforeID == "" {
		rows, err = r.db.QueryContext(ctx,
			`SELECT id, type, actor, topic, payload, created_at
			FROM events WHERE topic = 'audit.policy'
			ORDER BY rowid DESC LIMIT ?`, limit)
	} else {
		var cursor int64
		if err := r.db.QueryRowContext(ctx,
			`SELECT rowid FROM events WHERE id = ? AND topic = 'audit.policy'`, beforeID).Scan(&cursor); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, apperr.New(apperr.CodeNotFound, "SQLiteAuditRepository.ListAuditTail: cursor not found")
			}
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteAuditRepository.ListAuditTail: read cursor", err)
		}
		rows, err = r.db.QueryContext(ctx,
			`SELECT id, type, actor, topic, payload, created_at
			FROM events WHERE topic = 'audit.policy' AND rowid < ?
			ORDER BY rowid DESC LIMIT ?`, cursor, limit)
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteAuditRepository.ListAuditTail", err)
	}
	defer rows.Close()
	result := make([]types.AuditEventRow, 0, limit)
	for rows.Next() {
		var row types.AuditEventRow
		var createdAt int64
		if err := rows.Scan(&row.ID, &row.Action, &row.Actor, &row.Resource, &row.Meta, &createdAt); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteAuditRepository.ListAuditTail scan", err)
		}
		row.CreatedAt = fmt.Sprintf("%d", createdAt)
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteAuditRepository.ListAuditTail rows", err)
	}
	return result, nil
}

// GetAuditEvent 按全局唯一事件 ID 精确读回审计行。
func (r *SQLiteAuditRepository) GetAuditEvent(ctx context.Context, id string) (*types.AuditEventRow, error) {
	var row types.AuditEventRow
	var createdAt int64
	err := r.db.QueryRowContext(ctx,
		`SELECT id, type, actor, topic, payload, created_at
		FROM events WHERE id = ? AND topic = 'audit.policy'`, id).
		Scan(&row.ID, &row.Action, &row.Actor, &row.Resource, &row.Meta, &createdAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, apperr.New(apperr.CodeNotFound, "SQLiteAuditRepository.GetAuditEvent: event not found")
		}
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteAuditRepository.GetAuditEvent", err)
	}
	row.CreatedAt = fmt.Sprintf("%d", createdAt)
	return &row, nil
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
