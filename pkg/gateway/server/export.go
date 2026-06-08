package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ── Trajectory 导出（自演化训练数据生成）─────────────────────────────────────
//
// GET /v1/export/trajectories?format=sharegpt&days=30&min_turns=2&session_id=xxx
//
// 将 chat_messages 中的高质量对话导出为标准训练数据格式，供 M09 自演化或
// 本地模型（1B~7B）的 SFT/DPO 微调使用。
//
// 支持格式：
//   - sharegpt   ShareGPT JSONL（HuggingFace 主流 SFT 格式）
//   - openai     OpenAI fine-tuning JSONL
//   - raw        原始对话 JSONL（默认）

// shareGPTTurn ShareGPT 格式单轮消息
type shareGPTTurn struct {
	From  string `json:"from"` // "human" | "gpt"
	Value string `json:"value"`
}

// shareGPTConv ShareGPT 格式单个对话
type shareGPTConv struct {
	ID            string         `json:"id"`
	Conversations []shareGPTTurn `json:"conversations"`
}

// openAIMessage OpenAI fine-tuning 格式消息
type openAIMessage struct {
	Role    string `json:"role"` // "system" | "user" | "assistant"
	Content string `json:"content"`
}

// openAIConv OpenAI fine-tuning 格式单个对话
type openAIConv struct {
	Messages []openAIMessage `json:"messages"`
}

// rawConv 原始格式对话
type rawConv struct {
	SessionID  string `json:"session_id"`
	Title      string `json:"title"`
	ExportedAt string `json:"exported_at"`
	Messages   []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

func (s *Server) handleExportTrajectories(w http.ResponseWriter, r *http.Request) { //nolint:gocyclo
	ctx := r.Context()
	format := strings.ToLower(r.URL.Query().Get("format"))
	if format == "" {
		format = "raw"
	}
	days := 30
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 3650 {
			days = n
		}
	}
	minTurns := 2
	if v := r.URL.Query().Get("min_turns"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			minTurns = n
		}
	}
	filterSession := r.URL.Query().Get("session_id")

	// ── 查询符合条件的会话 ─────────────────────────────────────────────
	query := `
		SELECT cs.id, COALESCE(cs.title,''), COUNT(cm.id) AS turns
		FROM   chat_sessions cs
		JOIN   chat_messages cm ON cm.session_id = cs.id AND cm.role IN ('user','assistant')
		WHERE  cs.created_at >= strftime('%Y-%m-%dT%H:%M:%SZ','now', ? || ' days')`
	args := []any{strconv.Itoa(-days)}
	if filterSession != "" {
		query += " AND cs.id = ?"
		args = append(args, filterSession)
	}
	query += " GROUP BY cs.id HAVING turns >= ? ORDER BY cs.updated_at DESC LIMIT 500"
	args = append(args, minTurns)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type sessionMeta struct {
		id, title string
		turns     int
	}
	var sessions []sessionMeta
	for rows.Next() {
		var sm sessionMeta
		if rows.Scan(&sm.id, &sm.title, &sm.turns) == nil {
			sessions = append(sessions, sm)
		}
	}
	rows.Close()

	// ── 设置响应头（JSONL 流式输出）──────────────────────────────────
	w.Header().Set("Content-Type", "application/jsonl")
	w.Header().Set("Content-Disposition", `attachment; filename="trajectories.jsonl"`)

	enc := json.NewEncoder(w)
	exported := 0

	for _, sm := range sessions {
		msgRows, err := s.db.QueryContext(ctx,
			`SELECT role, content FROM chat_messages
			 WHERE session_id=? AND role IN ('user','assistant')
			 ORDER BY id`, sm.id)
		if err != nil {
			continue
		}

		type msg struct{ role, content string }
		var msgs []msg
		for msgRows.Next() {
			var m msg
			if msgRows.Scan(&m.role, &m.content) == nil {
				// 跳过压缩摘要消息（非训练数据）
				if m.role == "assistant" && strings.HasPrefix(m.content, "[上下文压缩摘要") {
					continue
				}
				msgs = append(msgs, m)
			}
		}
		msgRows.Close()

		if len(msgs) < minTurns*2 {
			continue
		}

		switch format {
		case "sharegpt":
			conv := shareGPTConv{ID: sm.id}
			for _, m := range msgs {
				from := "gpt"
				if m.role == "user" {
					from = "human"
				}
				conv.Conversations = append(conv.Conversations, shareGPTTurn{From: from, Value: m.content})
			}
			enc.Encode(conv) //nolint:errcheck

		case "openai":
			conv := openAIConv{}
			for _, m := range msgs {
				role := m.role
				if role == "assistant" {
					_ = role // OpenAI SFT 格式 assistant 角色不变
				}
				conv.Messages = append(conv.Messages, openAIMessage{Role: role, Content: m.content})
			}
			enc.Encode(conv) //nolint:errcheck

		default: // raw
			conv := rawConv{
				SessionID:  sm.id,
				Title:      sm.title,
				ExportedAt: time.Now().UTC().Format(time.RFC3339),
			}
			for _, m := range msgs {
				conv.Messages = append(conv.Messages, struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				}{m.role, m.content})
			}
			enc.Encode(conv) //nolint:errcheck
		}
		exported++
	}

	// 若无数据，写空行避免空响应
	if exported == 0 {
		enc.Encode(map[string]any{"exported": 0, "message": "no qualifying sessions found"}) //nolint:errcheck
	}
}
