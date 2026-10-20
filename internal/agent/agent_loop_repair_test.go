package agent

import (
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

func assistantToolUse(round int, ids ...string) types.Message {
	parts := make([]any, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, map[string]any{"type": "tool_use", "id": id, "name": "bash", "input": map[string]any{}})
	}
	return types.Message{Role: "assistant", Parts: parts, Round: round}
}

// 校验协议不变量：每个 assistant.tool_use 之后紧跟齐全的 tool_result，且没有孤儿 tool_result。
func assertToolCallsClosed(t *testing.T, msgs []types.Message) {
	t.Helper()
	for i := 0; i < len(msgs); i++ {
		if msgs[i].Role != "assistant" {
			if isToolResultMessage(msgs[i]) {
				t.Fatalf("orphan tool_result at %d", i)
			}
			continue
		}
		ids := extractToolUseIDs(msgs[i].Parts)
		if len(ids) == 0 {
			continue
		}
		j := i + 1
		have := map[string]bool{}
		for ; j < len(msgs) && isToolResultMessage(msgs[j]); j++ {
			for id := range extractToolResultIDs(msgs[j : j+1]) {
				have[id] = true
			}
		}
		for _, id := range ids {
			if !have[id] {
				t.Fatalf("assistant at %d: tool_use %s has no tool_result right after it: %+v", i, id, msgs)
			}
		}
		i = j - 1
	}
}

func TestRepairDanglingToolCalls(t *testing.T) {
	user := types.Message{Role: "user", Content: "hi"}
	steer := types.Message{Role: "user", Content: "插话"}
	cases := []struct {
		name string
		in   []types.Message
		want int // 期望消息总数
	}{
		{"dangling at tail", []types.Message{user, assistantToolUse(1, "a", "b")}, 4},
		{"partial results at tail", []types.Message{user, assistantToolUse(1, "a", "b"), makeToolResultMessage("a", "ok")}, 4},
		{"dangling followed by steer (result must precede steer)", []types.Message{user, assistantToolUse(1, "a"), steer}, 4},
		{"dangling in the middle", []types.Message{user, assistantToolUse(1, "a"), assistantToolUse(2, "b"), makeToolResultMessage("b", "ok")}, 5},
		{"complete history untouched", []types.Message{user, assistantToolUse(1, "a"), makeToolResultMessage("a", "ok"), {Role: "assistant", Content: "done"}}, 4},
		{"orphan tool_result degraded to user text", []types.Message{user, makeToolResultMessage("zzz", "stale"), {Role: "assistant", Content: "x"}}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RepairDanglingToolCalls(tc.in)
			if len(got) != tc.want {
				t.Fatalf("len=%d want %d: %+v", len(got), tc.want, got)
			}
			assertToolCallsClosed(t, got)
		})
	}
}

func TestRepairDanglingToolCalls_SynthesizedResultText(t *testing.T) {
	got := RepairDanglingToolCalls([]types.Message{{Role: "user", Content: "hi"}, assistantToolUse(3, "a")})
	last := got[len(got)-1]
	if last.Round != 3 || last.Content != interruptedToolResultText {
		t.Fatalf("synthesized result should carry the interrupted text and the assistant's round: %+v", last)
	}
}

func TestResumeAgenticLoop_RepairsHistory(t *testing.T) {
	ag := NewAgentWithDefaults("sess-resume-repair")
	msgs := []types.Message{{Role: "user", Content: "hi"}, assistantToolUse(1, "a"), {Role: "user", Content: "插话"}}
	if !ag.ResumeAgenticLoop("turn-1", msgs, types.TaintNone, 1) {
		t.Fatal("resume should accept history")
	}
	ag.sCtx.Mu.RLock()
	defer ag.sCtx.Mu.RUnlock()
	assertToolCallsClosed(t, ag.sCtx.LoopMessages)
}
