package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
)

// CognitiveSearcher L2 语义检索接口（消费方定义，防止包循环）。
// 实现由上层注入（pkg/cognition 层调用方提供 SurrealDBCoreStore）。
type CognitiveSearcher interface {
	// FTSSearch BM25 全文检索，返回 top-k 结果（docID + snippet）。
	FTSSearch(query string, k int) ([]CogResult, error)
	// VecKNN 向量近邻检索，embedding 为查询向量，k 为返回数量。
	VecKNN(embedding []float32, k int) ([]CogResult, error)
}

// CogResult 单条语义检索结果。
type CogResult struct {
	DocID   string
	Score   float64
	Snippet string
}

// buildPerceiveContext 基于当前状态上下文（包含用户的原始任务描述/Intent）
// 从 EpisodicMemory、ReflectionMemory 与 WorkingMemory 组装感知阶段所需的 LLM 提示词。
// M05 §3.4: S_PERCEIVE 阶段拉取同 task_type 的 top-3 reflection 注入上下文。
func buildPerceiveContext( //nolint:gocyclo
	ctx context.Context, memory protocol.Memory, sCtx *StateContext, cognitive CognitiveSearcher) ([]protocol.Message, error) {
	if memory == nil {
		return []protocol.Message{
			{Role: "system", Content: "Structure the user intent into a TaskModel JSON."},
		}, nil
	}

	// 1. 查询相关的历史 Episodic 事件（如相似的任务意图）
	query := protocol.EpisodicQuery{
		Semantic: "agent task intent",
		K:        3,
	}
	events, err := memory.Episodic().Query(ctx, query)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "failed to query episodic memory", err)
	}

	var episodicCtx string
	if len(events) > 0 {
		episodicCtx = "Relevant Historical Episodic Memories:\n"
		for _, e := range events {
			episodicCtx += fmt.Sprintf("- [%s] %s: %s\n", e.Event.CreatedAt.Format(time.RFC3339), e.Event.Type, string(e.Event.Payload))
		}
	}

	// 2. 跨会话 Reflection 召回（M05 §3.4）：按目标主题词拉取历史经验注入上下文。
	// TaskModel 为 nil（首次感知）时跳过，replanning 路径可复用已有目标词。
	var reflectionCtx string
	if rm := memory.Reflection(); rm != nil && sCtx.TaskModel != nil && sCtx.TaskModel.Goal != "" {
		reflections, rerr := rm.QueryReflections(ctx, protocol.ReflectionQuery{
			Topic: sCtx.TaskModel.Goal,
			K:     3,
		})
		if rerr == nil && len(reflections) > 0 {
			reflectionCtx = "Cross-Session Reflections (past experience for similar tasks):\n"
			for _, r := range reflections {
				reflectionCtx += fmt.Sprintf("- [%s] %s: %s\n",
					r.CreatedAt.Format(time.RFC3339), r.Strategy, r.Decision)
			}
		}
	}

	// 3. 耳语线索注入（非阻塞，Memory Agent 推送的历史经验线索）
	var whisperCtx string
	if sCtx.WhisperChan != nil {
		select {
		case w := <-sCtx.WhisperChan:
			if w.Salience >= 0.5 { // 低显著度线索过滤
				whisperCtx = fmt.Sprintf("## Memory Whisper (source: %s)\n%s\n", w.Source, w.Content)
			}
		default:
			// 无线索，继续
		}
	}

	// 4. L2 语义记忆（SurrealDB FTSSearch，Memory Agent 蒸馏写入）
	var semanticCtx string
	if cognitive != nil && sCtx.TaskModel != nil && sCtx.TaskModel.Goal != "" {
		ftsResults, err := cognitive.FTSSearch(sCtx.TaskModel.Goal, 5)
		if err == nil && len(ftsResults) > 0 {
			var sb strings.Builder
			sb.WriteString("Semantic Memory (L2):\n")
			for _, r := range ftsResults {
				sb.WriteString(fmt.Sprintf("- [score=%.2f] %s\n", r.Score, r.Snippet))
			}
			semanticCtx = sb.String()
		}
	}

	// 5. 组装上下文
	baseContent := "Structure the user intent into a TaskModel JSON.\n\n"
	if whisperCtx != "" {
		baseContent += whisperCtx + "\n"
	}
	if len(sCtx.ReasoningState) > 0 {
		baseContent += "Reasoning State from the previous iteration:\n" + string(sCtx.ReasoningState) + "\n\n"
	}
	if reflectionCtx != "" {
		baseContent += reflectionCtx + "\n"
	}
	if episodicCtx != "" {
		baseContent += episodicCtx + "\n"
	}
	if semanticCtx != "" {
		baseContent += semanticCtx + "\n"
	}
	if sCtx.InstalledExtensionsInfo != "" {
		baseContent += sCtx.InstalledExtensionsInfo + "\n\n"
	}

	msgs := []protocol.Message{
		{Role: "system", Content: baseContent},
	}

	if wm := memory.Working(); wm != nil {
		msgs = wm.Immutable().PrependToMessages(msgs)
	}

	return msgs, nil
}

