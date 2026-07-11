package chat

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
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
		Role             string          `json:"role"`
		Content          string          `json:"content"`
		ReasoningContent string          `json:"reasoning_content,omitempty"`
		ToolCalls        json.RawMessage `json:"tool_calls,omitempty"`
		TaskDuration     int64           `json:"task_duration,omitempty"` // in ms
	}
	msgs := make([]msgRow, 0, len(messages))
	total := 0
	for _, row := range messages {
		m := msgRow{
			Role:             row.Role,
			Content:          row.Content,
			ReasoningContent: row.ReasoningContent,
			TaskDuration:     parseTaskDuration(row.CreatedAt, row.UpdatedAt),
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

// 会话辅助方法（EnsureSession/ListMessages/SaveMessage/UpdateSessionTitle/
// TouchSession/newSessionID/truncate）见 sessions_helpers.go；全文搜索与会话
// 上下文统计处理器见 sessions_search.go（均为 R7 拆分）。
