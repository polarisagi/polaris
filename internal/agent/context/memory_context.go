package agentctx

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/prompt"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/internal/tool/catalog"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// 实现由上层注入（internal/agent 层调用方提供 SurrealDBCoreStore）。

// fsm.CogResult 单条语义检索结果。

// BuildPerceiveContext 基于当前状态上下文（包含用户的原始任务描述/Intent）
// 从 EpisodicMemory、ReflectionMemory 与 WorkingMemory 组装感知阶段所需的 LLM 提示词。
// M05 §3.4: S_PERCEIVE 阶段拉取同 task_type 的 top-3 reflection 注入上下文。
func BuildPerceiveContext( //nolint:gocyclo
	ctx context.Context, memory protocol.MemoryFacade, sCtx *fsm.StateContext, cognitive fsm.CognitiveSearcher) ([]types.Message, error) {
	b := prompt.NewPromptBuilder()

	// 1. 可信系统指令：阶段契约唯一来源 kernel/perceive.md（ADR-0098 决策三）
	if err := writePhaseInstruction(b, sCtx, "kernel/perceive.md", "Structure the user intent into a TaskModel JSON object."); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "BuildPerceiveContext", err)
	}

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

	intent := sCtx.RawIntentTS.UnsafeContent()
	var goal string
	if sCtx.TaskModel != nil {
		goal = sCtx.TaskModel.Goal
	}
	// ADR-0101 决策一：短确认（好的/ok/同意）的语义完全在对话历史里，长期记忆
	// 召回对它零信息增益，却要付一轮 episodic/反思/语义/RAG 检索（含 embedding）
	// 以及随之膨胀的 prompt。只保留画像与下方的对话历史。
	leanAck := fsm.ClassifyIntentWeight(intent) == fsm.IntentAck
	if leanAck {
		goal = ""
	}
	recalled, err := recallWithin(ctx, memory, cognitive, recallSpec{
		episodic:         sCtx.TaskID != "" && intent != "" && !leanAck,
		episodicQuery:    intent,
		episodicK:        3,
		episodicHeader:   "Relevant Historical Episodic Memories:\n",
		goal:             goal,
		reflectionHeader: "Cross-Session Reflections (past experience for similar tasks):\n",
		withProfile:      true,
		projectID:        scopeProjectID(sCtx), // P1/P3
		knowledge:        sCtx.KnowledgeSearcher,
	})
	if err := degradeOnRecallTimeout("BuildPerceiveContext", err); err != nil {
		return nil, err
	}
	var retrieved strings.Builder
	retrieved.WriteString(recalled)

	// 耳语线索：消费 channel，必须留在主路径（召回 goroutine 不得触碰 sCtx）。
	if sCtx.WhisperChan != nil {
		select {
		case w := <-sCtx.WhisperChan:
			if w.Salience >= 0.5 {
				fmt.Fprintf(&retrieved, "## Memory Whisper (source: %s)\n%s\n", w.Source, w.Content)
			}
		default:
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
		// 附加等级必须取 max(自身, 会话全局)（L-03）：写死 TaintMedium 时，
		// 若本会话已因更脏的来源升到 TaintHigh，这段召回文本会被重新标成
		// Medium，等于凭一次拼接把污点降级。与 BuildReflectContext 里
		// execute_result 的处置保持一致。
		b.WriteUserData(taint.NewTaintedString(
			retrieved.String(),
			taint.TaintSource{OriginTaintLevel: types.PropagateTaint(types.TaintMedium, sCtx.GlobalTaintLevel)},
			"retrieved_memory"))
	}

	// 对话历史紧贴本轮意图之前：Perceive 需据此消解指代、产出自包含 Goal（ADR-0098）。
	fsm.WriteConversationHistory(b, sCtx)
	if !sCtx.RawIntentTS.IsEmpty() {
		b.WriteUserData(sCtx.RawIntentTS)
	}

	return memory.ImmutableCore().PrependToMessages(b.Build()), nil
}

