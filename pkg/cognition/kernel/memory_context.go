package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/substrate"
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
	b := NewPromptBuilder()

	// 1. 可信系统指令（基础模板 + 扩展信息）
	instr := "Structure the user intent into a TaskModel JSON.\n\n"
	if sCtx.InstalledExtensionsInfo != "" {
		instr += sCtx.InstalledExtensionsInfo + "\n\n"
	}
	safe, err := substrate.SanitizeToSafe(substrate.NewTaintedString(
		instr, substrate.TaintSource{OriginTaintLevel: protocol.TaintNone}, "perceive_system_prompt"))
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "buildPerceiveContext: sanitize instr", err)
	}
	b.WriteInstruction(safe)

	if memory == nil {
		return b.Build(), nil
	}

	var retrieved strings.Builder

	// 1. 查询相关的历史 Episodic 事件
	query := protocol.EpisodicQuery{
		Semantic: "agent task intent",
		K:        3,
	}
	events, err := memory.Episodic().Query(ctx, query)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "failed to query episodic memory", err)
	}

	if len(events) > 0 {
		retrieved.WriteString("Relevant Historical Episodic Memories:\n")
		for _, e := range events {
			retrieved.WriteString(fmt.Sprintf("- [%s] %s: %s\n", e.Event.CreatedAt.Format(time.RFC3339), e.Event.Type, string(e.Event.Payload)))
		}
	}

	// 2. 跨会话 Reflection 召回
	if rm := memory.Reflection(); rm != nil && sCtx.TaskModel != nil && sCtx.TaskModel.Goal != "" {
		reflections, rerr := rm.QueryReflections(ctx, protocol.ReflectionQuery{
			Topic: sCtx.TaskModel.Goal,
			K:     3,
		})
		if rerr == nil && len(reflections) > 0 {
			retrieved.WriteString("Cross-Session Reflections (past experience for similar tasks):\n")
			for _, r := range reflections {
				retrieved.WriteString(fmt.Sprintf("- [%s] %s: %s\n",
					r.CreatedAt.Format(time.RFC3339), r.Strategy, r.Decision))
			}
		}
	}

	// 3. 耳语线索注入
	if sCtx.WhisperChan != nil {
		select {
		case w := <-sCtx.WhisperChan:
			if w.Salience >= 0.5 {
				retrieved.WriteString(fmt.Sprintf("## Memory Whisper (source: %s)\n%s\n", w.Source, w.Content))
			}
		default:
		}
	}

	// 4. L2 语义记忆
	if cognitive != nil && sCtx.TaskModel != nil && sCtx.TaskModel.Goal != "" {
		ftsResults, err := cognitive.FTSSearch(sCtx.TaskModel.Goal, 5)
		if err == nil && len(ftsResults) > 0 {
			retrieved.WriteString("Semantic Memory (L2):\n")
			for _, r := range ftsResults {
				retrieved.WriteString(fmt.Sprintf("- [score=%.2f] %s\n", r.Score, r.Snippet))
			}
		}
	}

	if len(sCtx.ReasoningState) > 0 {
		retrieved.WriteString("Reasoning State from the previous iteration:\n")
		retrieved.WriteString(string(sCtx.ReasoningState))
		retrieved.WriteString("\n\n")
	}

	if retrieved.Len() > 0 {
		b.WriteUserData(substrate.NewTaintedString(
			retrieved.String(),
			substrate.TaintSource{OriginTaintLevel: protocol.TaintMedium},
			"retrieved_memory"))
	}

	if sCtx.RawIntentTS.Content() != "" {
		b.WriteUserData(sCtx.RawIntentTS)
	}

	msgs := b.Build()

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
	b := NewPromptBuilder()

	instr := "Generate an execution DAG based on the TaskModel.\n\n"
	if sCtx.TaskModel != nil {
		taskJson, _ := json.Marshal(sCtx.TaskModel)
		instr += "Parsed TaskModel:\n" + string(taskJson) + "\n\n"
	}
	if sCtx.GroundingGap != "" {
		instr += "Critical Knowledge Gap:\n" + sCtx.GroundingGap + "\n(Please address this gap explicitly in the plan.)\n\n"
	}
	if sCtx.InstalledExtensionsInfo != "" {
		instr += sCtx.InstalledExtensionsInfo + "\n\n"
	}
	if tools != nil {
		instr += buildToolListSection(tools)
	}

	safe, err := substrate.SanitizeToSafe(substrate.NewTaintedString(
		instr, substrate.TaintSource{OriginTaintLevel: protocol.TaintNone}, "plan_system_prompt"))
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "buildPlanContext: sanitize instr", err)
	}
	b.WriteInstruction(safe)

	if memory == nil {
		return b.Build(), nil
	}

	var retrieved strings.Builder

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

	if len(events) > 0 {
		retrieved.WriteString("Historical execution experiences for reference:\n")
		for _, e := range events {
			retrieved.WriteString(fmt.Sprintf("- [%s] %s: %s\n", e.Event.CreatedAt.Format(time.RFC3339), e.Event.Type, string(e.Event.Payload)))
		}
	}

	if rm := memory.Reflection(); rm != nil && queryStr != "" {
		reflections, rerr := rm.QueryReflections(ctx, protocol.ReflectionQuery{
			Topic: queryStr,
			K:     3,
		})
		if rerr == nil && len(reflections) > 0 {
			retrieved.WriteString("Cross-Session Reflections (execution patterns for similar tasks):\n")
			for _, r := range reflections {
				retrieved.WriteString(fmt.Sprintf("- [%s] %s: %s\n",
					r.CreatedAt.Format(time.RFC3339), r.Strategy, r.Decision))
			}
		}
	}

	if cognitive != nil && sCtx.TaskModel != nil && sCtx.TaskModel.Goal != "" {
		queryTopic := sCtx.TaskModel.Goal
		ftsResults, err := cognitive.FTSSearch(queryTopic, 5)
		if err == nil && len(ftsResults) > 0 {
			retrieved.WriteString("Semantic Memory (L2):\n")
			for _, r := range ftsResults {
				retrieved.WriteString(fmt.Sprintf("- [score=%.2f] %s\n", r.Score, r.Snippet))
			}
		}
	}

	if retrieved.Len() > 0 {
		b.WriteUserData(substrate.NewTaintedString(
			retrieved.String(),
			substrate.TaintSource{OriginTaintLevel: protocol.TaintMedium},
			"retrieved_memory"))
	}

	msgs := b.Build()

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

func buildReflectContext(ctx context.Context, memory protocol.Memory, sCtx *StateContext) ([]protocol.Message, error) {
	b := NewPromptBuilder()

	instr := "Reflect on the execution result and evaluate the completion of the goal.\n\n"
	safe, err := substrate.SanitizeToSafe(substrate.NewTaintedString(
		instr, substrate.TaintSource{OriginTaintLevel: protocol.TaintNone}, "reflect_system_prompt"))
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "buildReflectContext: sanitize instr", err)
	}
	b.WriteInstruction(safe)

	if len(sCtx.ExecuteResult) > 0 {
		b.WriteUserData(substrate.NewTaintedString(
			"Execution Result Summary:\n"+string(sCtx.ExecuteResult)+"\n\n",
			substrate.TaintSource{OriginTaintLevel: protocol.TaintMedium},
			"execute_result"))
	}

	msgs := b.Build()

	if memory != nil {
		if wm := memory.Working(); wm != nil {
			msgs = wm.Immutable().PrependToMessages(msgs)
		}
	}

	return msgs, nil
}
