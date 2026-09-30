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
	// 五层前缀账本（ADR-0105 决策一）：L1 会话层 → L2 历史 → L3 阶段模板 → L4 回合内容。
	// L1/L2 由四个阶段共用同一写入器，字节一致；写入顺序与层无关，最终由 BuildLayered 排序。
	b := prompt.NewPromptBuilder()

	// L1：核心记忆、子 Agent 画像、工作区上下文（可信→指令区/不可信→TaintHigh 围栏）、扩展目录。
	if err := fsm.WriteSessionLayer(ctx, b, memory, sCtx); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "BuildPerceiveContext", err)
	}
	// L2：对话历史逐条写入，Perceive 据此消解指代、产出自包含 Goal（ADR-0098）。
	fsm.WriteConversationHistory(b, sCtx)
	// L3：阶段契约唯一来源 kernel/perceive.md（ADR-0098 决策三）+ 上下文压力提示。
	if err := writePhaseInstruction(b, memory, sCtx, "kernel/perceive.md", "Structure the user intent into a TaskModel JSON object."); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "BuildPerceiveContext", err)
	}

	if memory == nil {
		if !sCtx.RawIntentTS.IsEmpty() {
			b.WriteUserData(sCtx.RawIntentTS)
		}
		return fsm.FinishLayered(b, nil), nil
	}

	intent := sCtx.RawIntentTS.UnsafeContent()
	// ADR-0102 决策一：短确认（好的/ok/同意）的语义完全在对话历史里，长期记忆
	// 召回对它零信息增益，却要付一轮 episodic 检索（含 embedding）以及随之膨胀的 prompt。
	// 只保留画像与下方的对话历史。
	leanAck := fsm.ClassifyIntentWeight(intent) == fsm.IntentAck
	// 回合内只召一次（ADR-0105 决策四）：结果写入 sCtx.TurnRecall，Plan 只补缺口。
	// 反思/L2/RAG 以 Goal 为查询词，而本阶段正是产出本回合 Goal 的阶段：此刻 sCtx.TaskModel
	// 是上一回合遗留（回合起点不清），拿它查既是错主题，又会让 Plan 误判"已覆盖"而不用真正的
	// Goal 补查。故这三段一律留给 Plan；本阶段只查 episodic（按本轮原话）与画像。
	episodicQuery := ""
	if sCtx.TaskID != "" && intent != "" && !leanAck {
		episodicQuery = intent
	}
	recalled, err := turnRecallText(ctx, memory, cognitive, sCtx, recallWant{
		episodicQuery: episodicQuery,
		episodicK:     perceiveEpisodicK,
		withProfile:   true,
	}, "BuildPerceiveContext")
	if err != nil {
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

	// L4：召回在前，本轮意图压轴（历史已在 L2，不再夹在召回与意图之间）。
	if !sCtx.RawIntentTS.IsEmpty() {
		b.WriteUserData(sCtx.RawIntentTS)
	}

	return fsm.FinishLayered(b, memory), nil
}

// BuildPlanContext 基于已解析的 fsm.TaskModel 和可用工具列表
// 从 Memory 系统组装生成 DAG 计划所需的 LLM 提示词。
// tools 为 nil 时跳过工具注入（测试环境）。
func BuildPlanContext( //nolint:gocyclo
	ctx context.Context, memory protocol.MemoryFacade, sCtx *fsm.StateContext, cata catalog.Catalog, cognitive fsm.CognitiveSearcher) ([]types.Message, error) {
	b := prompt.NewPromptBuilder()

	// L1/L2 与 Perceive/Reflect/Respond 共用写入器，前缀字节一致（ADR-0105 决策一/二）。
	if err := fsm.WriteSessionLayer(ctx, b, memory, sCtx); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "BuildPlanContext", err)
	}
	fsm.WriteConversationHistory(b, sCtx)

	// L3：系统指令区只放进程内常量（TaintNone）。TaskModel 由 LLM 从外部意图解析而来、
	// GroundingGap 来自外部知识评估，二者都属数据而非指令：此前拼进 sysPrompt 并以
	// TaintNone 写入 ZoneImmutable，等于把外部可控文本提权为系统指令（GR-4.1-003）。
	if err := writePhaseInstruction(b, memory, sCtx, "kernel/plan.md", "Generate an execution DAG based on the TaskModel provided in the user data section."); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "BuildPlanContext", err)
	}

	// Build Tools List (M2.c/f) —— 按来源分级写入 ZoneExternalCatalog，禁止与内核指令混入
	// 同一 TaintNone 区（S-02，间接 Prompt Injection 防护）。工具目录只有 Plan 需要，
	// 且随懒加载激活而变，属 L3 而非 L1（否则 Plan 的 L1 与其它阶段失配）。
	if cata != nil {
		// TaskID 激活作用域必须与 internal/execute/dag/executor.go 的 Execute()
		// 注入值一致——生产路径用 a.sCtx.SessionID 作为 taskID（见 agent_execute.go
		// executor.Execute(ctx, plan, a.sCtx.SessionID, a.sCtx.AgentID)），此处保持同源。
		toolCtx := context.WithValue(ctx, protocol.CtxTaskIDKey{}, sCtx.SessionID)
		toolSec, toolTaint := BuildToolListSection(toolCtx, cata)
		if toolSec != "" {
			b.SetLayer(protocol.LayerPhase)
			b.WriteExternalCatalog("tools", taint.NewTaintedString(
				toolSec,
				taint.TaintSource{Module: "tool_catalog", OriginTaintLevel: toolTaint},
				"tool_catalog"))
			b.ResetLayer()
		}
	}
	// 重规划新增工具 / <tool-hints>（状态机刷新到 sCtx.PlanHintBlocks），同属 L3。
	fsm.WritePlanHints(b, sCtx)

	// L4 回合层从这里开始：召回（需要 memory）→ TaskModel → GroundingGap → 观测 → 重规划反馈。
	if memory != nil {
		if err := writePlanRecall(ctx, b, memory, sCtx, cognitive); err != nil {
			return nil, err
		}
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
	// 观察—再规划（决策八）：已执行轮次的结果，规划下一步而非重复。
	// 重规划闭环（决策六）：告诉模型上一版为何被拒 / 为何未达成，避免原样重来。
	fsm.WriteObservations(b, sCtx)
	fsm.WriteReplanFeedback(b, sCtx)

	return fsm.FinishLayered(b, memory), nil
}

