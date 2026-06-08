package memory

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/polarisagi/polaris/internal/protocol"
)

// EpisodicGraphIndexer 在 episodic_events 写入时同步建立 SurrealDB 图谱边，
// 打通 episodic↔knowledge 两个孤岛，使 HybridRetrieverImpl 的图遍历路径
// 可从知识实体跨越到记忆事件（AriGraph arXiv:2407.04363 §3）。
//
// 建立边:
//   - episodic:{id} → TRIGGERED_BY → agent:{agentID}   (事件来源追溯)
//   - episodic:{id} → IN_SESSION   → session:{taskID}  (会话聚类检索)
//   - episodic:{id} → ACTION_DONE  → entity:tool:unknown (工具调用关联，payload 无法解析时降级)
type EpisodicGraphIndexer struct {
	graph GraphTraverser
}

func NewEpisodicGraphIndexer(graph GraphTraverser) *EpisodicGraphIndexer {
	return &EpisodicGraphIndexer{graph: graph}
}

// Index 为 episodic 事件在图中建立关联边（best-effort：失败仅记日志，不阻断写路径）。
func (ei *EpisodicGraphIndexer) Index(_ context.Context, ev protocol.Event) {
	node := "episodic:" + ev.ID

	if ev.AgentID != "" {
		if err := ei.graph.GraphRelate(node, "TRIGGERED_BY", "agent:"+ev.AgentID); err != nil {
			slog.Warn("episodic_graph: TRIGGERED_BY 边写入失败", "event", ev.ID, "err", err)
		}
	}
	if ev.TaskID != "" && ev.TaskID != ev.ID {
		if err := ei.graph.GraphRelate(node, "IN_SESSION", "session:"+ev.TaskID); err != nil {
			slog.Warn("episodic_graph: IN_SESSION 边写入失败", "event", ev.ID, "err", err)
		}
	}
	if ev.Type == protocol.EventActionDone {
		toolName := extractToolName(ev.Payload)
		if err := ei.graph.GraphRelate(node, "ACTION_DONE", "entity:tool:"+toolName); err != nil {
			slog.Warn("episodic_graph: ACTION_DONE 边写入失败", "event", ev.ID, "err", err)
		}
	}
}

func extractToolName(payload []byte) string {
	if len(payload) == 0 {
		return "unknown"
	}
	var payloadMap map[string]any
	if err := json.Unmarshal(payload, &payloadMap); err != nil {
		return "unknown"
	}
	if name, ok := payloadMap["tool_name"].(string); ok && name != "" {
		return name
	}
	return "unknown"
}
