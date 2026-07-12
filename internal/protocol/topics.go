package protocol

import (
	"context"
	"encoding/json"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// TopicXxx Outbox 事件主题。OutboxWorker 按 OutboxEntry.TargetEngine 路由，
// 生产者与 RegisterHandler 必须引用同一常量，禁止字符串字面量。
const (
	TopicEpisodicProject    = "episodic"
	TopicEpisodicExtract    = "episodic_extract"
	TopicMemoryConsolidate  = "memory_consolidate"
	TopicSemanticCompress   = "semantic_compress"
	TopicExtensionLibrarian = "extension_librarian"
	TopicProviderRecovered  = "m1_provider_recovered"
	TopicCapabilityGap      = "m9_capability_gap"
	TopicLogicCollapse      = "m9_logic_collapse"
	TopicAgentInterrupt     = "agent_interrupt"
	// TopicNotification 后台/自动化任务终态通知投递（GD-13-001 最小实现），
	// 消费端见 internal/automation/notify.Dispatcher。
	TopicNotification = "notification"

	// TopicChatMessagePersistRetry 聊天消息持久化的 outbox 兜底重试通道
	// （GD-13-004 复核修复）。ChatHandler.SaveMessage 的同步直写重试耗尽后，
	// 投递到此主题异步兜底，避免瞬时/持久写入故障导致 chat_messages 永久丢失
	// 一轮已经流式展示给用户的回复。消费端见
	// internal/gateway/server/chat.ChatMessagePersistHandler。
	TopicChatMessagePersistRetry = "chat_message_persist_retry"

	// 沿用 RAG 主题
	TopicGraphBuild = "graph_build"
)

// NewOutboxEvent 构造一个 OutboxEntry，强制使用主题常量。
// 避免手写字面量导致的拼写错误或约定不一致。
func NewOutboxEvent(topic, op string, payload any, idemKey string) (OutboxEntry, error) {
	if topic == "" {
		return OutboxEntry{}, apperr.New(apperr.CodeInvalidInput, "outbox topic cannot be empty")
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return OutboxEntry{}, apperr.Wrap(apperr.CodeInternal, "failed to marshal outbox payload", err)
	}

	return OutboxEntry{
		TargetEngine:   topic,
		Operation:      op,
		Payload:        payloadBytes,
		IdempotencyKey: idemKey,
	}, nil
}

// InferOptions 控制 LLM 推理行为。
type InferOptions struct {
	Temperature float32
	MaxTokens   int
	StopWords   []string
}

// ModelResponse 封装 LLM 响应。
type ModelResponse struct {
	Content string
	Tokens  int
}

// ContextPredictFunc 提供上下文连续性预测能力。
type ContextPredictFunc func(ctx context.Context, events []types.Event) (float64, error)
