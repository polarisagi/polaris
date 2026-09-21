package repo

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/polarisagi/polaris/internal/protocol"
	protorepo "github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/types"
	"github.com/polarisagi/polaris/pkg/util"
)

// SQLiteChatRepository 实现 protocol.ChatRepository。
// 操作 chat_sessions, chat_messages 表以及 messages_fts 虚拟表。
// @arch: docs/arch/M02-Storage-Fabric.md
type SQLiteChatRepository struct {
	db *sql.DB
}

var _ protocol.ChatRepository = (*SQLiteChatRepository)(nil)

func NewSQLiteChatRepository(db *sql.DB) *SQLiteChatRepository {
	return &SQLiteChatRepository{db: db}
}

// CreateSession 创建一个新会话
func (r *SQLiteChatRepository) CreateSession(ctx context.Context, row types.ChatSessionRow) error {
	// project_id 空串按默认项目处理；INSERT OR IGNORE 保证已存在会话不被改归属
	// （聊天请求带不同 project_id 时以库内为准，ADR-0097 决策一）。
	projectID := row.ProjectID
	if projectID == "" {
		projectID = protorepo.DefaultProjectID
	}
	// INSERT ... SELECT 以项目存在且未归档为前提：归档项目不再接收新会话，
	// 但其中已有会话照常可用（它们走 INSERT OR IGNORE 的"已存在"分支）。
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := r.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO chat_sessions(id, title, project_id, thrashing_index, created_at, updated_at)
		 SELECT ?, ?, p.id, ?, ?, ? FROM projects p WHERE p.id = ? AND p.archived = 0`,
		row.ID, row.Title, row.ThrashingIndex, now, now, projectID)
	if err != nil {
		if strings.Contains(err.Error(), "FOREIGN KEY") {
			return apperr.Wrap(apperr.CodeNotFound, "project not found", err)
		}
		return apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.CreateSession", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	return r.explainNoInsert(ctx, row.ID, projectID)
}

// explainNoInsert 区分 CreateSession 未插入的三种原因：会话已存在（正常）、
// 项目不存在、项目已归档。
func (r *SQLiteChatRepository) explainNoInsert(ctx context.Context, sessionID, projectID string) error {
	var one int
	err := r.db.QueryRowContext(ctx, `SELECT 1 FROM chat_sessions WHERE id=?`, sessionID).Scan(&one)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.CreateSession check session", err)
	}
	var archived int
	err = r.db.QueryRowContext(ctx, `SELECT archived FROM projects WHERE id=?`, projectID).Scan(&archived)
	if err == sql.ErrNoRows {
		return apperr.Wrap(apperr.CodeNotFound, "project not found", err)
	}
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.CreateSession check project", err)
	}
	return apperr.New(apperr.CodeInvalidInput, "项目已归档，不能在其中新建会话")
}

// GetSession 获取会话信息
func (r *SQLiteChatRepository) GetSession(ctx context.Context, id string) (*types.ChatSessionRow, error) {
	var row types.ChatSessionRow
	err := r.db.QueryRowContext(ctx,
		`SELECT id, title, project_id, thrashing_index, created_at, updated_at FROM chat_sessions WHERE id=?`, id,
	).Scan(&row.ID, &row.Title, &row.ProjectID, &row.ThrashingIndex, &row.CreatedAt, &row.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.GetSession", err)
	}
	return &row, nil
}

// ListSessions 列出最近的会话（全部项目）
func (r *SQLiteChatRepository) ListSessions(ctx context.Context, limit int) ([]types.ChatSessionRow, error) {
	return r.listSessions(ctx, "", limit)
}

// ListProjectSessions 列出某项目下最近的会话。
func (r *SQLiteChatRepository) ListProjectSessions(ctx context.Context, projectID string, limit int) ([]types.ChatSessionRow, error) {
	return r.listSessions(ctx, projectID, limit)
}

// listSessions projectID 为空 = 不按项目过滤。
func (r *SQLiteChatRepository) listSessions(ctx context.Context, projectID string, limit int) ([]types.ChatSessionRow, error) {
	q := `SELECT cs.id, cs.title, cs.project_id, cs.thrashing_index, cs.created_at, cs.updated_at, COUNT(cm.id) AS message_count
		FROM chat_sessions cs
		LEFT JOIN chat_messages cm ON cm.session_id = cs.id `
	args := []any{}
	if projectID != "" {
		q += `WHERE cs.project_id = ? `
		args = append(args, projectID)
	}
	q += `GROUP BY cs.id ORDER BY cs.updated_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.ListSessions", err)
	}
	defer rows.Close()

	var result []types.ChatSessionRow
	for rows.Next() {
		var row types.ChatSessionRow
		if err := rows.Scan(&row.ID, &row.Title, &row.ProjectID, &row.ThrashingIndex, &row.CreatedAt, &row.UpdatedAt, &row.MessageCount); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.ListSessions scan", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.ListSessions rows", err)
	}
	return result, nil
}

