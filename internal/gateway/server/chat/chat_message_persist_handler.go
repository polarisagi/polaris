package chat

import (
	"context"
	"encoding/json"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ChatMessagePersistHandler 是 TopicChatMessagePersistRetry 的 outbox 消费端
// （GD-13-004 复核修复）。SaveMessage 同步直写 chat_messages 重试耗尽后，会把
// 完整行内容投递到此主题；OutboxWorker 按 at-least-once 语义异步重试调用
// Handle，直到 AppendMessageIdempotent 成功或达到 maxRetries 被标记 dead
// （dead 后仍可从 last_error/payload 人工排查恢复，不会静默丢失原始内容——
// payload 本身就是完整的消息行 JSON）。
//
// 与"messages 表投影自 EventLog"式的全量 CQRS 重写相比，这是刻意收敛后的
// 最小充分修复：episodic memory 记录的是 Agent 认知/任务状态（perceive/plan/
// reflect 结构化产物），与 chat_messages 记录的用户可见对话文本是两类不同的
// 数据，把二者合并成同一份投影源会引入新的不一致（例如 reflect 阶段内容与
// 实际流式回复文本并非同一段文字），而非消除不一致。SaveMessage 目前是
// 4 个独立生产方（sse.go 交互式对话 / channelsadmin webhook / cronadmin /
// workflowadmin）共用的持久化入口，把它整体改造成纯异步投影会让这些同步
// 调用方失去"写入即可见"的保证，且它们并不存在 sse.go 特有的客户端断连竞态。
// 因此选择加固 SaveMessage 本身（重试 + outbox 兜底），而不是替换其调用方。
type ChatMessagePersistHandler struct {
	chatRepo protocol.ChatRepository
}

// NewChatMessagePersistHandler 构造处理器。
func NewChatMessagePersistHandler(chatRepo protocol.ChatRepository) *ChatMessagePersistHandler {
	return &ChatMessagePersistHandler{chatRepo: chatRepo}
}

// chatMessagePersistPayload 是投递到 outbox 的完整消息行快照（JSON 序列化）。
type chatMessagePersistPayload struct {
	SessionID        string `json:"session_id"`
	Role             string `json:"role"`
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content"`
	ToolCalls        string `json:"tool_calls"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
	DedupeKey        string `json:"dedupe_key"`
}

// Handle 实现 store.OutboxHandler 接口签名（结构化类型匹配，注册处见
// cmd/polaris bootstrap，避免 chat 包对 internal/store 之外再引入 OutboxWorker
// 具体实现）。
func (h *ChatMessagePersistHandler) Handle(ctx context.Context, record *store.OutboxRecord) error {
	var p chatMessagePersistPayload
	if err := json.Unmarshal(record.Payload, &p); err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "chat_message_persist: parse payload failed", err)
	}
	if p.DedupeKey == "" {
		// 不应发生（enqueue 侧总是填充），但 fail-closed：没有幂等键就不能
		// 安全地在 at-least-once 重投下写入，宁可标记失败等待人工排查。
		return apperr.New(apperr.CodeInvalidInput, "chat_message_persist: missing dedupe_key in payload")
	}

	row := types.ChatMessageRow{
		SessionID:        p.SessionID,
		Role:             p.Role,
		Content:          p.Content,
		ReasoningContent: p.ReasoningContent,
		ToolCalls:        p.ToolCalls,
		CreatedAt:        p.CreatedAt,
		UpdatedAt:        p.UpdatedAt,
		DedupeKey:        p.DedupeKey,
	}
	if err := h.chatRepo.AppendMessageIdempotent(ctx, row); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "chat_message_persist: append idempotent failed", err)
	}
	return nil
}
