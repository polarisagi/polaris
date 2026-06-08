package server

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// GET /v1/insights?days=30
// 聚合对话历史用量统计：会话数、消息数、每日活跃趋势、角色分布。
// 纯 SQLite 聚合，无 LLM 调用，响应 < 10ms。
func (s *Server) handleInsights(w http.ResponseWriter, r *http.Request) { //nolint:gocyclo
	days := 30
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 365 {
			days = n
		}
	}
	ctx := r.Context()

	// ── 全局汇总 ─────────────────────────────────────────────────────────────
	var totalSessions, totalMessages int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chat_sessions`).Scan(&totalSessions) //nolint:errcheck
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chat_messages`).Scan(&totalMessages) //nolint:errcheck

	// ── 统计周期内汇总 ────────────────────────────────────────────────────────
	var periodSessions, periodMessages int
	s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chat_sessions
		 WHERE created_at >= strftime('%Y-%m-%dT%H:%M:%SZ', 'now', ? || ' days')`,
		strconv.Itoa(-days)).Scan(&periodSessions) //nolint:errcheck
	s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chat_messages
		 WHERE created_at >= strftime('%Y-%m-%dT%H:%M:%SZ', 'now', ? || ' days')`,
		strconv.Itoa(-days)).Scan(&periodMessages) //nolint:errcheck

	// ── 角色分布 ──────────────────────────────────────────────────────────────
	roleRows, err := s.db.QueryContext(ctx,
		`SELECT role, COUNT(*) FROM chat_messages GROUP BY role`)
	roleBreakdown := map[string]int{}
	if err == nil {
		defer roleRows.Close()
		for roleRows.Next() {
			var role string
			var cnt int
			if roleRows.Scan(&role, &cnt) == nil {
				roleBreakdown[role] = cnt
			}
		}
	}

	// ── 每日消息趋势（最近 N 天）────────────────────────────────────────────
	trendRows, err := s.db.QueryContext(ctx,
		`SELECT strftime('%Y-%m-%d', created_at) AS day, COUNT(*) AS cnt
		 FROM chat_messages
		 WHERE created_at >= strftime('%Y-%m-%dT%H:%M:%SZ', 'now', ? || ' days')
		 GROUP BY day
		 ORDER BY day ASC`,
		strconv.Itoa(-days))
	type dayCount struct {
		Day   string `json:"day"`
		Count int    `json:"count"`
	}
	var trend []dayCount
	if err == nil {
		defer trendRows.Close()
		for trendRows.Next() {
			var dc dayCount
			if trendRows.Scan(&dc.Day, &dc.Count) == nil {
				trend = append(trend, dc)
			}
		}
	}
	if trend == nil {
		trend = []dayCount{}
	}

	// ── 最活跃会话（消息数 top 10）───────────────────────────────────────────
	topRows, err := s.db.QueryContext(ctx,
		`SELECT cs.id, cs.title, COUNT(cm.id) AS msg_cnt, cs.updated_at
		 FROM chat_sessions cs
		 JOIN chat_messages cm ON cm.session_id = cs.id
		 GROUP BY cs.id
		 ORDER BY msg_cnt DESC
		 LIMIT 10`)
	type topSession struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		MsgCount  int    `json:"message_count"`
		UpdatedAt string `json:"updated_at"`
	}
	var topSessions []topSession
	if err == nil {
		defer topRows.Close()
		for topRows.Next() {
			var ts topSession
			if topRows.Scan(&ts.ID, &ts.Title, &ts.MsgCount, &ts.UpdatedAt) == nil {
				topSessions = append(topSessions, ts)
			}
		}
	}
	if topSessions == nil {
		topSessions = []topSession{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"period_days":     days,
		"total_sessions":  totalSessions,
		"total_messages":  totalMessages,
		"period_sessions": periodSessions,
		"period_messages": periodMessages,
		"role_breakdown":  roleBreakdown,
		"daily_trend":     trend,
		"top_sessions":    topSessions,
	})
}
