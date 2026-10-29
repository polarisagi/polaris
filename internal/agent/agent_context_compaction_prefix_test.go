package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

func TestCachedPrefixLenAndPinnedPrefixLen(t *testing.T) {
	msgs := layeredOverflowMsgs(10)
	if got := cachedPrefixLen(msgs); got != 4 {
		t.Fatalf("cachedPrefixLen = %d, want 4（最后一个断点在 L2 末）", got)
	}
	if got := pinnedPrefixLen(msgs); got != 4 {
		t.Fatalf("pinnedPrefixLen = %d, want 4", got)
	}
	plain := []types.Message{{Role: "system", Content: "s"}, {Role: "system", Content: "s2"}, {Role: "user", Content: "u"}}
	if cachedPrefixLen(plain) != 0 || pinnedPrefixLen(plain) != 2 {
		t.Fatal("无断点标记时退回开头连续 system")
	}
}

// 硬触发压缩：L0..L2 与 L3 system 原样保留，最后一条用户消息即使超过尾部预算也不被卷进摘要，
// 只有 L0..L2 之后的非 system 回合内容被摘要。
func TestHotPathCompact_KeepsPrefixSystemAndLastMessage(t *testing.T) {
	a := NewAgentWithDefaults("sess-cwm-prefix")
	a.provider = &stubSummaryProvider{summary: "中间内容摘要"}
	in := append(layeredOverflowMsgs(0)[:5:5],
		types.Message{Role: "user", Content: "<<DATA>>" + strings.Repeat("r", 3_000) + "<</DATA>>"},
		types.Message{Role: "user", Content: "本轮意图：" + strings.Repeat("q", 20_000)},
	)
	out := a.hotPathCompact(context.Background(), in, 2)
	for i := 0; i < 5; i++ {
		if out[i].Content != in[i].Content {
			t.Fatalf("msgs[%d]（L0..L3）不得被改写", i)
		}
	}
	last := out[len(out)-1]
	if last.Content != in[len(in)-1].Content || last.Role != "user" {
		t.Fatal("本轮最后一条用户消息必须原样保留在尾部")
	}
	var sawSummary bool
	for _, m := range out {
		if strings.Contains(m.Content, "中间内容摘要") {
			sawSummary = true
		}
		if strings.HasPrefix(m.Content, "<<DATA>>"+strings.Repeat("r", 100)) {
			t.Fatal("中间的回合内容应被摘要替换，不得原样保留")
		}
	}
	if !sawSummary {
		t.Fatal("应出现摘要消息")
	}
}
