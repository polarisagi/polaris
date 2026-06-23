package agentctx

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/agent/fsm"

	"github.com/polarisagi/polaris/internal/prompt"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// 实现由上层注入（pkg/cognition 层调用方提供 SurrealDBCoreStore）。

// fsm.CogResult 单条语义检索结果。

// BuildPerceiveContext 基于当前状态上下文（包含用户的原始任务描述/Intent）
// 从 EpisodicMemory、ReflectionMemory 与 WorkingMemory 组装感知阶段所需的 LLM 提示词。
// M05 §3.4: S_PERCEIVE 阶段拉取同 task_type 的 top-3 reflection 注入上下文。
func BuildPerceiveContext( //nolint:gocyclo
	ctx context.Context, memory protocol.Memory, sCtx *fsm.StateContext, cognitive fsm.CognitiveSearcher) ([]types.Message, error) {
	b := prompt.NewPromptBuilder()

	// 1. 可信系统指令（基础模板 + 扩展信息）
	instr := "Structure the user intent into a fsm.TaskModel JSON.\n\n"
	if sCtx.InstalledExtensionsInfo != "" {
		instr += sCtx.InstalledExtensionsInfo + "\n\n"
	}
	safe, err := taint.SanitizeToSafe(taint.NewTaintedString(
		instr, taint.TaintSource{OriginTaintLevel: types.TaintNone}, "perceive_system_prompt"))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "BuildPerceiveContext: sanitize instr", err)
	}
	b.WriteInstruction(safe)

	if memory == nil {
		return b.Build(), nil
	}

	var retrieved strings.Builder

	// 1. 查询相关的历史 Episodic 事件
	query := types.EpisodicQuery{
		Semantic: "agent task intent",
		K:        3,
	}
	events, err := memory.Episodic().Query(ctx, query)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "failed to query episodic memory", err)
	}

	if len(events) > 0 {
		retrieved.WriteString("Relevant Historical Episodic Memories:\n")
		for _, e := range events {
			if pbEv, _ := e.Event.(*types.Event); pbEv != nil {
				retrieved.WriteString(fmt.Sprintf("- [%s] %s: %s\n", pbEv.CreatedAt.Format(time.RFC3339), pbEv.Type, string(pbEv.Payload)))
			}
		}
	}

	// 2. 跨会话 Reflection 召回
	if rm := memory.Reflection(); rm != nil && sCtx.TaskModel != nil && sCtx.TaskModel.Goal != "" {
		reflections, rerr := rm.QueryReflections(ctx, types.ReflectionQuery{
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

	// 5. M10 知识库检索结果 (RAG)
	if sCtx.KnowledgeSearcher != nil && sCtx.TaskModel != nil && sCtx.TaskModel.Goal != "" {
		ragResults, err := sCtx.KnowledgeSearcher.SearchRAG(ctx, sCtx.TaskModel.Goal, 3)
		if err == nil && len(ragResults) > 0 {
			retrieved.WriteString("Knowledge Base (RAG):\n")
			for _, r := range ragResults {
				retrieved.WriteString(fmt.Sprintf("- [score=%.2f] %s: %s\n", r.Score, r.Source, r.Content))
			}
		}
	}

	if len(sCtx.ReasoningState) > 0 {
		retrieved.WriteString("Reasoning State from the previous iteration:\n")
		retrieved.WriteString(string(sCtx.ReasoningState))
		retrieved.WriteString("\n\n")
	}

	if retrieved.Len() > 0 {
		// 召回数据携带 TaintMedium，需反馈到会话全局污点（只升不降）
		if types.TaintMedium > sCtx.GlobalTaintLevel {
			sCtx.GlobalTaintLevel = types.TaintMedium
		}
		b.WriteUserData(taint.NewTaintedString(
			retrieved.String(),
			taint.TaintSource{OriginTaintLevel: types.TaintMedium},
			"retrieved_memory"))
	}

	if !sCtx.RawIntentTS.IsEmpty() {
		b.WriteUserData(sCtx.RawIntentTS)
	}

	msgs := b.Build()

	if wm := memory.Working(); wm != nil {
		msgs = wm.Immutable().PrependToMessages(msgs)
	}

	return msgs, nil
}

// BuildPlanContext 基于已解析的 fsm.TaskModel 和可用工具列表
// 从 Memory 系统组装生成 DAG 计划所需的 LLM 提示词。
// tools 为 nil 时跳过工具注入（测试环境）。
func BuildPlanContext( //nolint:gocyclo
	ctx context.Context, memory protocol.Memory, sCtx *fsm.StateContext, tools protocol.ToolRegistry, cognitive fsm.CognitiveSearcher) ([]types.Message, error) {
	b := prompt.NewPromptBuilder()

	instr := "Generate an execution DAG based on the fsm.TaskModel.\n\n"
	if sCtx.TaskModel != nil {
		taskJson, _ := json.Marshal(sCtx.TaskModel)
		instr += "Parsed fsm.TaskModel:\n" + string(taskJson) + "\n\n"
	}
	if sCtx.GroundingGap != "" {
		instr += "Critical Knowledge Gap:\n" + sCtx.GroundingGap + "\n(Please address this gap explicitly in the plan.)\n\n"
	}
	if sCtx.InstalledExtensionsInfo != "" {
		instr += sCtx.InstalledExtensionsInfo + "\n\n"
	}
	if tools != nil {
		instr += BuildToolListSection(tools)
	}

	safe, err := taint.SanitizeToSafe(taint.NewTaintedString(
		instr, taint.TaintSource{OriginTaintLevel: types.TaintNone}, "plan_system_prompt"))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "BuildPlanContext: sanitize instr", err)
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
	query := types.EpisodicQuery{
		Semantic: queryStr,
		K:        5,
	}
	events, err := memory.Episodic().Query(ctx, query)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "failed to query episodic memory", err)
	}

	if len(events) > 0 {
		retrieved.WriteString("Historical execution experiences for reference:\n")
		for _, e := range events {
			if pbEv, _ := e.Event.(*types.Event); pbEv != nil {
				retrieved.WriteString(fmt.Sprintf("- [%s] %s: %s\n", pbEv.CreatedAt.Format(time.RFC3339), pbEv.Type, string(pbEv.Payload)))
			}
		}
	}

	if rm := memory.Reflection(); rm != nil && queryStr != "" {
		reflections, rerr := rm.QueryReflections(ctx, types.ReflectionQuery{
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

	if sCtx.KnowledgeSearcher != nil && queryStr != "" {
		ragResults, err := sCtx.KnowledgeSearcher.SearchRAG(ctx, queryStr, 3)
		if err == nil && len(ragResults) > 0 {
			retrieved.WriteString("Knowledge Base (RAG):\n")
			for _, r := range ragResults {
				retrieved.WriteString(fmt.Sprintf("- [score=%.2f] %s: %s\n", r.Score, r.Source, r.Content))
			}
		}
	}

	if retrieved.Len() > 0 {
		if types.TaintMedium > sCtx.GlobalTaintLevel {
			sCtx.GlobalTaintLevel = types.TaintMedium
		}
		b.WriteUserData(taint.NewTaintedString(
			retrieved.String(),
			taint.TaintSource{OriginTaintLevel: types.TaintMedium},
			"retrieved_memory"))
	}

	msgs := b.Build()

	if wm := memory.Working(); wm != nil {
		msgs = wm.Immutable().PrependToMessages(msgs)
	}

	return msgs, nil
}

// BuildToolListSection 将注册表中所有工具格式化为 LLM 可读的工具定义段落。
// 格式与 DAGNode.Action + DAGNode.Params 字段对齐，便于 LLM 直接引用。
func BuildToolListSection(tools protocol.ToolRegistry) string {
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

func BuildReflectContext(ctx context.Context, memory protocol.Memory, sCtx *fsm.StateContext) ([]types.Message, error) {
	b := prompt.NewPromptBuilder()

	instr := "Reflect on the execution result and evaluate the completion of the goal.\n\n"
	safe, err := taint.SanitizeToSafe(taint.NewTaintedString(
		instr, taint.TaintSource{OriginTaintLevel: types.TaintNone}, "reflect_system_prompt"))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "BuildReflectContext: sanitize instr", err)
	}
	b.WriteInstruction(safe)

	if len(sCtx.ExecuteResult) > 0 {
		b.WriteUserData(taint.NewTaintedString(
			"Execution Result Summary:\n"+string(sCtx.ExecuteResult)+"\n\n",
			taint.TaintSource{OriginTaintLevel: types.TaintMedium},
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
