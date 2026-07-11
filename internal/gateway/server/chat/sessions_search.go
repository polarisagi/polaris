package chat

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/httputil"
)

// ============================================================================
// 全文搜索 + 会话上下文统计 HTTP 处理器（R7 拆分自 sessions.go）。
// 会话 CRUD 处理器见 sessions.go，辅助方法见 sessions_helpers.go。
// ============================================================================

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
		httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
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

	history, err := h.ListMessages(r.Context(), sessionID)
	if err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
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
