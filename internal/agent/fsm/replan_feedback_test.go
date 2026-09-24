package fsm

import (
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/prompt"
)

func TestRecordReplanFeedback_Bounded(t *testing.T) {
	s := &StateContext{}
	for _, r := range []string{"a", "b", "c", "d"} {
		s.RecordReplanFeedback(r)
	}
	if len(s.ReplanFeedback) != maxReplanFeedback || s.ReplanFeedback[0] != "b" {
		t.Fatalf("应只保留最近 %d 条，得到 %v", maxReplanFeedback, s.ReplanFeedback)
	}
	s.RecordReplanFeedback(strings.Repeat("长", 1000))
	if last := s.ReplanFeedback[len(s.ReplanFeedback)-1]; len(last) > maxReplanFeedbackBytes {
		t.Fatalf("单条应截断到 %d 字节，得到 %d", maxReplanFeedbackBytes, len(last))
	}
}

// TestWriteReplanFeedback 复现 2026-09-25 实测：重规划 prompt 与首次相同，模型反复
// 选择被 L1_taint 拒绝的 bash 直到 ReplanGuard 耗尽（ADR-0098 决策六）。
func TestWriteReplanFeedback(t *testing.T) {
	empty := prompt.NewPromptBuilder()
	WriteReplanFeedback(empty, &StateContext{})
	if len(empty.Build()) != 0 {
		t.Fatal("无失败记录时不应写入任何内容")
	}

	b := prompt.NewPromptBuilder()
	s := &StateContext{}
	s.RecordReplanFeedback(`rejected by L1_taint for tool "bash": TaintHigh args blocked`)
	WriteReplanFeedback(b, s)
	var all strings.Builder
	for _, m := range b.Build() {
		all.WriteString(m.Content)
	}
	if !strings.Contains(all.String(), "previous_attempts_failed") || !strings.Contains(all.String(), `"bash"`) {
		t.Fatalf("重规划 prompt 必须携带上次被拒原因: %q", all.String())
	}
}
