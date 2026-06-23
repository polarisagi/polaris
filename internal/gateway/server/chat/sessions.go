package chat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/polarisagi/polaris/pkg/types"
)

// ─── 会话 CRUD HTTP 处理器 ──────────────────────────────────────────────────

// GET /v1/sessions
func (h *ChatHandler) HandleListSessions(w http.ResponseWriter, r *http.Request) {
	// 先查 channels（单连接 SQLite：两个 rows 不能同时持有连接）
	channelTypes := map[string]string{}
	if chRows, err := h.DB.QueryContext(r.Context(), `SELECT id, type FROM channels`); err == nil {
		for chRows.Next() {
			var id, t string
			if chRows.Scan(&id, &t) == nil {
				channelTypes[id] = t
			}
		}
		chRows.Close()
	}

	sessions, err := h.ChatRepo.ListSessions(r.Context(), 200)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	type sessionRow struct {
		ID             string  `json:"id"`
		Title          string  `json:"title"`
		ThrashingIndex float64 `json:"thrashing_index"`
		CreatedAt      string  `json:"created_at"`
		UpdatedAt      string  `json:"updated_at"`
		MessageCount   int     `json:"message_count"`
		Source         string  `json:"source"` // "web" | "telegram" | "discord" | ...
	}
	list := make([]sessionRow, 0, len(sessions))
	for _, row := range sessions {
		sr := sessionRow{
			ID:             row.ID,
			Title:          row.Title,
			ThrashingIndex: row.ThrashingIndex,
			CreatedAt:      row.CreatedAt,
			UpdatedAt:      row.UpdatedAt,
			MessageCount:   row.MessageCount,
			Source:         "web",
		}
		if strings.HasPrefix(sr.ID, "ch_") {
			// session key 格式: ch_<channelID>_<chatID>，channelID 本身以 ch_ 开头
			rest := sr.ID[3:] // 去掉前缀 "ch_"
			for chID, chType := range channelTypes {
				if strings.HasPrefix(rest, chID+"_") {
					sr.Source = chType
					break
				}
			}
			if sr.Source == "web" {
				sr.Source = "channel" // 未知平台兜底
			}
		}
		list = append(list, sr)
	}
	if list == nil {
		list = []sessionRow{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"sessions": list})
}

// GET /v1/sessions/{sessionID}
func (h *ChatHandler) HandleGetSession(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("sessionID")
	maxChars := 50000
	if v := r.URL.Query().Get("max_chars"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxChars = n
		}
	}

	messages, err := h.ChatRepo.ListMessages(r.Context(), sessionID, 100000)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	type msgRow struct {
		Role         string          `json:"role"`
		Content      string          `json:"content"`
		ToolCalls    json.RawMessage `json:"tool_calls,omitempty"`
		TaskDuration int64           `json:"task_duration,omitempty"` // in ms
	}
	msgs := make([]msgRow, 0, len(messages))
	total := 0
	for _, row := range messages {
		m := msgRow{
			Role:         row.Role,
			Content:      row.Content,
			TaskDuration: parseTaskDuration(row.CreatedAt, row.UpdatedAt),
		}
		if row.ToolCalls != "" {
			m.ToolCalls = json.RawMessage(row.ToolCalls)
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

func parseTaskDuration(createdStr, updatedStr string) int64 {
	if createdStr == "" || updatedStr == "" {
		return 0
	}
	parseDBTime := func(s string) time.Time {
		if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
			return t
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t
		}
		return time.Time{}
	}
	tC := parseDBTime(createdStr)
	tU := parseDBTime(updatedStr)
	if !tU.IsZero() && !tC.IsZero() {
		return tU.Sub(tC).Milliseconds()
	}
	return 0
}

// DELETE /v1/sessions/{sessionID}
func (h *ChatHandler) HandleDeleteSession(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("sessionID")
	if err := h.ChatRepo.DeleteSession(r.Context(), sessionID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
}

// ─── 会话辅助方法 ────────────────────────────────────────────────────────────

func (h *ChatHandler) EnsureSession(ctx context.Context, sessionID string) error {
	err := h.ChatRepo.CreateSession(ctx, types.ChatSessionRow{ID: sessionID, Title: ""})
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "Server.ensureSession", err)
	}
	return nil
}

