package consolidation

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/polarisagi/polaris/pkg/types"
)

// PerMessageExtractor 在每条 episodic 事件写入后立即执行轻量级实体提取。
// 限制：仅对 event_type IN ('observation', 'tool_call', 'reflection') 执行，
//
//	跳过 'state_transition'（状态机信号，无信息价值）。
//
// 与 ConsolidationPipeline 的区别：
//   - 粒度：单条消息 vs 整个 session
//   - 时延：准实时（事件写入后秒级）vs 会话结束后分钟级
//   - 提取深度：轻量（单条） vs 深度（跨事件关系推断）
type PerMessageExtractor struct {
	pipeline *ConsolidationPipeline
}

// NewPerMessageExtractor 创建每条消息提取器。
func NewPerMessageExtractor(pipeline *ConsolidationPipeline) *PerMessageExtractor {
	return &PerMessageExtractor{pipeline: pipeline}
}

// shouldExtract 判断是否值得对此 event_type 执行提取。
func shouldExtract(eventType string) bool {
	switch eventType {
	case "observation", "tool_call", "reflection":
		return true
	}
	return false
}

// HandleOutboxRecord 处理 OutboxWorker 分发的 episodic_extract 事件。
// payload: {"session_id":"...","event_type":"...","content":"..."}
func (pe *PerMessageExtractor) HandleOutboxRecord(ctx context.Context, payload []byte) error {
	if pe.pipeline == nil {
		return nil
	}

	var msg struct {
		SessionID string `json:"session_id"`
		EventType string `json:"event_type"`
		Content   string `json:"content"`
	}
	if err := json.Unmarshal(payload, &msg); err != nil {
		return nil //nolint:nilerr // malformed payload 跳过
	}
	if !shouldExtract(msg.EventType) || msg.Content == "" {
		return nil
	}

	// 构造单条 ScoredEvent
	events := []types.ScoredEvent{
		{
			Event: types.Event{
				Type:    types.EventType(msg.EventType),
				Payload: []byte(msg.Content),
			},
			Score: 1.0,
		},
	}

	entities, relations, err := pe.pipeline.extractEntitiesAndRelations(ctx, msg.SessionID, events)
	if err != nil || (len(entities) == 0 && len(relations) == 0) {
		return nil //nolint:nilerr // 提取失败非致命
	}

	if err := pe.pipeline.upsertSemantic(ctx, entities, relations, types.TaintMedium); err != nil {
		slog.Warn("per_message_extractor: upsert failed", "session_id", msg.SessionID, "err", err)
	}

	return nil
}
