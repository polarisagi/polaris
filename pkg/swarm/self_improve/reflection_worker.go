package self_improve

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
)

// ============================================================================
// ReflectionWorker
// 架构文档: docs/arch/M05-Memory-System.md §3.4
// ============================================================================

// ReflectionWorker 后台协程，负责在任务结束或 Session 关闭时提取 Agent 反思记忆。
type ReflectionWorker struct {
	episodic   protocol.EpisodicMemory
	provider   protocol.Provider
	reflection protocol.ReflectionMemory
}

func NewReflectionWorker(episodic protocol.EpisodicMemory, provider protocol.Provider, reflection protocol.ReflectionMemory) *ReflectionWorker {
	return &ReflectionWorker{
		episodic:   episodic,
		provider:   provider,
		reflection: reflection,
	}
}

// ConsolidateReflections 在任务终态触发。
func (rw *ReflectionWorker) ConsolidateReflections(ctx context.Context, taskID string, taskType string, replanCount int) error {
	// 如果不是需要深度反思的复杂任务类型，跳过
	if taskType != "complex_reasoning" && taskType != "coding" && replanCount < 2 {
		return nil
	}

	// 1. 收集 Evidence Episodic Events
	events, err := rw.episodic.Query(ctx, protocol.EpisodicQuery{
		SessionID: taskID,
		K:         100,
	})
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "reflection_worker: query episodic events", err)
	}
	if len(events) == 0 {
		return nil
	}

	// 2. LLM 提取反思
	var sb strings.Builder
	eventIDs := make([]string, 0, len(events))
	for _, se := range events {
		eventIDs = append(eventIDs, se.Event.ID)
		sb.WriteString(string(se.Event.Type))
		sb.WriteString(": ")
		sb.WriteString(string(se.Event.Payload))
		sb.WriteByte('\n')
	}

	prompt := fmt.Sprintf(
		"Extract a metacognitive reflection from the following AI agent session. Focus on 'success_pattern', 'failure_mode', 'efficiency_insight', or 'cross_task_principle'.\n"+
			"Output strictly JSON:\n"+
			"{\n"+
			"  \"reflection_type\": \"...\",\n"+
			"  \"content\": \"... (max 500 tokens)\"\n"+
			"}\n\nSession Logs:\n%s",
		sb.String(),
	)

	resp, err := rw.provider.Infer(ctx, &protocol.InferRequest{
		Messages:        []protocol.Message{{Role: "user", Content: prompt}},
		MaxTokens:       512,
		Temperature:     0.2,
		ReasoningEffort: protocol.ReasoningEffortLow,
	})
	if err != nil {
		return err
	}

	// 3. 解析结果
	content := strings.TrimSpace(resp.Content)
	if idx := strings.Index(content, "{"); idx > 0 {
		content = content[idx:]
	}
	if idx := strings.LastIndex(content, "}"); idx >= 0 && idx < len(content)-1 {
		content = content[:idx+1]
	}

	var res struct {
		ReflectionType string `json:"reflection_type"`
		Content        string `json:"content"`
	}
	if err := json.Unmarshal([]byte(content), &res); err != nil {
		return err
	}

	if res.Content == "" {
		return nil
	}

	// 4. M11 FactualityGuard 抽样核验 (这里简化为直接写入)
	// 5. 写入 ReflectionMemory
	entry := protocol.ReflectionEntry{
		ID:         fmt.Sprintf("ref_%d", time.Now().UnixNano()),
		SessionID:  taskID,
		FailReason: "N/A",
		Strategy:   res.ReflectionType,
		Decision:   res.Content,
		Meta: map[string]any{
			"task_type":          taskType,
			"evidence_event_ids": eventIDs,
			"salience":           0.8,
			"taint_level":        protocol.TaintLow,
		},
		CreatedAt: time.Now(),
	}

	return rw.reflection.AppendReflection(ctx, entry)
}
