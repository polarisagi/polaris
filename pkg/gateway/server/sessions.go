package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
)

// ─── 会话 CRUD HTTP 处理器 ──────────────────────────────────────────────────

// GET /v1/sessions
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	// 先查 channels（单连接 SQLite：两个 rows 不能同时持有连接）
	channelTypes := map[string]string{}
	if chRows, err := s.db.QueryContext(r.Context(), `SELECT id, type FROM channels`); err == nil {
		for chRows.Next() {
			var id, t string
			if chRows.Scan(&id, &t) == nil {
				channelTypes[id] = t
			}
		}
		chRows.Close()
	}

	rows, err := s.db.QueryContext(r.Context(), `
		SELECT cs.id, cs.title, cs.created_at, cs.updated_at,
		       COUNT(cm.id) AS message_count
		FROM   chat_sessions cs
		LEFT JOIN chat_messages cm ON cm.session_id = cs.id
		GROUP BY cs.id
		ORDER BY cs.updated_at DESC
		LIMIT 200`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type sessionRow struct {
		ID           string `json:"id"`
		Title        string `json:"title"`
		CreatedAt    string `json:"created_at"`
		UpdatedAt    string `json:"updated_at"`
		MessageCount int    `json:"message_count"`
		Source       string `json:"source"` // "web" | "telegram" | "discord" | ...
	}
	var list []sessionRow
	for rows.Next() {
		var row sessionRow
		if err := rows.Scan(&row.ID, &row.Title, &row.CreatedAt, &row.UpdatedAt, &row.MessageCount); err != nil {
			continue
		}
		row.Source = "web"
		if strings.HasPrefix(row.ID, "ch_") {
			// session key 格式: ch_<channelID>_<chatID>，channelID 本身以 ch_ 开头
			rest := row.ID[3:] // 去掉前缀 "ch_"
			for chID, chType := range channelTypes {
				if strings.HasPrefix(rest, chID+"_") {
					row.Source = chType
					break
				}
			}
			if row.Source == "web" {
				row.Source = "channel" // 未知平台兜底
			}
		}
		list = append(list, row)
	}
	if list == nil {
		list = []sessionRow{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"sessions": list})
}

// GET /v1/sessions/{sessionID}
func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("sessionID")
	maxChars := 50000
	if v := r.URL.Query().Get("max_chars"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxChars = n
		}
	}

	rows, err := s.db.QueryContext(r.Context(),
		`SELECT role, content, tool_calls, created_at, updated_at FROM chat_messages WHERE session_id=? ORDER BY id`, sessionID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type msgRow struct {
		Role         string          `json:"role"`
		Content      string          `json:"content"`
		ToolCalls    json.RawMessage `json:"tool_calls,omitempty"`
		TaskDuration int64           `json:"task_duration,omitempty"` // in ms
	}
	var msgs []msgRow
	total := 0
	for rows.Next() {
		var m msgRow
		var tc sql.NullString
		var createdStr, updatedStr string
		if err := rows.Scan(&m.Role, &m.Content, &tc, &createdStr, &updatedStr); err != nil {
			continue
		}
		if tc.Valid && tc.String != "" {
			m.ToolCalls = json.RawMessage(tc.String)
		}

		// Parse duration if both exist
		if createdStr != "" && updatedStr != "" {
			layout := "2006-01-02T15:04:05Z"
			tC, _ := time.Parse(layout, createdStr)
			tU, _ := time.Parse(layout, updatedStr)
			if !tU.IsZero() && !tC.IsZero() {
				m.TaskDuration = tU.Sub(tC).Milliseconds()
			}
		}

		total += len(m.Content)
		if total > maxChars {
			break
		}
		msgs = append(msgs, m)
	}
	if msgs == nil {
		msgs = []msgRow{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"session_id": sessionID, "messages": msgs})
}

// DELETE /v1/sessions/{sessionID}
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("sessionID")
	if _, err := s.db.ExecContext(r.Context(),
		`DELETE FROM chat_sessions WHERE id=?`, sessionID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
}

// ─── 会话辅助方法 ────────────────────────────────────────────────────────────

