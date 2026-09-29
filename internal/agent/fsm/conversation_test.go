package fsm

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/polarisagi/polaris/pkg/types"
)

func numberedHistory(n int) []types.Message {
	h := make([]types.Message, 0, n)
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		h = append(h, types.Message{Role: role, Content: fmt.Sprintf("msg-%03d", i)})
	}
	return h
}

func TestWindowConversationHistory_Empty(t *testing.T) {
	anchor, kept := WindowConversationHistory(nil, 20, 1024)
	if anchor != "" || len(kept) != 0 {
		t.Fatalf("空历史应无输出: %q %v", anchor, kept)
	}
}

func TestWindowConversationHistory_SkipsSystemAndBlankKeepsRoles(t *testing.T) {
	h := []types.Message{
		{Role: "system", Content: "You are Polaris"},
		{Role: "user", Content: "你好"},
		{Role: "assistant", Content: "   "},
		{Role: "assistant", Content: "你好！", ReasoningContent: "思考链不应带入"},
		{Role: "tool", Content: "裸 tool 角色"},
	}
	anchor, kept := WindowConversationHistory(h, 20, 1024)
	if anchor != "" {
		t.Fatalf("未越阈值不应有锚定摘要: %q", anchor)
	}
	if len(kept) != 3 {
		t.Fatalf("应保留 3 条，got %d: %+v", len(kept), kept)
	}
	if kept[0].Role != "user" || kept[1].Role != "assistant" || kept[2].Role != "user" {
		t.Fatalf("角色映射错误（tool 应降为 user）: %+v", kept)
	}
	for _, m := range kept {
		if m.ReasoningContent != "" || strings.Contains(m.Content, "Polaris") {
			t.Fatalf("历史不得携带 ReasoningContent/system 内容: %+v", m)
		}
	}
}

// 跳窗一次丢掉窗口前半，而不是逐条滑动。
func TestWindowConversationHistory_BlockSkipByCount(t *testing.T) {
	// maxMessages=20 → 步长 10：n=21..30 丢 10 条；n=31 起丢 20 条。
	for n := 21; n <= 30; n++ {
		anchor, kept := WindowConversationHistory(numberedHistory(n), 20, 0)
		if !strings.Contains(anchor, "较早的 10 条") {
			t.Fatalf("n=%d 应丢弃 10 条: %q", n, anchor)
		}
		if len(kept) != n-10 || kept[0].Content != "msg-010" {
			t.Fatalf("n=%d kept=%d first=%q", n, len(kept), kept[0].Content)
		}
	}
	anchor, kept := WindowConversationHistory(numberedHistory(31), 20, 0)
	if !strings.Contains(anchor, "较早的 20 条") || len(kept) != 11 || kept[0].Content != "msg-020" {
		t.Fatalf("n=31 应再跳一块: anchor=%q kept=%d", anchor, len(kept))
	}
}

// 相邻回合（历史只追加）在未越过阈值时，前者的 L2 是后者 L2 的前缀；锚定摘要跳窗前不变。
func TestWindowConversationHistory_AppendOnlyPrefix(t *testing.T) {
	for _, limits := range [][2]int{{20, 0}, {20, 4096}, {6, 0}} {
		prevAnchor, prevKept := WindowConversationHistory(numberedHistory(1), limits[0], limits[1])
		for n := 2; n <= 200; n++ {
			anchor, kept := WindowConversationHistory(numberedHistory(n), limits[0], limits[1])
			if anchor == prevAnchor {
				// 未跳窗：旧序列必须是新序列的前缀。
				if len(kept) < len(prevKept) {
					t.Fatalf("limits=%v n=%d 未跳窗但序列缩短", limits, n)
				}
				for i := range prevKept {
					if kept[i].Role != prevKept[i].Role || kept[i].Content != prevKept[i].Content {
						t.Fatalf("limits=%v n=%d 未跳窗但第 %d 条字节变化", limits, n, i)
					}
				}
			}
			prevAnchor, prevKept = anchor, kept
		}
	}
}

// 跳窗次数必须远少于回合数（每 maxMessages/2 条才跳一次），否则缓存收益为零。
func TestWindowConversationHistory_JumpFrequency(t *testing.T) {
	jumps := 0
	prev, _ := WindowConversationHistory(numberedHistory(1), 20, 0)
	for n := 2; n <= 200; n++ {
		a, _ := WindowConversationHistory(numberedHistory(n), 20, 0)
		if a != prev {
			jumps++
		}
		prev = a
	}
	if jumps > 200/10+1 {
		t.Fatalf("跳窗次数 %d 过多", jumps)
	}
}

func TestWindowConversationHistory_AnchorBounded(t *testing.T) {
	anchor, _ := WindowConversationHistory(numberedHistory(500), 20, 0)
	if strings.Count(anchor, "\n- ") > historyAnchorMaxItems+1 {
		t.Fatalf("锚定摘要条目应有上限: %q", anchor)
	}
	if !strings.Contains(anchor, "msg-") {
		t.Fatalf("锚定摘要应含被丢弃消息的首行: %q", anchor)
	}
}

func TestWindowConversationHistory_ByteCapKeepsNewest(t *testing.T) {
	old := strings.Repeat("旧", 200)
	newest := strings.Repeat("新", 50) + "END"
	h := []types.Message{
		{Role: "user", Content: old},
		{Role: "assistant", Content: newest},
	}
	const maxBytes = 200
	_, kept := WindowConversationHistory(h, 20, maxBytes)
	if len(kept) == 0 {
		t.Fatal("至少保留最后一条")
	}
	last := kept[len(kept)-1].Content
	if len(last) > maxBytes || !strings.HasSuffix(last, "END") {
		t.Fatalf("单条超限应保留结尾且不超上限: %d %q", len(last), last)
	}
	for _, m := range kept {
		if !utf8.ValidString(m.Content) {
			t.Fatalf("截断不得切断多字节字符: %q", m.Content)
		}
	}
}

func TestWindowConversationHistory_Deterministic(t *testing.T) {
	h := numberedHistory(45)
	a1, k1 := WindowConversationHistory(h, 20, 512)
	a2, k2 := WindowConversationHistory(h, 20, 512)
	if a1 != a2 || len(k1) != len(k2) {
		t.Fatal("par_inv_03：同输入必须产出同字节")
	}
	for i := range k1 {
		if k1[i].Role != k2[i].Role || k1[i].Content != k2[i].Content {
			t.Fatal("par_inv_03：同输入必须产出同字节")
		}
	}
}