// SetSessionProject 把会话移动到另一个项目。会话不存在 / 项目不存在均返回 CodeNotFound。
func (r *SQLiteChatRepository) SetSessionProject(ctx context.Context, sessionID, projectID string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE chat_sessions SET project_id=? WHERE id=?`, projectID, sessionID)
	if err != nil {
		if strings.Contains(err.Error(), "FOREIGN KEY") {
			return apperr.Wrap(apperr.CodeNotFound, "project not found", err)
		}
		return apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.SetSessionProject", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apperr.New(apperr.CodeNotFound, "session not found")
	}
	return nil
}

// UpdateSessionTitle 更新会话标题
func (r *SQLiteChatRepository) UpdateSessionTitle(ctx context.Context, id, title string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE chat_sessions SET title=?, updated_at=strftime('%Y-%m-%dT%H:%M:%SZ','now') WHERE id=?`,
		title, id)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.UpdateSessionTitle", err)
	}
	return nil
}

// UpdateSessionThrashingIndex 更新 thrashing_index
func (r *SQLiteChatRepository) UpdateSessionThrashingIndex(ctx context.Context, id string, idx float64) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE chat_sessions SET thrashing_index=?, updated_at=strftime('%Y-%m-%dT%H:%M:%SZ','now') WHERE id=?`,
		idx, id)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.UpdateSessionThrashingIndex", err)
	}
	return nil
}

// DeleteSession 删除会话及其消息
func (r *SQLiteChatRepository) DeleteSession(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM chat_sessions WHERE id=?`, id)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.DeleteSession", err)
	}
	return nil
}

// AppendMessage 追加一条消息。
//
// 修复：原实现的 INSERT 语句恒定使用 strftime('now') 作为 created_at/updated_at，
// 完全忽略调用方传入的 row.CreatedAt/row.UpdatedAt——ChatHandler.SaveMessage
// 为还原"回复实际耗时"特意回算了 created_at（now - durationMs），但因这里从未
// 使用这两个字段，回算逻辑此前是死代码，chat_messages.created_at 永远等于写入
// 时刻而非推理起始时刻。现在用 COALESCE + NULLIF 判空字符串回退 strftime(now)：
// 调用方未设置（如 durationMs<=0 场景）时保持原有写入即当前时间行为，
// 非空时采用调用方回算值。
func (r *SQLiteChatRepository) AppendMessage(ctx context.Context, row types.ChatMessageRow) error {
	if err := r.appendMessage(ctx, "INSERT", row); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.AppendMessage", err)
	}
	return nil
}

// AppendMessageIdempotent 与 AppendMessage 相同，但使用 INSERT OR IGNORE 并要求
// row.DedupeKey 非空（GD-13-004 复核修复：outbox 重试兜底路径专用，OutboxWorker
// at-least-once 语义下可能对同一条记录多次调用 handler，dedupe_key 唯一索引
// 保证重复调用不会插入重复消息行）。
func (r *SQLiteChatRepository) AppendMessageIdempotent(ctx context.Context, row types.ChatMessageRow) error {
	if row.DedupeKey == "" {
		return apperr.New(apperr.CodeInvalidInput, "SQLiteChatRepository.AppendMessageIdempotent: dedupe_key required")
	}
	if err := r.appendMessage(ctx, "INSERT OR IGNORE", row); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.AppendMessageIdempotent", err)
	}
	return nil
}

