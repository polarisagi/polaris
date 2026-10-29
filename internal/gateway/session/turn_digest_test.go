package session

import (
	"context"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

type fakeDigestTurns struct {
	TurnStore
	steps map[string][]Step
}

func (f fakeDigestTurns) SessionToolSteps(context.Context, string) (map[string][]Step, error) {
	return f.steps, nil
}

func TestWithTurnDigests_AppendsToOwningAssistantOnly(t *testing.T) {
	o := &orchestrator{turns: fakeDigestTurns{steps: map[string][]Step{
		"t1": {{Tool: "read_file", ArgsPreview: `{"path":"/a/main.go"}`, Status: StepStatusOK, ResultPreview: "package main"}},
	}}}
	hist := []types.Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1", TurnID: "t1"},
		{Role: "user", Content: "q2"},
		{Role: "assistant", Content: "a2", TurnID: "t2"},
	}
	out := o.withTurnDigests(context.Background(), "s", hist)
	if !strings.Contains(out[1].Content, "<turn_digest>") || !strings.Contains(out[1].Content, "read_file(") {
		t.Fatalf("t1 回合应附摘要: %q", out[1].Content)
	}
	if out[3].Content != "a2" || out[0].Content != "q1" {
		t.Fatalf("无步骤的回合与 user 消息不应改动: %+v", out)
	}
	if hist[1].Content != "a1" {
		t.Fatal("不得改写调用方的历史切片")
	}
	again := o.withTurnDigests(context.Background(), "s", hist)
	if again[1].Content != out[1].Content {
		t.Fatal("摘要必须确定性（前缀缓存依赖字节稳定）")
	}
}

func TestBuildTurnDigest_Bounded(t *testing.T) {
	var steps []Step
	for i := 0; i < 100; i++ {
		steps = append(steps, Step{Tool: "grep", ArgsPreview: strings.Repeat("p", 200), Status: StepStatusOK})
	}
	d := buildTurnDigest(steps)
	if !strings.Contains(d, "未列出") {
		t.Fatalf("超长应注明省略: %q", d)
	}
	if n := len([]rune(d)); n > turnDigestMaxChars+120 {
		t.Fatalf("摘要过长: %d", n)
	}
}
