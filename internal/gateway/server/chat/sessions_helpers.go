package chat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// saveMessageRetryAttempts：SaveMessage 同步直写 chat_messages 失败时的有限
// 重试次数（GD-13-004 复核修复）。SQLite 单写者 + busy_timeout=5000ms 下绝
// 大多数瞬时锁争用已在驱动层被吸收，这里的重试面向剩余的极端瞬时故障（磁盘
// 短暂繁忙等）；重试耗尽后转 outbox 异步兜底，不在请求路径无限阻塞。
const saveMessageRetryAttempts = 3

// saveMessageRetryBackoff 返回第 attempt（0-based）次重试前的等待时长。
func saveMessageRetryBackoff(attempt int) time.Duration {
	backoffs := [saveMessageRetryAttempts]time.Duration{
		50 * time.Millisecond, 150 * time.Millisecond, 450 * time.Millisecond,
	}
	if attempt < 0 || attempt >= len(backoffs) {
		return backoffs[len(backoffs)-1]
	}
	return backoffs[attempt]
}

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

// SaveMessage 持久化一条会话消息（用户输入或 assistant 回复）。
//
// GD-13-004 复核修复：原实现同步写入失败时只记录日志、直接放弃——单次写入
// 失败即意味着一轮已经流式展示给用户的回复从聊天历史永久消失（下次加载
// 该会话时该轮凭空缺失），却没有任何重试或补救。现在加固为：
//  1. 有限次数重试（saveMessageRetryAttempts，覆盖瞬时故障，SQLite 单写者
//     busy_timeout 已吸收大部分锁争用，这里兜底剩余极端情况）；
//  2. 重试仍失败时，若注入了 OutboxWriter，投递到 TopicChatMessagePersistRetry
//     做异步兜底重投（ChatMessagePersistHandler 消费），不阻塞请求路径；
//  3. 每条消息生成稳定 dedupe_key，供 outbox at-least-once 重投时
//     AppendMessageIdempotent 去重，避免重复插入。
//
// 未注入 OutboxWriter 时（测试/未接入 outbox 的最小部署）行为与修复前一致：
// 仅记录错误日志。
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
		DedupeKey:        newMessageDedupeKey(sessionID, role),
	}
	if durationMs > 0 {
		row.UpdatedAt = now.Format(time.RFC3339)
		row.CreatedAt = createdAt
	}

	var lastErr error
retryLoop:
	for attempt := range saveMessageRetryAttempts {
		if attempt > 0 {
			select {
			case <-time.After(saveMessageRetryBackoff(attempt - 1)):
			case <-ctx.Done():
				lastErr = ctx.Err()
				break retryLoop
			}
		}
		if err := h.ChatRepo.AppendMessage(ctx, row); err != nil {
			lastErr = err
			slog.Warn("server: saveMessage attempt failed, will retry", "session", sessionID, "role", role, "attempt", attempt+1, "err", err)
			continue
		}
		return nil
	}

	// 同步直写重试耗尽：尝试 outbox 异步兜底，避免这轮消息彻底消失。
	if h.OutboxWriter != nil {
		payload := chatMessagePersistPayload{
			SessionID:        row.SessionID,
			Role:             row.Role,
			Content:          row.Content,
			ReasoningContent: row.ReasoningContent,
			ToolCalls:        row.ToolCalls,
			CreatedAt:        row.CreatedAt,
			UpdatedAt:        row.UpdatedAt,
			DedupeKey:        row.DedupeKey,
		}
		payloadBytes, marshalErr := json.Marshal(payload)
		if marshalErr == nil {
			entry := protocol.OutboxEntry{
				TargetEngine:   protocol.TopicChatMessagePersistRetry,
				Operation:      "insert",
				Payload:        payloadBytes,
				IdempotencyKey: "chat_message_persist:" + row.DedupeKey,
			}
			obCtx, obCancel := context.WithTimeout(context.Background(), 3*time.Second)
			writeErr := h.OutboxWriter.Write(obCtx, entry)
			obCancel()
			if writeErr == nil {
				slog.Warn("server: saveMessage direct write failed, enqueued outbox fallback", "session", sessionID, "role", role, "err", lastErr)
				return nil
			}
			slog.Error("server: saveMessage outbox fallback enqueue failed", "session", sessionID, "role", role, "err", writeErr)
		} else {
			slog.Error("server: saveMessage outbox fallback payload marshal failed", "session", sessionID, "role", role, "err", marshalErr)
		}
	}

	return apperr.Wrap(apperr.CodeInternal, "Server.saveMessage", lastErr)
}

// newMessageDedupeKey 生成消息幂等键：会话/角色前缀便于人工排查 + 随机后缀
// 保证唯一性（熵池耗尽时降级为纳秒时间戳，与 newSessionID 一致的降级策略）。
func newMessageDedupeKey(sessionID, role string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%s:%s:%d", sessionID, role, time.Now().UnixNano())
	}
	return fmt.Sprintf("%s:%s:%s", sessionID, role, hex.EncodeToString(b))
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
