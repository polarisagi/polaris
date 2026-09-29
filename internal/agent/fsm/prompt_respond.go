package fsm

import (
	"context"
	"fmt"
	"log/slog"
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

// WriteConversationHistory 把对话历史逐条写入 L2 历史层（ADR-0105 决策二，取代 ADR-0098
// 决策四的单条 <conversation_history> 滑窗消息）。
//
// 历史里既有用户原话也有此前的模型输出，一律按 TaintHigh 进数据区围栏、不得提升为指令：
// 每条消息保持真实 user/assistant 角色，各自独立 Spotlighting（标记只取决于该条内容），
// 因此旧消息字节不随新消息变化，L2 在两次跳窗之间纯追加，可被 Provider 前缀缓存命中。
// 四个阶段调用同一函数、读同一份 ConversationHistory，得到字节相同的 L2。
func WriteConversationHistory(b *prompt.PromptBuilder, sCtx *StateContext) {
	sCtx.Mu.RLock()
	history := sCtx.ConversationHistory
	sCtx.Mu.RUnlock()

	th := config.CurrentThresholds().M4Kernel
	anchor, kept := WindowConversationHistory(history, th.ConversationHistoryMaxMessages, th.ConversationHistoryMaxBytes)
	src := taint.TaintSource{Module: "session", OriginTaintLevel: types.TaintHigh}
	if anchor != "" {
		// 锚定摘要由被丢弃的不可信历史派生，同样是数据：user 角色 + TaintHigh 围栏。
		b.WriteHistoryMessage("user", taint.NewTaintedString(
			"<conversation_summary>\n"+anchor+"\n</conversation_summary>", src, "conversation_summary"))
	}
	for _, m := range kept {
		b.WriteHistoryMessage(m.Role, taint.NewTaintedString(m.Content, src, "conversation_history"))
	}
}

// WriteRespondSections 组装 S_RESPOND 的 L2/L3/L4 内容（L1 会话层由 WriteSessionLayer 写入）。
// 记忆路径（agent/context.BuildRespondContext）与无记忆降级路径共用，两条路径 prompt 同构。
func WriteRespondSections(b *prompt.PromptBuilder, sCtx *StateContext) {
	WriteConversationHistory(b, sCtx) // L2
	WriteKernelInstruction(b, "kernel/respond.md", "Reply to the user's latest message in natural language.")

	sCtx.Mu.RLock()
	rawIntent := sCtx.RawIntentTS
	taskModel := sCtx.TaskModel
	result := sCtx.ExecuteResult
	images := sCtx.ExecuteImageParts
	reflection := sCtx.Reflection
	globalTaint := sCtx.GlobalTaintLevel
	sCtx.Mu.RUnlock()

	// L4 回合层。本轮意图放最后：它是回合内最易变、最靠近生成点的内容。
	// 被拒/失败的尝试也要让回复阶段看到：否则无工具可用而直答时，回复会像什么都没
	// 发生过，甚至声称已完成（ADR-0098 决策六）。
	WriteReplanFeedback(b, sCtx)
	// 观察—再规划（决策八）：多轮执行时据全部观察作答；观察含最后一轮，不再重复单轮结果。
	hasObservations := WriteObservations(b, sCtx)
	if len(result) > 0 || reflection != nil {
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
	if !rawIntent.IsEmpty() {
		b.WriteUserData(rawIntent)
	}
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
	if err := WriteSessionLayer(context.Background(), b, nil, sCtx); err != nil {
		slog.Warn("respond: session layer failed, continuing without it", "err", err)
	}
	WriteRespondSections(b, sCtx)
	msgs := AppendRespondReminder(FinishLayered(b, nil))
	if sCtx.EpochTracker != nil {
		sCtx.ContextEpoch = sCtx.EpochTracker.check(msgs)
	}
	return msgs
}
