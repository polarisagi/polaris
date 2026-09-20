package compact

import (
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

// GD-14-001：开头连续 system 消息必须被切为固定前缀，不进入摘要。
func TestSplitPinnedHead(t *testing.T) {
	msgs := []types.Message{
		{Role: "system", Content: "kernel"},
		{Role: "system", Content: "tools"},
		{Role: "user", Content: "hi"},
		{Role: "system", Content: "late"},
	}
	head, rest := SplitPinnedHead(msgs)
	if len(head) != 2 || len(rest) != 2 || rest[0].Role != "user" {
		t.Fatalf("head=%v rest=%v", head, rest)
	}
	if h, r := SplitPinnedHead([]types.Message{{Role: "user"}}); len(h) != 0 || len(r) != 1 {
		t.Fatal("no system prefix should yield empty head")
	}
}