func (r *SQLiteChatRepository) appendMessage(ctx context.Context, verb string, row types.ChatMessageRow) error {
	var dedupeKey any
	if row.DedupeKey != "" {
		dedupeKey = row.DedupeKey
	}
	_, err := r.db.ExecContext(ctx,
		verb+` INTO chat_messages(session_id, role, content, reasoning_content, tool_calls, file_offset, file_length, dedupe_key, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,
			COALESCE(NULLIF(?, ''), strftime('%Y-%m-%dT%H:%M:%SZ','now')),
			COALESCE(NULLIF(?, ''), strftime('%Y-%m-%dT%H:%M:%SZ','now')))`,
		row.SessionID, row.Role, row.Content, row.ReasoningContent, row.ToolCalls, row.FileOffset, row.FileLength, dedupeKey,
		row.CreatedAt, row.UpdatedAt)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.appendMessage", err)
	}
	return nil
}

// ListMessages 列出指定会话的消息。limit<=0 表示不限行数（SQLite LIMIT -1）。
func (r *SQLiteChatRepository) ListMessages(ctx context.Context, sessionID string, limit int) ([]types.ChatMessageRow, error) {
	if limit <= 0 {
		limit = -1 // SQLite: LIMIT -1 = no upper bound
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, session_id, role, content, reasoning_content, tool_calls, file_offset, file_length, created_at, updated_at
		FROM chat_messages WHERE session_id=? ORDER BY id ASC LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.ListMessages", err)
	}
	defer rows.Close()

	var result []types.ChatMessageRow
	for rows.Next() {
		var row types.ChatMessageRow
		if err := rows.Scan(&row.ID, &row.SessionID, &row.Role, &row.Content, &row.ReasoningContent, &row.ToolCalls, &row.FileOffset, &row.FileLength, &row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.ListMessages scan", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.ListMessages rows", err)
	}
	return result, nil
}

// SearchMessages 全文检索消息
func (r *SQLiteChatRepository) SearchMessages(ctx context.Context, query string, limit int) ([]types.ChatMessageRow, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT cm.id, cm.session_id, cm.role, cm.content, cm.tool_calls, cm.file_offset, cm.file_length, cm.created_at, cm.updated_at
		FROM messages_fts fts
		JOIN chat_messages cm ON cm.id = fts.rowid
		WHERE messages_fts MATCH ? ORDER BY rank LIMIT ?`, util.QuoteFTS5Query(query), limit)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.SearchMessages", err)
	}
	defer rows.Close()

	var result []types.ChatMessageRow
	for rows.Next() {
		var row types.ChatMessageRow
		if err := rows.Scan(&row.ID, &row.SessionID, &row.Role, &row.Content, &row.ToolCalls, &row.FileOffset, &row.FileLength, &row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.SearchMessages scan", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.SearchMessages rows", err)
	}
	return result, nil
}

// --- Additional mutations ---

// RestoreSession 备份恢复。projectID 在本库不存在（旧备份无该字段 / 项目未随备份恢复）
// 时落默认项目，而不是因外键失败丢掉整个会话。
func (r *SQLiteChatRepository) RestoreSession(ctx context.Context, id, title, projectID string, thrashing float64, createdAt, updatedAt string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO chat_sessions(id, title, project_id, thrashing_index, created_at, updated_at)
		 VALUES(?, ?, COALESCE((SELECT id FROM projects WHERE id = ?), ?), ?, ?, ?)`,
		id, title, projectID, protorepo.DefaultProjectID, thrashing, createdAt, updatedAt)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	return nil
}

func (r *SQLiteChatRepository) RestoreMessage(ctx context.Context, id, sessionID, role, content, createdAt string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO chat_messages(id, session_id, role, content, created_at) VALUES(?,?,?,?,?)`,
		id, sessionID, role, content, createdAt)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	return nil
}

func (r *SQLiteChatRepository) TouchSession(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE chat_sessions SET updated_at=datetime('now') WHERE id=?`, id)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	return nil
}

func (r *SQLiteChatRepository) ClearNonSystemMessages(ctx context.Context, sessionID string) error {
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM chat_messages WHERE session_id=? AND role != 'system'`, sessionID)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	return nil
}

func (r *SQLiteChatRepository) ReplaceSessionMessages(ctx context.Context, sessionID string, msgs []types.ChatMessageRow) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `DELETE FROM chat_messages WHERE session_id=?`, sessionID); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	for _, m := range msgs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO chat_messages(session_id, role, content) VALUES(?,?,?)`,
			sessionID, m.Role, m.Content); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "db error", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.ReplaceSessionMessages commit", err)
	}
	return nil
}
