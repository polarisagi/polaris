package fsm

import (
	"fmt"
	"strings"

	"github.com/polarisagi/polaris/internal/prompt"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

const (
	maxObservations     = 4
	maxObservationBytes = 4096
)

// RecordObservation 记录一轮执行结果（ADR-0098 决策八）。
//
// 观察—再规划循环里 ExecuteResult 每轮被覆盖；下一轮规划要知道"已经做过什么、
// 拿到了什么"才能规划下一步而非重复，回复也要据全部观察作答。有界：最近 4 轮、
// 每轮 ≤4KB，与单轮 ExecuteResult 的截断策略同向（保留开头，工具输出结论多在前部）。
func (s *StateContext) RecordObservation(result string) {
	result = strings.TrimSpace(result)
	if result == "" {
		return
	}
	if len(result) > maxObservationBytes {
		result = strings.ToValidUTF8(result[:maxObservationBytes], "") + "\n...[truncated]"
	}
	s.Mu.Lock()
	defer s.Mu.Unlock()
	s.Observations = append(s.Observations, result)
	if n := len(s.Observations); n > maxObservations {
		s.Observations = s.Observations[n-maxObservations:]
	}
}

// WriteObservations 写入本回合已获得的观察（S_PLAN 与 S_RESPOND 共用）。工具输出
// 属外部数据：至少 TaintMedium 且不低于会话累积污点（L-03）。
func WriteObservations(b *prompt.PromptBuilder, sCtx *StateContext) bool {
	sCtx.Mu.RLock()
	obs := append([]string(nil), sCtx.Observations...)
	global := sCtx.GlobalTaintLevel
	sCtx.Mu.RUnlock()
	if len(obs) == 0 {
		return false
	}
	var sb strings.Builder
	sb.WriteString("<observations>\n")
	for i, o := range obs {
		fmt.Fprintf(&sb, "[round %d]\n%s\n", i+1, o)
	}
	sb.WriteString("</observations>")
	b.WriteUserData(taint.NewTaintedString(sb.String(),
		taint.TaintSource{Module: "execute", OriginTaintLevel: types.PropagateTaint(types.TaintMedium, global)},
		"turn_observations"))
	return true
}
