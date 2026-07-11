package chat

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/polarisagi/polaris/internal/gateway/httputil"
)

// GET /v1/sessions/{sessionID}/recap
// 对当前会话做纯本地摘要：统计消息数、工具调用类型、最近用户 prompt 预览。
// 零 LLM 调用，响应 < 5ms。设计来源：hermes-agent session_recap.py。
func (h *ChatHandler) HandleSessionRecap(w http.ResponseWriter, r *http.Request) { //nolint:gocyclo
	sessionID := r.PathValue("sessionID")
	ctx := r.Context()

	rawMsgs, err := h.ChatRepo.ListMessages(ctx, sessionID, 0)
	if err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
		return
	}

	type msgRow struct {
		role, content, createdAt string
	}
	msgs := make([]msgRow, 0, len(rawMsgs))
	for _, m := range rawMsgs {
		msgs = append(msgs, msgRow{role: m.Role, content: m.Content, createdAt: m.CreatedAt})
	}

	if len(msgs) == 0 {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"session_id":    sessionID,
			"message_count": 0,
			"summary":       "会话尚无消息",
		})
		return
	}

	// 统计基本指标
	userCount, assistantCount := 0, 0
	var firstAt, lastAt string
	var recentUserPrompts []string

	for i, m := range msgs {
		if i == 0 {
			firstAt = m.createdAt
		}
		lastAt = m.createdAt
		switch m.role {
		case "user":
			userCount++
			// 收集最近 3 条用户消息预览（后 20 条窗口）
			if i >= len(msgs)-20 {
				preview := m.content
				if len([]rune(preview)) > 80 {
					runes := []rune(preview)
					preview = string(runes[:80]) + "…"
				}
				recentUserPrompts = append(recentUserPrompts, preview)
			}
		case "assistant":
			assistantCount++
		}
	}

	// 只保留最近 3 条用户 prompt
	if len(recentUserPrompts) > 3 {
		recentUserPrompts = recentUserPrompts[len(recentUserPrompts)-3:]
	}

	// 最新 assistant 回复预览（最多 200 字符）
	lastReply := ""
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].role == "assistant" {
			content := msgs[i].content
			// 跳过压缩摘要消息
			if strings.HasPrefix(content, "[上下文压缩摘要") {
				continue
			}
			if len([]rune(content)) > 120 {
				runes := []rune(content)
				content = string(runes[:120]) + "…"
			}
			lastReply = content
			break
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"session_id":          sessionID,
		"message_count":       len(msgs),
		"user_messages":       userCount,
		"assistant_messages":  assistantCount,
		"started_at":          firstAt,
		"last_active_at":      lastAt,
		"recent_user_prompts": recentUserPrompts,
		"last_reply_preview":  lastReply,
	})
}