// buildPlanContext 基于已解析的 TaskModel 和可用工具列表
// 从 Memory 系统组装生成 DAG 计划所需的 LLM 提示词。
// tools 为 nil 时跳过工具注入（测试环境）。
func buildPlanContext( //nolint:gocyclo
	ctx context.Context, memory protocol.Memory, sCtx *StateContext, tools protocol.ToolRegistry, cognitive CognitiveSearcher) ([]protocol.Message, error) {
	if memory == nil {
		return []protocol.Message{
			{Role: "system", Content: "Generate an execution DAG based on the TaskModel."},
		}, nil
	}

	// 查询与任务目标或已有子任务相关的历史记忆
	var queryStr string
	if sCtx.TaskModel != nil {
		queryStr = sCtx.TaskModel.Goal
	}
	query := protocol.EpisodicQuery{
		Semantic: queryStr,
		K:        5,
	}
	events, err := memory.Episodic().Query(ctx, query)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "failed to query episodic memory", err)
	}

	var episodicCtx string
	if len(events) > 0 {
		episodicCtx = "Historical execution experiences for reference:\n"
		for _, e := range events {
			episodicCtx += fmt.Sprintf("- [%s] %s: %s\n", e.Event.CreatedAt.Format(time.RFC3339), e.Event.Type, string(e.Event.Payload))
		}
	}

	// 跨会话 Reflection 召回（M05 §3.4）：规划阶段注入成功/失败模式经验
	var reflectionCtx string
	if rm := memory.Reflection(); rm != nil && queryStr != "" {
		reflections, rerr := rm.QueryReflections(ctx, protocol.ReflectionQuery{
			Topic: queryStr,
			K:     3,
		})
		if rerr == nil && len(reflections) > 0 {
			reflectionCtx = "Cross-Session Reflections (execution patterns for similar tasks):\n"
			for _, r := range reflections {
				reflectionCtx += fmt.Sprintf("- [%s] %s: %s\n",
					r.CreatedAt.Format(time.RFC3339), r.Strategy, r.Decision)
			}
		}
	}

	// 4. L2 语义记忆（SurrealDB FTSSearch）
	var semanticCtx string
	if cognitive != nil && sCtx.TaskModel != nil && sCtx.TaskModel.Goal != "" {
		queryTopic := sCtx.TaskModel.Goal
		ftsResults, err := cognitive.FTSSearch(queryTopic, 5)
		if err == nil && len(ftsResults) > 0 {
			var sb strings.Builder
			sb.WriteString("Semantic Memory (L2):\n")
			for _, r := range ftsResults {
				sb.WriteString(fmt.Sprintf("- [score=%.2f] %s\n", r.Score, r.Snippet))
			}
			semanticCtx = sb.String()
		}
	}

	// 5. 组装系统提示词
	baseContent := "Generate an execution DAG based on the TaskModel.\n\n"
	if reflectionCtx != "" {
		baseContent += reflectionCtx + "\n"
	}
	if episodicCtx != "" {
		baseContent += episodicCtx + "\n"
	}
	if semanticCtx != "" {
		baseContent += semanticCtx + "\n"
	}

	if sCtx.TaskModel != nil {
		taskJson, _ := json.Marshal(sCtx.TaskModel)
		baseContent += "Parsed TaskModel:\n" + string(taskJson) + "\n\n"
	}

	if sCtx.InstalledExtensionsInfo != "" {
		baseContent += sCtx.InstalledExtensionsInfo + "\n\n"
	}

	// 注入可用工具列表，LLM 必须仅使用列表中的工具名称（action 字段）
	if tools != nil {
		baseContent += buildToolListSection(tools)
	}

	msgs := []protocol.Message{
		{Role: "system", Content: baseContent},
	}

	if wm := memory.Working(); wm != nil {
		msgs = wm.Immutable().PrependToMessages(msgs)
	}

	return msgs, nil
}

// buildToolListSection 将注册表中所有工具格式化为 LLM 可读的工具定义段落。
// 格式与 DAGNode.Action + DAGNode.Params 字段对齐，便于 LLM 直接引用。
func buildToolListSection(tools protocol.ToolRegistry) string {
	if tools == nil {
		return ""
	}
	list := tools.List()
	if len(list) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("Available Tools List (The 'action' field of DAG nodes MUST be one of the following names):\n")
	for _, t := range list {
		sb.WriteString(fmt.Sprintf("- %s: %s", t.Name, t.Description))
		if t.InputSchema != nil {
			if schemaBytes, err := json.Marshal(t.InputSchema); err == nil {
				sb.WriteString(fmt.Sprintf(" (Parameters schema: %s)", string(schemaBytes)))
			}
		}
		sb.WriteByte('\n')
	}
	sb.WriteByte('\n')
	return sb.String()
}

// buildReflectContext 组装反思阶段的 Prompt。
func buildReflectContext(ctx context.Context, memory protocol.Memory, sCtx *StateContext) ([]protocol.Message, error) {
	if memory == nil {
		return []protocol.Message{
			{Role: "system", Content: "Reflect on the execution result and evaluate the completion of the goal."},
		}, nil
	}

	baseContent := "Reflect on the execution result and evaluate the completion of the goal.\n\n"

	if len(sCtx.ExecuteResult) > 0 {
		baseContent += "Execution Result Summary:\n" + string(sCtx.ExecuteResult) + "\n\n"
	}

	msgs := []protocol.Message{
		{Role: "system", Content: baseContent},
	}

	if wm := memory.Working(); wm != nil {
		msgs = wm.Immutable().PrependToMessages(msgs)
	}

	return msgs, nil
}
