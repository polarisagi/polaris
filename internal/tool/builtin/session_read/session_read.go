// Package session_read 实现只读的对话片段读取工具（ADR-0120 决策六）。
package session_read

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
)

const (
	defaultAround = 6
	maxAround     = 20
	maxMsgRunes   = 2000
)

type args struct {
	SessionID string `json:"session_id"`
	MessageID int64  `json:"message_id"`
	Around    int    `json:"around"`
}

type msg struct {
	id            int64
	role, at, txt string
}

// MakeSessionReadFn 构造 session_read 执行函数。
func MakeSessionReadFn(db protocol.SQLQuerier) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var a args
		if len(input) > 0 {
			if err := json.Unmarshal(input, &a); err != nil {
				return nil, apperr.Wrap(apperr.CodeInvalidInput, "session_read: invalid args", err)
			}
		}
		if db == nil {
			return nil, apperr.New(apperr.CodeInternal, "session_read: database not configured")
		}
		if a.SessionID == "" && a.MessageID == 0 {
			return nil, apperr.New(apperr.CodeInvalidInput, "session_read: session_id or message_id is required")
		}
		if a.Around <= 0 {
			a.Around = defaultAround
		}
		if a.Around > maxAround {
			a.Around = maxAround
		}
		if err := resolveSession(ctx, db, &a); err != nil {
			return nil, err
		}
		msgs, err := load(ctx, db, a)
		if err != nil {
			return nil, err
		}
		if len(msgs) == 0 {
			return []byte("no messages"), nil
		}
		return []byte(format(a, msgs)), nil
	}
}

func resolveSession(ctx context.Context, db protocol.SQLQuerier, a *args) error {
	if a.MessageID == 0 {
		return nil
	}
	var sid string
	if err := db.QueryRowContext(ctx, `SELECT session_id FROM chat_messages WHERE id = ?`, a.MessageID).Scan(&sid); err != nil {
		return apperr.New(apperr.CodeNotFound, "session_read: message not found")
	}
	if a.SessionID != "" && a.SessionID != sid {
		return apperr.New(apperr.CodeInvalidInput, "session_read: message_id does not belong to session_id")
	}
	a.SessionID = sid
	return nil
}

const selectCols = `SELECT id, role, created_at, content FROM chat_messages WHERE session_id = ? AND role IN ('user','assistant')`

func load(ctx context.Context, db protocol.SQLQuerier, a args) ([]msg, error) {
	if a.MessageID == 0 {
		out, err := queryRows(ctx, db, selectCols+` ORDER BY id DESC LIMIT ?`, a.SessionID, a.Around*2)
		reverse(out)
		return out, err
	}
	before, err := queryRows(ctx, db, selectCols+` AND id <= ? ORDER BY id DESC LIMIT ?`, a.SessionID, a.MessageID, a.Around+1)
	if err != nil {
		return nil, err
	}
	reverse(before)
	after, err := queryRows(ctx, db, selectCols+` AND id > ? ORDER BY id ASC LIMIT ?`, a.SessionID, a.MessageID, a.Around)
	if err != nil {
		return nil, err
	}
	return append(before, after...), nil
}

func queryRows(ctx context.Context, db protocol.SQLQuerier, q string, qargs ...any) ([]msg, error) {
	rows, err := db.QueryContext(ctx, q, qargs...)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "session_read: query", err)
	}
	return scan(rows)
}

func scan(rows *sql.Rows) ([]msg, error) {
	defer func() { _ = rows.Close() }()
	var out []msg
	for rows.Next() {
		var m msg
		if err := rows.Scan(&m.id, &m.role, &m.at, &m.txt); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "session_read: scan", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "session_read: rows", err)
	}
	return out, nil
}

func reverse(m []msg) {
	for i, j := 0, len(m)-1; i < j; i, j = i+1, j-1 {
		m[i], m[j] = m[j], m[i]
	}
}

func format(a args, msgs []msg) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "session=%s\n", a.SessionID)
	for _, m := range msgs {
		mark := ""
		if m.id == a.MessageID {
			mark = " <== hit"
		}
		txt := m.txt
		if r := []rune(txt); len(r) > maxMsgRunes {
			txt = string(r[:maxMsgRunes]) + "…[truncated]"
		}
		fmt.Fprintf(&sb, "--- message=%d %s %s%s\n%s\n", m.id, m.role, m.at, mark, txt)
	}
	return sb.String()
}
