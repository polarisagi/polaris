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