// loadMessages 从数据库加载会话历史（role + content 纯文本）。
// 【架构约束】图片/视频等多模态 Parts 不落盘，仅存在于当轮请求的内存中。
// 这意味着多轮视觉对话中，历史轮次的图片不会随上下文一并重传给大模型。
// 如需多轮图片记忆，需要在 saveMessage 中序列化 Parts 并在此处反序列化还原。
func (h *ChatHandler) LoadMessages(ctx context.Context, sessionID string) ([]types.Message, error) {
	rows, err := h.DB.QueryContext(ctx,
		`SELECT role, content FROM chat_messages WHERE session_id=? ORDER BY id`, sessionID)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "Server.loadMessages", err)
	}
	defer rows.Close()

	var msgs []types.Message
	for rows.Next() {
		var m types.Message
		if err := rows.Scan(&m.Role, &m.Content); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "Server.loadMessages", err)
		}
		msgs = append(msgs, m)
	}
	return msgs, nil
}

func (h *ChatHandler) SaveMessage(ctx context.Context, sessionID, role, content string, toolCalls string, durationMs int64) error {
	row := types.ChatMessageRow{
		SessionID: sessionID,
		Role:      role,
		Content:   content,
		ToolCalls: toolCalls,
	}
	if durationMs > 0 {
		now := time.Now().UTC()
		row.UpdatedAt = now.Format(time.RFC3339)
		row.CreatedAt = now.Add(-time.Duration(durationMs) * time.Millisecond).Format(time.RFC3339)
	}
	if err := h.ChatRepo.AppendMessage(ctx, row); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "Server.saveMessage", err)
	}
	return nil
}

// updateSessionTitle 把首条用户消息截断为会话标题（仅在 title 为空时写入）。
func (h *ChatHandler) UpdateSessionTitle(ctx context.Context, sessionID, firstInput string) error {
	title := firstInput
	if len([]rune(title)) > 40 {
		runes := []rune(title)
		title = string(runes[:40]) + "…"
	}
	err := h.ChatRepo.UpdateSessionTitle(ctx, sessionID, title)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "Server.updateSessionTitle", err)
	}
	return nil
}

// touchSession 更新 updated_at（每次对话后调用）。
func (h *ChatHandler) TouchSession(ctx context.Context, sessionID string) error {
	tctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.ChatRepo.TouchSession(tctx, sessionID); err != nil {
		slog.Warn("server: failed to touch session", "session", sessionID, "err", err)
		return apperr.Wrap(apperr.CodeInternal, "ChatHandler.TouchSession", err)
	}
	return nil
}

// newSessionID 生成 16 字节随机 hex ID。
// 熵池耗尽时降级为纳秒时间戳，确保唯一性（不使用固定零值，防止 session 碰撞）。
func newSessionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("sess_%d", time.Now().UnixNano())
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
func (h *ChatHandler) HandleSearch(w http.ResponseWriter, r *http.Request) {
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
	messages, err := h.ChatRepo.SearchMessages(r.Context(), q, 100)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

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

	for _, msg := range messages {
		sessID := msg.SessionID
		sr, ok := bySession[sessID]
		if !ok {
			sess, err := h.ChatRepo.GetSession(r.Context(), sessID)
			if err != nil {
				continue
			}
			sr = &sessionResult{
				SessionID: sessID,
				Title:     sess.Title,
				UpdatedAt: sess.UpdatedAt,
			}
			bySession[sessID] = sr
			ordered = append(ordered, sessID)
		}
		if len(sr.Matches) < 3 {
			snip := msg.Content
			if len(snip) > 100 {
				snip = snip[:100] + "…"
			}
			sr.Matches = append(sr.Matches, matchRow{
				Role:    msg.Role,
				Snippet: snip,
				Content: truncate(msg.Content, 300),
			})
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

// GET /v1/sessions/{sessionID}/context
// 返回会话上下文使用统计：当前 token 数、阈值、使用率、最近压缩时间。
func (h *ChatHandler) HandleGetSessionContext(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("sessionID")
	if sessionID == "" {
		http.Error(w, "sessionID required", http.StatusBadRequest)
		return
	}

	history, err := h.LoadMessages(r.Context(), sessionID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	stats := h.Compressor.Stats(history)

	var lastCompactAt *time.Time
	if !stats.LastCompactAt.IsZero() {
		t := stats.LastCompactAt
		lastCompactAt = &t
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"session_id":      sessionID,
		"token_count":     stats.TokenCount,
		"threshold":       stats.Threshold,
		"usage_percent":   float64(int(stats.UsagePercent*10)) / 10.0,
		"last_compact_at": lastCompactAt,
		"message_count":   stats.MessageCount,
	})
}
