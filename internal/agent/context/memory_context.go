package agentctx

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/agent/fsm"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/internal/tool/catalog"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// 实现由上层注入（pkg/cognition 层调用方提供 SurrealDBCoreStore）。

// fsm.CogResult 单条语义检索结果。

// BuildPerceiveContext 基于当前状态上下文（包含用户的原始任务描述/Intent）
// 从 EpisodicMemory、ReflectionMemory 与 WorkingMemory 组装感知阶段所需的 LLM 提示词。
// M05 §3.4: S_PERCEIVE 阶段拉取同 task_type 的 top-3 reflection 注入上下文。
func BuildPerceiveContext( //nolint:gocyclo
	ctx context.Context, memory protocol.MemoryFacade, sCtx *fsm.StateContext, cognitive fsm.CognitiveSearcher) ([]types.Message, error) {
	b := protocol.NewPromptBuilder()

	// 1. 可信系统指令（基础模板 + 扩展信息）
	instr := "Structure the user intent into a fsm.TaskModel JSON.\n\n"
	if sCtx.InstalledExtensionsInfo != "" {
		instr += sCtx.InstalledExtensionsInfo + "\n\n"
	}
	if hint := contextPressureHint(sCtx); hint != "" {
		instr += hint + "\n\n"
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

	// [UP-03] 注入核心工作记忆（ZoneCoreMemory）：LLM 经 core_memory_edit 显式维护的
	// 任务核心状态，每轮感知均需可见；读取失败按无核心记忆降级，不阻断组装。
	if blocks, cmErr := memory.ListCoreMemory(ctx, sCtx.AgentID, sCtx.SessionID); cmErr == nil && len(blocks) > 0 {
		b.WriteCoreMemory(blocks)
	}

	var retrieved strings.Builder

	intent := sCtx.RawIntentTS.Content()
	if intent != "" {
		// 1. 查询相关的历史 Episodic 事件
		query := types.EpisodicQuery{
			Semantic:      intent,
			K:             3,
			MaxTaintLevel: types.TaintHigh,
		}
		events, err := memory.ListEpisodicEvents(ctx, query)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "failed to query episodic memory", err)
		}

		if len(events) > 0 {
			retrieved.WriteString("Relevant Historical Episodic Memories:\n")
			for _, e := range events {
				if pbEv, _ := e.Event.(*types.Event); pbEv != nil {
					fmt.Fprintf(&retrieved, "- [%s] %s: %s\n", pbEv.CreatedAt.Format(time.RFC3339), pbEv.Type, string(pbEv.Payload))
				}
			}
		}
	}

	// 2. 跨会话 Reflection 召回
	if sCtx.TaskModel != nil && sCtx.TaskModel.Goal != "" {
		reflections, rerr := memory.ListReflections(ctx, types.ReflectionQuery{
			Topic: sCtx.TaskModel.Goal,
			K:     3,
		})
		if rerr == nil && len(reflections) > 0 {
			retrieved.WriteString("Cross-Session Reflections (past experience for similar tasks):\n")
			for _, r := range reflections {
				fmt.Fprintf(&retrieved, "- [%s] %s: %s\n",
					r.CreatedAt.Format(time.RFC3339), r.Strategy, r.Decision)
			}
		}
	}

	// 3. 耳语线索注入
	if sCtx.WhisperChan != nil {
		select {
		case w := <-sCtx.WhisperChan:
			if w.Salience >= 0.5 {
				fmt.Fprintf(&retrieved, "## Memory Whisper (source: %s)\n%s\n", w.Source, w.Content)
			}
		default:
		}
	}

	// 3.5 用户画像（P0-2：消费 default 用户画像）
	if p, err := memory.GetUserProfile(ctx, "default"); err == nil && p != nil {
		var summary []string
		for _, sf := range p.StableFacts {
			summary = append(summary, "- "+fmt.Sprint(sf))
		}
		for _, bp := range p.BehavioralPatterns {
			summary = append(summary, "- "+fmt.Sprint(bp))
		}
		if len(summary) > 0 {
			retrieved.WriteString("## User Profile (Context)\n" + strings.Join(summary, "\n") + "\n")
		}
	}

	// 4. L2 语义记忆
	if cognitive != nil && sCtx.TaskModel != nil && sCtx.TaskModel.Goal != "" {
		ftsResults, err := cognitive.FTSSearch(sCtx.TaskModel.Goal, 5)
		if err == nil && len(ftsResults) > 0 {
			retrieved.WriteString("Semantic Memory (L2):\n")
			for _, r := range ftsResults {
				fmt.Fprintf(&retrieved, "- [score=%.2f] %s\n", r.Score, r.Snippet)
			}
		}
	}

	// 5. M10 知识库检索结果 (RAG)
	if sCtx.KnowledgeSearcher != nil && sCtx.TaskModel != nil && sCtx.TaskModel.Goal != "" {
		ragResults, err := sCtx.KnowledgeSearcher.SearchRAG(ctx, sCtx.TaskModel.Goal, 3)
		if err == nil && len(ragResults) > 0 {
			retrieved.WriteString("Knowledge Base (RAG):\n")
			for _, r := range ragResults {
				fmt.Fprintf(&retrieved, "- [score=%.2f] %s: %s\n", r.Score, r.Source, r.Content)
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

	if memory != nil {
		msgs = memory.ImmutableCore().PrependToMessages(msgs)
	}

	return msgs, nil
}

// BuildPlanContext 基于已解析的 fsm.TaskModel 和可用工具列表
// 从 Memory 系统组装生成 DAG 计划所需的 LLM 提示词。
// tools 为 nil 时跳过工具注入（测试环境）。
func BuildPlanContext( //nolint:gocyclo
	ctx context.Context, memory protocol.MemoryFacade, sCtx *fsm.StateContext, cata catalog.Catalog, cognitive fsm.CognitiveSearcher) ([]types.Message, error) {
	b := protocol.NewPromptBuilder()

	var sysPrompt strings.Builder
	sysPrompt.WriteString("Generate an execution DAG based on the fsm.TaskModel.\n\n")
	if sCtx.TaskModel != nil {
		taskJson, _ := json.Marshal(sCtx.TaskModel)
		sysPrompt.WriteString("Parsed fsm.TaskModel:\n" + string(taskJson) + "\n\n")
	}
	if sCtx.GroundingGap != "" {
		sysPrompt.WriteString("Critical Knowledge Gap:\n" + sCtx.GroundingGap + "\n(Please address this gap explicitly in the plan.)\n\n")
	}
	if sCtx.InstalledExtensionsInfo != "" {
		sysPrompt.WriteString(sCtx.InstalledExtensionsInfo + "\n\n")
	}
	if hint := contextPressureHint(sCtx); hint != "" {
		sysPrompt.WriteString(hint + "\n\n")
	}

	// 5. Build Tools List (M2.c/f)
	if cata != nil {
		// TaskID 激活作用域必须与 internal/agent/dag/executor.go 的 Execute()
		// 注入值一致——生产路径用 a.sCtx.SessionID 作为 taskID（见 agent_execute.go
		// executor.Execute(ctx, plan, a.sCtx.SessionID, a.sCtx.AgentID)），此处保持同源。
		toolCtx := context.WithValue(ctx, protocol.CtxTaskIDKey{}, sCtx.SessionID)
		toolSec := BuildToolListSection(toolCtx, cata)
		sysPrompt.WriteString(toolSec)
	}

	safe, err := taint.SanitizeToSafe(taint.NewTaintedString(
		sysPrompt.String(), taint.TaintSource{OriginTaintLevel: types.TaintNone}, "plan_system_prompt"))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "BuildPlanContext: sanitize instr", err)
	}
	b.WriteInstruction(safe)

	if memory == nil {
		return b.Build(), nil
	}

	// [UP-03] 规划阶段同样注入核心工作记忆，保证 DAG 生成可见任务核心状态。
	if blocks, cmErr := memory.ListCoreMemory(ctx, sCtx.AgentID, sCtx.SessionID); cmErr == nil && len(blocks) > 0 {
		b.WriteCoreMemory(blocks)
	}

	var retrieved strings.Builder

	var queryStr string
	if sCtx.TaskModel != nil {
		queryStr = sCtx.TaskModel.Goal
	}
	query := types.EpisodicQuery{
		Semantic:      queryStr,
		K:             5,
		MaxTaintLevel: types.TaintHigh,
	}
	events, err := memory.ListEpisodicEvents(ctx, query)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "failed to query episodic memory", err)
	}

	if len(events) > 0 {
		retrieved.WriteString("Historical execution experiences for reference:\n")
		for _, e := range events {
			if pbEv, _ := e.Event.(*types.Event); pbEv != nil {
				fmt.Fprintf(&retrieved, "- [%s] %s: %s\n", pbEv.CreatedAt.Format(time.RFC3339), pbEv.Type, string(pbEv.Payload))
			}
		}
	}

	if memory != nil && queryStr != "" {
		reflections, rerr := memory.ListReflections(ctx, types.ReflectionQuery{
			Topic: queryStr,
			K:     3,
		})
		if rerr == nil && len(reflections) > 0 {
			retrieved.WriteString("Cross-Session Reflections (execution patterns for similar tasks):\n")
			for _, r := range reflections {
				fmt.Fprintf(&retrieved, "- [%s] %s: %s\n",
					r.CreatedAt.Format(time.RFC3339), r.Strategy, r.Decision)
			}
		}
	}

	if cognitive != nil && sCtx.TaskModel != nil && sCtx.TaskModel.Goal != "" {
		queryTopic := sCtx.TaskModel.Goal
		ftsResults, err := cognitive.FTSSearch(queryTopic, 5)
		if err == nil && len(ftsResults) > 0 {
			retrieved.WriteString("Semantic Memory (L2):\n")
			for _, r := range ftsResults {
				fmt.Fprintf(&retrieved, "- [score=%.2f] %s\n", r.Score, r.Snippet)
			}
		}
	}

	if sCtx.KnowledgeSearcher != nil && queryStr != "" {
		ragResults, err := sCtx.KnowledgeSearcher.SearchRAG(ctx, queryStr, 3)
		if err == nil && len(ragResults) > 0 {
			retrieved.WriteString("Knowledge Base (RAG):\n")
			for _, r := range ragResults {
				fmt.Fprintf(&retrieved, "- [score=%.2f] %s: %s\n", r.Score, r.Source, r.Content)
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

	if memory != nil {
		msgs = memory.ImmutableCore().PrependToMessages(msgs)
	}

	return msgs, nil
}

// BuildToolListSection 将注册表中所有工具格式化为 LLM 可读的工具定义段落。
// 格式与 DAGNode.Action + DAGNode.Params 字段对齐，便于 LLM 直接引用。
//
// ctx 必须携带 protocol.CtxTaskIDKey（由调用方从 sCtx.SessionID 注入），否则
// 懒加载模式下 search_tools 在上一轮激活的工具（CompositeCatalog.ActivateTool）
// 无法在本轮 Schemas() 重建时命中同一激活作用域——见 internal/tool/catalog/composite.go
// 的 Schemas() 与 internal/tool/tool_search.go 的 sessionIDFromCtx，两处必须使用
// 同一个 TaskID 才能让"搜索到的工具在后续轮次真正可调用"这个懒加载协议闭环。
func BuildToolListSection(ctx context.Context, cata catalog.Catalog) string {
	if cata == nil {
		return ""
	}
	// TrustCommunity 是通常的默认门槛，如果有更高要求可传入不同值
	schemas := cata.Schemas(ctx, types.TrustCommunity)
	if len(schemas) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("Available Tools List (The 'action' field of DAG nodes MUST be one of the following names):\n")
	for _, t := range schemas {
		fmt.Fprintf(&sb, "- %s: %s", t.Name, t.Description)
		if t.Parameters != nil {
			if schemaBytes, err := json.Marshal(t.Parameters); err == nil {
				fmt.Fprintf(&sb, " (Parameters schema: %s)", string(schemaBytes))
			}
		}
		sb.WriteByte('\n')
	}
	sb.WriteByte('\n')
	return sb.String()
}

func BuildReflectContext(ctx context.Context, memory protocol.MemoryFacade, sCtx *fsm.StateContext) ([]types.Message, error) {
	b := protocol.NewPromptBuilder()

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
	if len(sCtx.ExecuteImageParts) > 0 {
		b.WriteUserImages(sCtx.ExecuteImageParts)
	}

	msgs := b.Build()

	if memory != nil {
		if memory != nil {
			msgs = memory.ImmutableCore().PrependToMessages(msgs)
		}
	}

	return msgs, nil
}
