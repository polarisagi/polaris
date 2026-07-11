package chat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// 会话辅助方法：创建/加载/保存消息、标题更新、心跳、ID 生成（R7 拆分自
// sessions.go）。CRUD HTTP 处理器见 sessions.go，全文搜索见 sessions_search.go。
// ============================================================================

func (h *ChatHandler) EnsureSession(ctx context.Context, sessionID string) error {
	err := h.ChatRepo.CreateSession(ctx, types.ChatSessionRow{ID: sessionID, Title: ""})
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "Server.ensureSession", err)
	}
	return nil
}

// ListMessages 从数据库加载会话历史（role + content 纯文本）。
// 【架构约束】图片/视频等多模态 Parts 不落盘，仅存在于当轮请求的内存中。
// 这意味着多轮视觉对话中，历史轮次的图片不会随上下文一并重传给大模型。
// 如需多轮图片记忆，需要在 saveMessage 中序列化 Parts 并在此处反序列化还原。
func (h *ChatHandler) ListMessages(ctx context.Context, sessionID string) ([]types.Message, error) {
	rows, err := h.ChatRepo.ListMessages(ctx, sessionID, 0)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "Server.loadMessages", err)
	}

	var msgs []types.Message
	for _, r := range rows {
		msgs = append(msgs, types.Message{
			Role:             r.Role,
			Content:          r.Content,
			ReasoningContent: r.ReasoningContent,
		})
	}
	return msgs, nil
}

func (h *ChatHandler) SaveMessage(ctx context.Context, sessionID, role, content string, toolCalls string, reasoningContent string, durationMs int64) error {
	now := time.Now().UTC()
	createdAt := now.Format(time.RFC3339)
	if durationMs > 0 {
		createdAt = now.Add(-time.Duration(durationMs) * time.Millisecond).Format(time.RFC3339)
	}

	row := types.ChatMessageRow{
		SessionID:        sessionID,
		Role:             role,
		Content:          content,
		ReasoningContent: reasoningContent,
		ToolCalls:        toolCalls,
	}
	if durationMs > 0 {
		row.UpdatedAt = now.Format(time.RFC3339)
		row.CreatedAt = createdAt
	}
	if err := h.ChatRepo.AppendMessage(ctx, row); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "Server.saveMessage", err)
	}
	return nil
}

// UpdateSessionTitle 把首条用户消息截断为会话标题（仅在 title 为空时写入）。
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

// TouchSession 更新 updated_at（每次对话后调用）。
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
