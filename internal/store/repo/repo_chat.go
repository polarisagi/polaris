package repo

import (
	"context"
	"database/sql"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// SQLiteChatRepository 实现 protocol.ChatRepository。
// 操作 chat_sessions, chat_messages 表以及 messages_fts 虚拟表。
// @arch: docs/upgrade/repo-interface-migration.md §3.1
type SQLiteChatRepository struct {
	db *sql.DB
}

var _ protocol.ChatRepository = (*SQLiteChatRepository)(nil)

func NewSQLiteChatRepository(db *sql.DB) *SQLiteChatRepository {
	return &SQLiteChatRepository{db: db}
}

// CreateSession 创建一个新会话
func (r *SQLiteChatRepository) CreateSession(ctx context.Context, row types.ChatSessionRow) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO chat_sessions(id, title, thrashing_index, created_at, updated_at) VALUES(?, ?, ?, ?, ?)`,
		row.ID, row.Title, row.ThrashingIndex, time.Now().UTC().Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.CreateSession", err)
	}
	return nil
}

// GetSession 获取会话信息
func (r *SQLiteChatRepository) GetSession(ctx context.Context, id string) (*types.ChatSessionRow, error) {
	var row types.ChatSessionRow
	err := r.db.QueryRowContext(ctx,
		`SELECT id, title, thrashing_index, created_at, updated_at FROM chat_sessions WHERE id=?`, id,
	).Scan(&row.ID, &row.Title, &row.ThrashingIndex, &row.CreatedAt, &row.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.GetSession", err)
	}
	return &row, nil
}

// ListSessions 列出最近的会话
func (r *SQLiteChatRepository) ListSessions(ctx context.Context, limit int) ([]types.ChatSessionRow, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT cs.id, cs.title, cs.thrashing_index, cs.created_at, cs.updated_at, COUNT(cm.id) AS message_count 
		FROM chat_sessions cs 
		LEFT JOIN chat_messages cm ON cm.session_id = cs.id 
		GROUP BY cs.id 
		ORDER BY cs.updated_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.ListSessions", err)
	}
	defer rows.Close()

	var result []types.ChatSessionRow
	for rows.Next() {
		var row types.ChatSessionRow
		if err := rows.Scan(&row.ID, &row.Title, &row.ThrashingIndex, &row.CreatedAt, &row.UpdatedAt, &row.MessageCount); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.ListSessions scan", err)
		}
		result = append(result, row)
	}
	return result, rows.Err()
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

// AppendMessage 追加一条消息
func (r *SQLiteChatRepository) AppendMessage(ctx context.Context, row types.ChatMessageRow) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO chat_messages(session_id, role, content, tool_calls, created_at, updated_at) 
		VALUES(?,?,?,?, strftime('%Y-%m-%dT%H:%M:%SZ','now'), strftime('%Y-%m-%dT%H:%M:%SZ','now'))`,
		row.SessionID, row.Role, row.Content, row.ToolCalls)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.AppendMessage", err)
	}
	return nil
}

// ListMessages 列出指定会话的消息。limit<=0 表示不限行数（SQLite LIMIT -1）。
func (r *SQLiteChatRepository) ListMessages(ctx context.Context, sessionID string, limit int) ([]types.ChatMessageRow, error) {
	if limit <= 0 {
		limit = -1 // SQLite: LIMIT -1 = no upper bound
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, session_id, role, content, tool_calls, created_at, updated_at
		FROM chat_messages WHERE session_id=? ORDER BY id ASC LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.ListMessages", err)
	}
	defer rows.Close()

	var result []types.ChatMessageRow
	for rows.Next() {
		var row types.ChatMessageRow
		if err := rows.Scan(&row.ID, &row.SessionID, &row.Role, &row.Content, &row.ToolCalls, &row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.ListMessages scan", err)
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// SearchMessages 全文检索消息
func (r *SQLiteChatRepository) SearchMessages(ctx context.Context, query string, limit int) ([]types.ChatMessageRow, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT cm.id, cm.session_id, cm.role, cm.content, cm.tool_calls, cm.created_at, cm.updated_at
		FROM messages_fts fts
		JOIN chat_messages cm ON cm.id = fts.rowid
		WHERE messages_fts MATCH ? ORDER BY rank LIMIT ?`, query, limit)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.SearchMessages", err)
	}
	defer rows.Close()

	var result []types.ChatMessageRow
	for rows.Next() {
		var row types.ChatMessageRow
		if err := rows.Scan(&row.ID, &row.SessionID, &row.Role, &row.Content, &row.ToolCalls, &row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.SearchMessages scan", err)
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// --- Additional mutations ---

func (r *SQLiteChatRepository) RestoreSession(ctx context.Context, id, title string, thrashing float64, createdAt, updatedAt string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO chat_sessions(id, title, thrashing_index, created_at, updated_at) VALUES(?,?,?,?,?)`,
		id, title, thrashing, createdAt, updatedAt)
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
	return tx.Commit()
}