// BuildPlanContext 基于已解析的 fsm.TaskModel 和可用工具列表
// 从 Memory 系统组装生成 DAG 计划所需的 LLM 提示词。
// tools 为 nil 时跳过工具注入（测试环境）。
func BuildPlanContext( //nolint:gocyclo
	ctx context.Context, memory protocol.MemoryFacade, sCtx *fsm.StateContext, cata catalog.Catalog, cognitive fsm.CognitiveSearcher) ([]types.Message, error) {
	b := prompt.NewPromptBuilder()

	// 系统指令区只放进程内常量（TaintNone）。TaskModel 由 LLM 从外部意图解析而来、
	// GroundingGap 来自外部知识评估，二者都属数据而非指令：此前拼进 sysPrompt 并以
	// TaintNone 写入 ZoneImmutable，等于把外部可控文本提权为系统指令（GR-4.1-003）。
	if err := writePhaseInstruction(b, sCtx, "kernel/plan.md", "Generate an execution DAG based on the TaskModel provided in the user data section."); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "BuildPlanContext", err)
	}

	if sCtx.TaskModel != nil {
		taskJSON, _ := json.Marshal(sCtx.TaskModel)
		b.WriteUserData(taint.NewTaintedString(
			"<task_model>\n"+string(taskJSON)+"\n</task_model>",
			taint.TaintSource{Module: "task_model", OriginTaintLevel: types.PropagateTaint(types.TaintMedium, sCtx.GlobalTaintLevel)},
			"m4_task_model"))
	}
	if sCtx.GroundingGap != "" {
		b.WriteUserData(taint.NewTaintedString(
			"<grounding_gap>\n"+sCtx.GroundingGap+"\n</grounding_gap>\n(Address this gap explicitly in the plan.)",
			// 世界模型对外部知识的评估：至少 TaintHigh，且不低于会话已累积污点（L-03：禁止常量）
			taint.TaintSource{Module: "world_model", OriginTaintLevel: types.PropagateTaint(types.TaintHigh, sCtx.GlobalTaintLevel)},
			"grounding_gap"))
	}

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

	var queryStr string
	if sCtx.TaskModel != nil {
		queryStr = sCtx.TaskModel.Goal
	}
	recalled, err := recallWithin(ctx, memory, cognitive, recallSpec{
		episodic:         true,
		episodicQuery:    queryStr,
		episodicK:        5,
		episodicHeader:   "Historical execution experiences for reference:\n",
		goal:             queryStr,
		reflectionHeader: "Cross-Session Reflections (execution patterns for similar tasks):\n",
		projectID:        scopeProjectID(sCtx), // P2/P4
		knowledge:        sCtx.KnowledgeSearcher,
	})
	if err := degradeOnRecallTimeout("BuildPlanContext", err); err != nil {
		return nil, err
	}
	var retrieved strings.Builder
	retrieved.WriteString(recalled)

	if retrieved.Len() > 0 {
		if types.TaintMedium > sCtx.GlobalTaintLevel {
			sCtx.GlobalTaintLevel = types.TaintMedium
		}
		// 同 BuildPerceiveContext：附加等级取 max(自身, 会话全局)，禁写死常量（L-03）。
		b.WriteUserData(taint.NewTaintedString(
			retrieved.String(),
			taint.TaintSource{OriginTaintLevel: types.PropagateTaint(types.TaintMedium, sCtx.GlobalTaintLevel)},
			"retrieved_memory"))
	}
	// 观察—再规划（决策八）：已执行轮次的结果，规划下一步而非重复。
	// 重规划闭环（决策六）：告诉模型上一版为何被拒 / 为何未达成，避免原样重来。
	fsm.WriteObservations(b, sCtx)
	fsm.WriteReplanFeedback(b, sCtx)

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

	fsm.WriteKernelInstruction(b, "kernel/reflect.md", "Reflect on the execution result and evaluate the completion of the goal.")

	// 没有目标就无从判定 GoalAchieved：此前反思只看到执行结果。
	if sCtx.TaskModel != nil && sCtx.TaskModel.Goal != "" {
		b.WriteUserData(taint.NewTaintedString("Task Goal: "+sCtx.TaskModel.Goal,
			taint.TaintSource{OriginTaintLevel: types.PropagateTaint(types.TaintMedium, sCtx.GlobalTaintLevel)},
			"m4_task_model"))
	}
	if len(sCtx.ExecuteResult) > 0 {
		b.WriteUserData(taint.NewTaintedString(
			"Execution Result Summary:\n"+string(sCtx.ExecuteResult)+"\n\n",
			taint.TaintSource{OriginTaintLevel: types.PropagateTaint(types.TaintMedium, sCtx.GlobalTaintLevel)},
			"execute_result"))
	}
	if len(sCtx.ExecuteImageParts) > 0 {
		b.WriteUserImages(sCtx.ExecuteImageParts)
	}

	msgs := b.Build()
	if memory != nil {
		msgs = memory.ImmutableCore().PrependToMessages(msgs)
	}
	return msgs, nil
}