func (s *Server) ensureSession(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO chat_sessions(id) VALUES(?)`, sessionID)
	return err
}

// loadMessages 从数据库加载会话历史（role + content 纯文本）。
// 【架构约束】图片/视频等多模态 Parts 不落盘，仅存在于当轮请求的内存中。
// 这意味着多轮视觉对话中，历史轮次的图片不会随上下文一并重传给大模型。
// 如需多轮图片记忆，需要在 saveMessage 中序列化 Parts 并在此处反序列化还原。
func (s *Server) loadMessages(ctx context.Context, sessionID string) ([]protocol.Message, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT role, content FROM chat_messages WHERE session_id=? ORDER BY id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []protocol.Message
	for rows.Next() {
		var m protocol.Message
		if err := rows.Scan(&m.Role, &m.Content); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, nil
}

func (s *Server) saveMessage(ctx context.Context, sessionID, role, content string, toolCalls string, durationMs int64) error {
	if durationMs > 0 {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO chat_messages(session_id, role, content, tool_calls, created_at, updated_at) 
			 VALUES(?,?,?,?, datetime('now', '-' || ? || ' seconds'), datetime('now'))`,
			sessionID, role, content, toolCalls, float64(durationMs)/1000.0)
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO chat_messages(session_id, role, content, tool_calls) VALUES(?,?,?,?)`,
		sessionID, role, content, toolCalls)
	return err
}

// updateSessionTitle 把首条用户消息截断为会话标题（仅在 title 为空时写入）。
func (s *Server) updateSessionTitle(ctx context.Context, sessionID, firstInput string) error {
	title := firstInput
	if len([]rune(title)) > 40 {
		runes := []rune(title)
		title = string(runes[:40]) + "…"
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE chat_sessions SET title=?, updated_at=strftime('%Y-%m-%dT%H:%M:%SZ','now')
		 WHERE id=? AND (title='' OR title IS NULL)`,
		title, sessionID)
	return err
}

// touchSession 更新 updated_at（每次对话后调用）。
func (s *Server) touchSession(ctx context.Context, sessionID string) {
	s.db.ExecContext(ctx, //nolint:errcheck
		`UPDATE chat_sessions SET updated_at=strftime('%Y-%m-%dT%H:%M:%SZ','now') WHERE id=?`,
		sessionID)
}

// newSessionID 生成 16 字节随机 hex ID。
func newSessionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("sess_%d", 0)
	}
	return "sess_" + hex.EncodeToString(b)
}

// truncate 截断字节，防止错误消息过长写入 SSE。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ─── 全文搜索 ────────────────────────────────────────────────────────────────

// GET /v1/search?q=<query>&limit=<n>
// 借助 FTS5 跨会话搜索历史消息，按会话分组返回匹配片段。
// 要求：016_fts5_search.sql 已运行（messages_fts 虚拟表存在）。
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		http.Error(w, "q is required", http.StatusBadRequest)
		return
	}
	limit := 10
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 50 {
			limit = n
		}
	}

	// FTS5 搜索：按会话分组，每会话取最多 3 条匹配；结果按 rank 排序
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT cm.session_id, cs.title, cs.updated_at, cm.role,
		       snippet(messages_fts, 0, '**', '**', '…', 20) AS snip,
		       cm.content
		FROM   messages_fts
		JOIN   chat_messages cm ON cm.id = messages_fts.rowid
		JOIN   chat_sessions cs ON cs.id = cm.session_id
		WHERE  messages_fts MATCH ?
		ORDER  BY rank
		LIMIT  ?`, q, limit*3)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type matchRow struct {
		Role    string `json:"role"`
		Snippet string `json:"snippet"`
		Content string `json:"content"`
	}
	type sessionResult struct {
		SessionID string     `json:"session_id"`
		Title     string     `json:"title"`
		UpdatedAt string     `json:"updated_at"`
		Matches   []matchRow `json:"matches"`
	}

	ordered := []string{}
	bySession := map[string]*sessionResult{}

	for rows.Next() {
		var sessID, title, updatedAt, role, snip, content string
		if err := rows.Scan(&sessID, &title, &updatedAt, &role, &snip, &content); err != nil {
			continue
		}
		sr, ok := bySession[sessID]
		if !ok {
			sr = &sessionResult{
				SessionID: sessID,
				Title:     title,
				UpdatedAt: updatedAt,
			}
			bySession[sessID] = sr
			ordered = append(ordered, sessID)
		}
		if len(sr.Matches) < 3 {
			sr.Matches = append(sr.Matches, matchRow{Role: role, Snippet: snip, Content: truncate(content, 300)})
		}
	}

	results := make([]*sessionResult, 0, len(ordered))
	for _, id := range ordered {
		results = append(results, bySession[id])
		if len(results) >= limit {
			break
		}
	}
	if results == nil {
		results = []*sessionResult{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"query": q, "results": results})
}
