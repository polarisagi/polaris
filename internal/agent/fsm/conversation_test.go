package fsm

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/polarisagi/polaris/pkg/types"
)

func TestRenderConversationHistory_Empty(t *testing.T) {
	if got := RenderConversationHistory(nil, 20, 1024); got != "" {
		t.Fatalf("空历史应渲染为空串，得到 %q", got)
	}
}

func TestRenderConversationHistory_SkipsSystemAndBlank(t *testing.T) {
	h := []types.Message{
		{Role: "system", Content: "你是 Polaris"},
		{Role: "user", Content: "你好"},
		{Role: "assistant", Content: "   "},
		{Role: "assistant", Content: "你好！"},
	}
	got := RenderConversationHistory(h, 20, 1024)
	if strings.Contains(got, "Polaris") {
		t.Fatalf("system 角色不得进入对话历史: %q", got)
	}
	want := "[user]\n你好\n\n[assistant]\n你好！"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRenderConversationHistory_KeepsTailByCount(t *testing.T) {
	h := make([]types.Message, 0, 4)
	for _, c := range []string{"m1", "m2", "m3", "m4"} {
		h = append(h, types.Message{Role: "user", Content: c})
	}
	got := RenderConversationHistory(h, 2, 1024)
	if strings.Contains(got, "m1") || strings.Contains(got, "m2") {
		t.Fatalf("超出条数上限应丢弃最早的消息: %q", got)
	}
	if !strings.Contains(got, "m3") || !strings.Contains(got, "m4") {
		t.Fatalf("应保留最近的消息: %q", got)
	}
}

func TestRenderConversationHistory_ByteCapKeepsNewestTail(t *testing.T) {
	old := strings.Repeat("旧", 200)
	newest := strings.Repeat("新", 50) + "END"
	h := []types.Message{
		{Role: "user", Content: old},
		{Role: "assistant", Content: newest},
	}
	const maxBytes = 200
	got := RenderConversationHistory(h, 20, maxBytes)
	if len(got) > maxBytes {
		t.Fatalf("渲染结果 %d 字节，超过上限 %d", len(got), maxBytes)
	}
	if !strings.HasSuffix(got, "END") {
		t.Fatalf("字节截断必须保留最新内容的结尾: %q", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("截断不得切断多字节字符: %q", got)
	}
}

func TestRenderConversationHistory_Deterministic(t *testing.T) {
	h := []types.Message{{Role: "user", Content: "a"}, {Role: "assistant", Content: "b"}}
	first := RenderConversationHistory(h, 20, 1024)
	if second := RenderConversationHistory(h, 20, 1024); first != second {
		t.Fatal("par_inv_03：同输入必须产出同字节")
	}
}
