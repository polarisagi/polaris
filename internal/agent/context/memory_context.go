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
	b := prompt.NewPromptBuilder()

	// 1. 可信系统指令（基础模板，不含第三方来源内容）
	instr := "Structure the user intent into a fsm.TaskModel JSON.\n\n"
	if hint := contextPressureHint(sCtx); hint != "" {
		instr += hint + "\n\n"
	}
	safe, err := taint.SanitizeToSafe(taint.NewTaintedString(
		instr, taint.TaintSource{OriginTaintLevel: types.TaintNone}, "perceive_system_prompt"))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "BuildPerceiveContext: sanitize instr", err)
	}
	b.WriteInstruction(safe)

	// GD-14-005：用户显式声明信任的工作区约束文档，作为项目级系统指令写入
	// ZoneImmutable。只有 WorkspaceContextLoader 判定 Trusted 的内容才会到这里
	// ——默认路径下本字段恒为空，AGENTS.md 走下方不可信通道。
	if sCtx.WorkspaceContextTrusted != "" {
		trustedSafe, tErr := taint.SanitizeToSafe(taint.NewTaintedString(
			sCtx.WorkspaceContextTrusted,
			taint.TaintSource{Module: "workspace", OriginTaintLevel: types.TaintNone},
			"workspace_context_trusted"))
		if tErr != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "BuildPerceiveContext: sanitize trusted workspace context", tErr)
		}
		b.WriteInstruction(trustedSafe)
	}

	// S-02：已安装扩展的自述信息来源不可信（第三方可控），单独进入
	// ZoneExternalCatalog 并按 TaintHigh 打标，禁止混入 ZoneImmutable。
	if sCtx.InstalledExtensionsInfo != "" {
		b.WriteExternalCatalog("extensions", taint.NewTaintedString(
			sCtx.InstalledExtensionsInfo,
			taint.TaintSource{Module: "extension", OriginTaintLevel: types.TaintHigh},
			"extension_catalog"))
	}

	// GD-14-005：工作区上下文的默认通道。AGENTS.md/CLAUDE.md 在 clone 来的仓库中
	// 完全是攻击者可控的，威胁模型与第三方扩展自述一致，故同样只进
	// ZoneExternalCatalog 并按 TaintHigh 围栏——绝不因"文件名恰好是约定名字"
	// 而推定信任。
	if sCtx.WorkspaceContextUntrusted != "" {
		b.WriteExternalCatalog("workspace_context", taint.NewTaintedString(
			sCtx.WorkspaceContextUntrusted,
			taint.TaintSource{Module: "workspace", OriginTaintLevel: types.TaintHigh},
			"workspace_context"))
	}

	if memory == nil {
		return b.Build(), nil
	}

	// [UP-03] 注入核心工作记忆（ZoneCoreMemory）：LLM 经 core_memory_edit 显式维护的
	// 任务核心状态，每轮感知均需可见；读取失败按无核心记忆降级，不阻断组装。
	if blocks, cmErr := memory.ListCoreMemory(ctx, sCtx.AgentID, sCtx.SessionID); cmErr == nil && len(blocks) > 0 {
		b.WriteCoreMemory(blocks)
	}

	var retrieved strings.Builder

	intent := sCtx.RawIntentTS.UnsafeContent()
	if sCtx.TaskID != "" && intent != "" {
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
	b := prompt.NewPromptBuilder()

	var sysPrompt strings.Builder
	sysPrompt.WriteString("Generate an execution DAG based on the fsm.TaskModel.\n\n")
	if sCtx.TaskModel != nil {
		taskJson, _ := json.Marshal(sCtx.TaskModel)
		sysPrompt.WriteString("<task_model>\n" + string(taskJson) + "\n</task_model>\n\n")
	}
	if sCtx.GroundingGap != "" {
		sysPrompt.WriteString("<grounding_gap source=\"untrusted\">\n" + sCtx.GroundingGap + "\n</grounding_gap>\n(Please address this gap explicitly in the plan.)\n\n")
	}
	if hint := contextPressureHint(sCtx); hint != "" {
		sysPrompt.WriteString(hint + "\n\n")
	}

	safe, err := taint.SanitizeToSafe(taint.NewTaintedString(
		sysPrompt.String(), taint.TaintSource{OriginTaintLevel: types.TaintNone}, "plan_system_prompt"))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "BuildPlanContext: sanitize instr", err)
	}
	b.WriteInstruction(safe)

	// S-02：已安装扩展自述信息来源不可信，单独进入 ZoneExternalCatalog。
	if sCtx.InstalledExtensionsInfo != "" {
		b.WriteExternalCatalog("extensions", taint.NewTaintedString(
			sCtx.InstalledExtensionsInfo,
			taint.TaintSource{Module: "extension", OriginTaintLevel: types.TaintHigh},
			"extension_catalog"))
	}

	// 5. Build Tools List (M2.c/f) —— 按来源分级写入 ZoneExternalCatalog，禁止与
	// 内核指令混入同一 TaintNone 区（S-02，间接 Prompt Injection 防护）。
	if cata != nil {
		// TaskID 激活作用域必须与 internal/execute/dag/executor.go 的 Execute()
		// 注入值一致——生产路径用 a.sCtx.SessionID 作为 taskID（见 agent_execute.go
		// executor.Execute(ctx, plan, a.sCtx.SessionID, a.sCtx.AgentID)），此处保持同源。
		toolCtx := context.WithValue(ctx, protocol.CtxTaskIDKey{}, sCtx.SessionID)
		toolSec, toolTaint := BuildToolListSection(toolCtx, cata)
		if toolSec != "" {
			b.WriteExternalCatalog("tools", taint.NewTaintedString(
				toolSec,
				taint.TaintSource{Module: "tool_catalog", OriginTaintLevel: toolTaint},
				"tool_catalog"))
		}
	}

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

// BuildToolListSection 已迁移至 tool_list_section.go（R7 文件行数治理，S-02/S-03
// 改造导致本文件超出 400 行上限，按职责拆分：工具目录格式化与 Perceive/Plan/
// Reflect 上下文组装是两个不同职责）。

func BuildReflectContext(ctx context.Context, memory protocol.MemoryFacade, sCtx *fsm.StateContext) ([]types.Message, error) {
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
