package fsm

import (
	"fmt"
	"strings"

	"github.com/polarisagi/polaris/configs"
	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/prompt"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

// WriteKernelInstruction 把 configs/prompts/kernel/<name> 写入指令区。阶段契约的
// 唯一来源是这些模板（ADR-0098 决策三）：此前记忆路径各自内联一句话、不带 Schema，
// 模型只能自编输出格式，解析必然失败。模板随二进制嵌入，读失败即构建缺陷，
// 此时回退 fallback 保证 prompt 至少携带阶段意图。
func WriteKernelInstruction(b *prompt.PromptBuilder, name, fallback string) {
	tmpl, err := configs.LoadPromptTemplate(name, nil)
	if err != nil {
		tmpl = fallback
	}
	safe, _ := taint.SanitizeToSafe(taint.NewTaintedString(
		tmpl, taint.TaintSource{OriginTaintLevel: types.TaintNone}, "system_prompt"))
	b.WriteInstruction(safe)
}

// WriteConversationHistory 写入有界对话历史（ADR-0098 决策四）。历史里既有用户
// 原话也有此前的模型输出，一律按 TaintHigh 进数据区围栏，不得提升为指令。
func WriteConversationHistory(b *prompt.PromptBuilder, sCtx *StateContext) {
	sCtx.Mu.RLock()
	history := sCtx.ConversationHistory
	sCtx.Mu.RUnlock()

	th := config.CurrentThresholds().M4Kernel
	rendered := RenderConversationHistory(history, th.ConversationHistoryMaxMessages, th.ConversationHistoryMaxBytes)
	if rendered == "" {
		return
	}
	b.WriteUserData(taint.NewTaintedString(
		"<conversation_history>\n"+rendered+"\n</conversation_history>",
		taint.TaintSource{Module: "session", OriginTaintLevel: types.TaintHigh},
		"conversation_history"))
}

// WriteRespondSections 组装 S_RESPOND 的指令与数据区。记忆路径
// （agent/context.BuildRespondContext）与无记忆降级路径共用，两条路径 prompt 同构。
func WriteRespondSections(b *prompt.PromptBuilder, sCtx *StateContext) {
	WriteKernelInstruction(b, "kernel/respond.md", "Reply to the user's latest message in natural language.")
	WriteConversationHistory(b, sCtx)

	sCtx.Mu.RLock()
	rawIntent := sCtx.RawIntentTS
	taskModel := sCtx.TaskModel
	result := sCtx.ExecuteResult
	images := sCtx.ExecuteImageParts
	reflection := sCtx.Reflection
	globalTaint := sCtx.GlobalTaintLevel
	sCtx.Mu.RUnlock()

	if !rawIntent.IsEmpty() {
		b.WriteUserData(rawIntent)
	}
	// 被拒/失败的尝试也要让回复阶段看到：否则无工具可用而直答时，回复会像什么都没
	// 发生过，甚至声称已完成（ADR-0098 决策六）。
	WriteReplanFeedback(b, sCtx)
	// 观察—再规划（决策八）：多轮执行时据全部观察作答；观察含最后一轮，不再重复单轮结果。
	hasObservations := WriteObservations(b, sCtx)
	if len(result) == 0 && reflection == nil {
		return
	}

	// 执行结果来自工具输出，至少 TaintMedium 且不低于会话已累积污点（L-03）。
	dataTaint := types.PropagateTaint(types.TaintMedium, globalTaint)
	var sb strings.Builder
	if taskModel != nil && taskModel.Goal != "" {
		sb.WriteString("<task_goal>\n" + taskModel.Goal + "\n</task_goal>\n")
	}
	if len(result) > 0 && !hasObservations {
		sb.WriteString("<execution_result>\n" + string(result) + "\n</execution_result>\n")
	}
	if reflection != nil {
		fmt.Fprintf(&sb, "<reflection>\nGoalAchieved: %t\nErrors: %s\n</reflection>\n",
			reflection.GoalAchieved, strings.Join(reflection.Errors, "; "))
	}
	b.WriteUserData(taint.NewTaintedString(sb.String(),
		taint.TaintSource{Module: "execute", OriginTaintLevel: dataTaint}, "execute_result"))
	b.WriteUserImages(images)
}

// AppendRespondReminder 在回复 prompt 最末追加收尾提醒（kernel/respond_reminder.md）。
//
// ImmutableCore 人格里写着"有工具就立即调用"并附工具清单，回复阶段却不挂工具：
// 模型在信息不足时会以文本形式"调用"工具（2026-09-25 实测输出 `<tool_calls>` 标记）。
// 契约已在 respond.md 声明，这里利用位置优势在末尾重申；只追加静态模板，不改人格。
func AppendRespondReminder(msgs []types.Message) []types.Message {
	reminder, err := configs.LoadPromptTemplate("kernel/respond_reminder.md", nil)
	if err != nil || strings.TrimSpace(reminder) == "" {
		return msgs
	}
	return append(msgs, types.Message{Role: "system", Content: strings.TrimSpace(reminder)})
}

// promptRespond S_RESPOND 的 PromptFn。有记忆系统时交给 ContextBuilder 注入人格、
// 用户偏好与核心记忆；否则走同构的降级组装。
func (sm *StateMachine) promptRespond(sCtx *StateContext, pCtx protocol.StateContext) []types.Message {
	if pCtx.Mem != nil {
		ctx, cancel := sm.bgCtx()
		defer cancel()
		if msgs, err := sm.cb.BuildRespondContext(ctx, pCtx.Mem, sCtx); err == nil {
			return msgs
		}
	}

	b := prompt.NewPromptBuilder()
	if sCtx.SysEnvSnapshot != "" {
		b.WriteSystemEnvironment(sCtx.SysEnvSnapshot)
	}
	WriteRespondSections(b, sCtx)
	msgs := AppendRespondReminder(b.Build())
	if sCtx.EpochTracker != nil {
		sCtx.ContextEpoch = sCtx.EpochTracker.check(msgs)
	}
	return msgs
}