// writePlanRecall 规划阶段的记忆召回（从 BuildPlanContext 拆出以控制圈复杂度，语义不变）：
// 召回段落入 L4 首位。
func writePlanRecall(
	ctx context.Context, b *prompt.PromptBuilder, memory protocol.MemoryFacade, sCtx *fsm.StateContext, cognitive fsm.CognitiveSearcher) error {
	var queryStr string
	if sCtx.TaskModel != nil {
		queryStr = sCtx.TaskModel.Goal
	}
	// 复用本回合 Perceive 的召回：情景记忆已查过则不再查，只用已解析的 Goal 补 反思/L2/RAG；
	// Perceive 被跳过或召回被放弃、且 Goal 非空时才补查情景（ADR-0105 决策四）。
	recalled, err := turnRecallText(ctx, memory, cognitive, sCtx, recallWant{
		episodicQuery: queryStr,
		episodicK:     planEpisodicK,
		goal:          queryStr,
	}, "BuildPlanContext")
	if err != nil {
		return err
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
	return nil
}

// BuildToolListSection 已迁移至 tool_list_section.go（R7 文件行数治理，S-02/S-03
// 改造导致本文件超出 400 行上限，按职责拆分：工具目录格式化与 Perceive/Plan/
// Reflect 上下文组装是两个不同职责）。

func BuildReflectContext(ctx context.Context, memory protocol.MemoryFacade, sCtx *fsm.StateContext) ([]types.Message, error) {
	b := prompt.NewPromptBuilder()

	// L1/L2 与其它阶段共用（此前 Reflect 不带核心记忆与历史，前缀与 Perceive/Plan 完全不同）。
	if err := fsm.WriteSessionLayer(ctx, b, memory, sCtx); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "BuildReflectContext", err)
	}
	fsm.WriteConversationHistory(b, sCtx)

	// L3：阶段契约。
	fsm.WritePhaseContract(b, memory, "kernel/reflect.md", "Reflect on the execution result and evaluate the completion of the goal.")

	// L4：没有目标就无从判定 GoalAchieved：此前反思只看到执行结果。
	if sCtx.TaskModel != nil && sCtx.TaskModel.Goal != "" {
		b.WriteUserData(taint.NewTaintedString("Task Goal: "+sCtx.TaskModel.Goal,
			taint.TaintSource{OriginTaintLevel: types.PropagateTaint(types.TaintMedium, sCtx.GlobalTaintLevel)},
			"m4_task_model"))
	}
	// 执行结果按观察上限投影（≤ObservationMaxBytes，带 read_tool_ref 取回提示），
	// 不再全文注入 8KB 的 ExecuteResult（ADR-0105 决策四，与 Plan/Respond 同口径）。
	if execResult := fsm.ExecuteResultForPrompt(sCtx); len(execResult) > 0 {
		b.WriteUserData(taint.NewTaintedString(
			"Execution Result Summary:\n"+string(execResult)+"\n\n",
			taint.TaintSource{OriginTaintLevel: types.PropagateTaint(types.TaintMedium, sCtx.GlobalTaintLevel)},
			"execute_result"))
	}
	if len(sCtx.ExecuteImageParts) > 0 {
		b.WriteUserImages(sCtx.ExecuteImageParts)
	}

	return fsm.FinishLayered(b, memory), nil
}
