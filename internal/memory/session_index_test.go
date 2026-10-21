package memory

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/polarisagi/polaris/pkg/types"
)

func TestRenderSessionIndex_Empty(t *testing.T) {
	if got := renderSessionIndex(nil, 0, nil); got != "" {
		t.Fatalf("无内容应返回空串: %q", got)
	}
}

func TestRenderSessionIndex_ContentAndBounds(t *testing.T) {
	blocks := []types.CoreMemoryBlock{{BlockKey: "persona", Content: "你好世界"}, {BlockKey: "task_state", Content: "abc"}}
	got := renderSessionIndex(blocks, 42, []string{"Polaris", "迁移\n方案  v2"})
	for _, want := range []string{"persona(4)", "task_state(3)", "42 entities", "Polaris, 迁移 方案 v2", "memory_search", "session_search"} {
		if !strings.Contains(got, want) {
			t.Fatalf("缺少 %q:\n%s", want, got)
		}
	}
	long := make([]string, 12)
	for i := range long {
		long[i] = strings.Repeat("长", 200)
	}
	manyBlocks := make([]types.CoreMemoryBlock, 300)
	for i := range manyBlocks {
		manyBlocks[i] = types.CoreMemoryBlock{BlockKey: strings.Repeat("k", 20), Content: "x"}
	}
	if n := utf8.RuneCountInString(renderSessionIndex(manyBlocks, 9999, long)); n > sessionIndexMaxRunes {
		t.Fatalf("超过上限: %d", n)
	}
}
