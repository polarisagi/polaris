package protocol

import "testing"

// TestWriteToolHints_EmptySkipped 验证空字符串（PolicyEvolver.BuildSystemHintBlock
// 冷启动/无数据时的返回值）不会被写入 Prompt，避免注入无意义噪声。
func TestWriteToolHints_EmptySkipped(t *testing.T) {
	b := NewPromptBuilder()
	b.WriteToolHints("")
	msgs := b.Build()
	if len(msgs) != 0 {
		t.Fatalf("空 hint 不应写入任何消息，实际 %d 条", len(msgs))
	}
}

// TestWriteToolHints_WritesToImmutableZone 验证非空 hint 内容进入 ZoneImmutable
// （2026-07-12 unwired-code-audit 补齐：PolicyEvolver 读侧此前完全无处可写）。
func TestWriteToolHints_WritesToImmutableZone(t *testing.T) {
	b := NewPromptBuilder()
	hint := "<tool-hints>\n  <tool name=\"boom\">FailureWarning: ...</tool>\n</tool-hints>"
	b.WriteToolHints(hint)
	msgs := b.Build()
	if len(msgs) != 1 {
		t.Fatalf("期望写入 1 条消息，实际 %d 条", len(msgs))
	}
	if msgs[0].Role != "system" {
		t.Fatalf("期望 Role=system，实际 %q", msgs[0].Role)
	}
	if msgs[0].Content != hint {
		t.Fatalf("Content 不符预期: %q", msgs[0].Content)
	}
}
