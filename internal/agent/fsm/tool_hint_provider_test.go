package fsm

import (
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

// mockToolHintProvider 供测试模拟 action.PolicyEvolver 的
// BuildSystemHintBlock() 输出（2026-07-12 unwired-code-audit 补齐：
// PolicyEvolver 读侧此前无任何调用方消费）。
type mockToolHintProvider struct {
	hint string
}

func (m *mockToolHintProvider) BuildSystemHintBlock() string { return m.hint }

func TestAppendToolHints_NilProvider_NoOp(t *testing.T) {
	sm := &StateMachine{}
	msgs := []types.Message{{Role: "system", Content: "base"}}
	sm.appendToolHints(msgs)
	if msgs[0].Content != "base" {
		t.Fatalf("nil provider 不应修改消息内容，实际 %q", msgs[0].Content)
	}
}

func TestAppendToolHints_EmptyHint_NoOp(t *testing.T) {
	sm := &StateMachine{}
	sm.WithToolHintProvider(&mockToolHintProvider{hint: ""})
	msgs := []types.Message{{Role: "system", Content: "base"}}
	sm.appendToolHints(msgs)
	if msgs[0].Content != "base" {
		t.Fatalf("空 hint 不应修改消息内容，实际 %q", msgs[0].Content)
	}
}

func TestAppendToolHints_AppendsToFirstMessage(t *testing.T) {
	sm := &StateMachine{}
	sm.WithToolHintProvider(&mockToolHintProvider{hint: "<tool-hints>...</tool-hints>"})
	msgs := []types.Message{{Role: "system", Content: "base"}}
	sm.appendToolHints(msgs)
	want := "base\n\n<tool-hints>...</tool-hints>"
	if msgs[0].Content != want {
		t.Fatalf("期望 %q，实际 %q", want, msgs[0].Content)
	}
}

func TestAppendToolHints_EmptyMsgs_NoOp(t *testing.T) {
	sm := &StateMachine{}
	sm.WithToolHintProvider(&mockToolHintProvider{hint: "irrelevant"})
	var msgs []types.Message
	sm.appendToolHints(msgs) // 不应 panic
}
