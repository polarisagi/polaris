package fsm

import (
	"strings"

	"github.com/polarisagi/polaris/internal/prompt"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

const (
	maxReplanFeedback      = 3
	maxReplanFeedbackBytes = 400
)

// RecordReplanFeedback 记录一次规划被拒 / 执行失败的原因（ADR-0098 决策六）。
//
// 此前重规划 prompt 与首次完全相同，模型不知道上一版为何被拒，反复产出同一计划
// 直到 ReplanGuard 耗尽。只保留最近几条并截断：原因来自 Go 侧校验/执行错误，
// 但可能夹带工具输出片段，不能无界灌进 prompt。
func (s *StateContext) RecordReplanFeedback(reason string) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return
	}
	if len(reason) > maxReplanFeedbackBytes {
		reason = strings.ToValidUTF8(reason[:maxReplanFeedbackBytes], "")
	}
	s.Mu.Lock()
	defer s.Mu.Unlock()
	s.ReplanFeedback = append(s.ReplanFeedback, reason)
	if n := len(s.ReplanFeedback); n > maxReplanFeedback {
		s.ReplanFeedback = s.ReplanFeedback[n-maxReplanFeedback:]
	}
}

// WriteReplanFeedback 把失败原因写进数据区（S_PLAN 与 S_RESPOND 共用）。
// 错误文本可能含工具名、参数片段等模型产出，按 TaintMedium 围栏，不提升为指令；
// "不得重复被拒方案"的指令本身在 kernel/plan.md（TaintNone）。
func WriteReplanFeedback(b *prompt.PromptBuilder, sCtx *StateContext) {
	sCtx.Mu.RLock()
	feedback := append([]string(nil), sCtx.ReplanFeedback...)
	global := sCtx.GlobalTaintLevel
	sCtx.Mu.RUnlock()
	if len(feedback) == 0 {
		return
	}
	b.WriteUserData(taint.NewTaintedString(
		"<previous_attempts_failed>\n- "+strings.Join(feedback, "\n- ")+"\n</previous_attempts_failed>",
		taint.TaintSource{Module: "replan_feedback", OriginTaintLevel: types.PropagateTaint(types.TaintMedium, global)},
		"replan_feedback"))
}
