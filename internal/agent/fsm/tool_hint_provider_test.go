package fsm

import (
	"strings"
	"testing"
)

// mockToolHintProvider 供测试模拟 action.PolicyEvolver 的
// BuildSystemHintBlock() 输出（2026-07-12 unwired-code-audit 补齐：
// PolicyEvolver 读侧此前无任何调用方消费）。
type mockToolHintProvider struct {
	hint string
}

func (m *mockToolHintProvider) BuildSystemHintBlock() string { return m.hint }

func TestPlanHintBlocks_NilProvider_Empty(t *testing.T) {
	sm := &StateMachine{}
	if blocks := sm.planHintBlocks(); len(blocks) != 0 {
		t.Fatalf("nil provider 且无动态提示时应无提示块，实际 %v", blocks)
	}
}

func TestPlanHintBlocks_EmptyHint_Empty(t *testing.T) {
	sm := &StateMachine{}
	sm.WithToolHintProvider(&mockToolHintProvider{hint: ""})
	if blocks := sm.planHintBlocks(); len(blocks) != 0 {
		t.Fatalf("空 hint 不应产生提示块，实际 %v", blocks)
	}
}

func TestPlanHintBlocks_ToolHintsAndDynamic(t *testing.T) {
	sm := &StateMachine{}
	sm.WithToolHintProvider(&mockToolHintProvider{hint: "<tool-hints>...</tool-hints>"})
	sm.dynamicHints = []ExtActivatedHint{{ToolName: "t1", Description: "d1"}}
	blocks := sm.planHintBlocks()
	if len(blocks) != 2 {
		t.Fatalf("期望 2 个提示块，实际 %v", blocks)
	}
	if !strings.Contains(blocks[0], "t1") || blocks[1] != "<tool-hints>...</tool-hints>" {
		t.Fatalf("提示块内容/顺序错误: %v", blocks)
	}
}
