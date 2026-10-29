package repo

import (
	"context"
	"database/sql"

	"github.com/polarisagi/polaris/internal/protocol"
	protorepo "github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// SQLiteChatTurnRepository 读写 chat_turn_steps 及 chat_messages 的回合元数据列。
// 写走读写连接 w，读走读连接 r（WAL 下读不等写，会话加载不被后台回合的批量写卡住）。
type SQLiteChatTurnRepository struct {
	w *sql.DB
	r protocol.SQLQuerier
}

func NewSQLiteChatTurnRepository(w *sql.DB, r protocol.SQLQuerier) *SQLiteChatTurnRepository {
	if r == nil {
		r = w
	}
	return &SQLiteChatTurnRepository{w: w, r: r}
}

var _ protorepo.ChatTurnRepository = (*SQLiteChatTurnRepository)(nil)

func (s *SQLiteChatTurnRepository) AppendTurnSteps(ctx context.Context, rows []protorepo.TurnStepRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeStorageUnavailable, "AppendTurnSteps: begin", err)
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO chat_turn_steps
		(turn_id, session_id, seq, parent_id, kind, data, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return apperr.Wrap(apperr.CodeStorageUnavailable, "AppendTurnSteps: prepare", err)
	}
	defer stmt.Close()
	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx, r.TurnID, r.SessionID, r.Seq, r.ParentID, r.Kind, string(r.Data), r.CreatedAtMs); err != nil {
			return apperr.Wrap(apperr.CodeStorageUnavailable, "AppendTurnSteps: insert", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return apperr.Wrap(apperr.CodeStorageUnavailable, "AppendTurnSteps: commit", err)
	}
	return nil
}

func (s *SQLiteChatTurnRepository) ListTurnSteps(ctx context.Context, sessionID, turnID string) ([]protorepo.TurnStepRow, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT turn_id, session_id, seq, parent_id, kind, data, created_at
		FROM chat_turn_steps WHERE session_id = ? AND turn_id = ? ORDER BY seq`, sessionID, turnID)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeStorageUnavailable, "ListTurnSteps", err)
	}
	defer rows.Close()
	var out []protorepo.TurnStepRow
	for rows.Next() {
		var r protorepo.TurnStepRow
		var data string
		if err := rows.Scan(&r.TurnID, &r.SessionID, &r.Seq, &r.ParentID, &r.Kind, &data, &r.CreatedAtMs); err != nil {
			return nil, apperr.Wrap(apperr.CodeStorageUnavailable, "ListTurnSteps: scan", err)
		}
		r.Data = []byte(data)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeStorageUnavailable, "ListTurnSteps: rows", err)
	}
	return out, nil
}

func (s *SQLiteChatTurnRepository) ListSessionToolSteps(ctx context.Context, sessionID string) ([]protorepo.TurnStepRow, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT turn_id, session_id, seq, parent_id, kind, data, created_at
		FROM chat_turn_steps WHERE session_id = ? AND kind = 'tool' ORDER BY turn_id, seq`, sessionID)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeStorageUnavailable, "ListSessionToolSteps", err)
	}
	defer rows.Close()
	var out []protorepo.TurnStepRow
	for rows.Next() {
		var r protorepo.TurnStepRow
		var data string
		if err := rows.Scan(&r.TurnID, &r.SessionID, &r.Seq, &r.ParentID, &r.Kind, &data, &r.CreatedAtMs); err != nil {
			return nil, apperr.Wrap(apperr.CodeStorageUnavailable, "ListSessionToolSteps: scan", err)
		}
		r.Data = []byte(data)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeStorageUnavailable, "ListSessionToolSteps: rows", err)
	}
	return out, nil
}

func (s *SQLiteChatTurnRepository) TurnSummaries(ctx context.Context, sessionID string) (map[string][]byte, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT turn_id, data FROM chat_turn_steps WHERE session_id = ? AND seq = 0`, sessionID)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeStorageUnavailable, "TurnSummaries", err)
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var id, data string
		if err := rows.Scan(&id, &data); err != nil {
			return nil, apperr.Wrap(apperr.CodeStorageUnavailable, "TurnSummaries: scan", err)
		}
		out[id] = []byte(data)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeStorageUnavailable, "TurnSummaries: rows", err)
	}
	return out, nil
}

func (s *SQLiteChatTurnRepository) AttachTurnMeta(ctx context.Context, messageID int64, turnID, usageJSON string) error {
	if _, err := s.w.ExecContext(ctx, `UPDATE chat_messages SET turn_id = ?, usage = ? WHERE id = ?`, turnID, usageJSON, messageID); err != nil {
		return apperr.Wrap(apperr.CodeStorageUnavailable, "AttachTurnMeta", err)
	}
	return nil
}

func (s *SQLiteChatTurnRepository) PurgeTurnStepsBefore(ctx context.Context, beforeMs int64) (int64, error) {
	res, err := s.w.ExecContext(ctx, `DELETE FROM chat_turn_steps WHERE created_at < ?`, beforeMs)
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeStorageUnavailable, "PurgeTurnStepsBefore", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
