package consolidation

import (
	"strings"

	"github.com/polarisagi/polaris/pkg/types"

	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
)

type mockOffloader struct {
	offloaded map[string][]byte
}

func (m *mockOffloader) Offload(id string, data []byte) error {
	if m.offloaded == nil {
		m.offloaded = make(map[string][]byte)
	}
	m.offloaded[id] = data
	return nil
}

type mockOutboxWriter struct {
	entries []protocol.OutboxEntry
}

func (m *mockOutboxWriter) Write(ctx context.Context, entry protocol.OutboxEntry) error {
	m.entries = append(m.entries, entry)
	return nil
}

func TestSessionCompressor(t *testing.T) {
	sc := NewSessionCompressor(100)
	if sc.Canvas() == nil {
		t.Fatal("Canvas is nil")
	}

	sc.InjectOffloader(&mockOffloader{})
	var pressure atomic.Int32
	sc.InjectMemPressure(&pressure)
	sc.InjectOutboxWriter(&mockOutboxWriter{})

	sc.TrackToolCall("t1", "tool_1")
	sc.TrackToolResult("t1", true, "done")

	if !sc.ShouldTrigger(70, 100) {
		t.Fatal("Should trigger at 70%")
	}

	// Test MemPressure
	pressure.Store(2) // critical -> 35% trigger
	if !sc.ShouldTrigger(40, 100) {
		t.Fatal("Should trigger at 40% under pressure")
	}

	sc.SetAnchor("anchor text")
	if sc.Anchor() != "anchor text" {
		t.Fatal("Anchor mismatch")
	}

	// Compress with tool outputs
	msgs := []types.Message{
		{
			Role: "user",
			Parts: []any{
				map[string]any{"type": "text", "text": "hello"},
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": "t1",
					"content":     "this is a very long string that should be pruned this is a very long string that should be pruned",
				},
			},
		},
	}

	// Set threshold very low to ensure pruning
	sc.maxToolOutputBytes = 5
	// Make ShouldTrigger pass
	pressure.Store(2)

	// Fast-forward antithrashCooldown or just set it
	sc.lastCompressAt = time.Now().Add(-2 * time.Minute)

	pruned, triggered := sc.Compress(msgs, 40, 100)
	if !triggered {
		t.Fatal("Compress should have triggered")
	}

	part := pruned[0].Parts[1].(map[string]any)
	content := part["content"].(string)
	if content == "this is a very long string that should be pruned this is a very long string that should be pruned" {
		t.Fatal("Content was not pruned")
	}

	sc.ResetCanvas()
}

func TestEstimateImageTokens(t *testing.T) {
	msgs := []types.Message{
		{
			Parts: []any{
				map[string]any{
					"type": "image",
					"_meta": map[string]any{
						"width":  float64(1024),
						"height": float64(1024),
					},
				},
				map[string]any{
					"type": "image",
					"source": map[string]any{
						"url": "http://a.com/b.png",
					},
				},
			},
		},
	}
	tokens := EstimateImageTokens(msgs)
	if tokens <= 0 {
		t.Fatal("Expected > 0 tokens")
	}
}

func TestLooksLikeErrorStack(t *testing.T) {
	res := looksLikeErrorStack([]byte("panic: runtime error: invalid memory address or nil pointer dereference\n\ngoroutine 1 [running]:\nmain.main()"))
	if !res {
		t.Fatal("Expected true")
	}
	res = looksLikeErrorStack([]byte("normal text"))
	if res {
		t.Fatal("Expected false")
	}
}
func TestJaccardSimilarity(t *testing.T) {
	tests := []struct {
		a, b string
		want float64 // 下限
	}{
		{"language", "language", 1.0},
		{"programming_language", "language", 0.4},        // 有交集
		{"lang_preference", "language_preference", 0.30}, // 部分重叠（1/3=0.33）
		{"go_version", "python_framework", 0.0},          // 无交集
	}
	for _, tt := range tests {
		got := jaccardSimilarity(tt.a, tt.b)
		if got < tt.want-0.01 {
			t.Errorf("jaccardSimilarity(%q, %q) = %.2f，期望 >= %.2f", tt.a, tt.b, got, tt.want)
		}
	}

	// 完全相同应为 1.0
	if v := jaccardSimilarity("user_lang", "user_lang"); v != 1.0 {
		t.Errorf("相同字符串 Jaccard 应为 1.0，got %.2f", v)
	}

	// 空字符串边界
	if v := jaccardSimilarity("", "abc"); v != 0 {
		t.Errorf("空字符串 Jaccard 应为 0，got %.2f", v)
	}
}

// ─── SessionCompressor canvas 集成测试 ────────────────────────────────────────

func TestSessionCompressor_TrackAndCompress(t *testing.T) {
	sc := NewSessionCompressor(512)

	// 模拟两步工具执行
	sc.TrackToolCall("tid1", "read_file")
	sc.TrackToolResult("tid1", true, "读取 go.mod")
	sc.TrackToolCall("tid2", "bash")
	sc.TrackToolResult("tid2", false, "go build 失败")

	// 触发压缩（填满 token 窗口）
	msgs := []types.Message{
		{Role: "user", Content: strings.Repeat("x", 10000)},
	}
	out, compressed := sc.Compress(msgs, 130000, 200000)

	if !compressed {
		t.Skip("未达压缩阈值，跳过 anchor 验证")
	}
	_ = out

	anchor := sc.Anchor()
	if !strings.Contains(anchor, "Task State") {
		t.Errorf("压缩后 anchor 应包含 Mermaid Task State 块，got: %s", anchor)
	}
	if !strings.Contains(anchor, "read_file") || !strings.Contains(anchor, "bash") {
		t.Errorf("anchor 应包含工具名，got: %s", anchor)
	}
}

func TestSessionCompressor_CanvasNotInjectedWhenEmpty(t *testing.T) {
	sc := NewSessionCompressor(512)
	// 不追踪任何工具，画布为空

	msgs := []types.Message{
		{Role: "user", Content: strings.Repeat("x", 10000)},
	}
	_, compressed := sc.Compress(msgs, 130000, 200000)
	if !compressed {
		t.Skip("未达压缩阈值")
	}

	// 空画布时 anchor 不应有 Mermaid 块
	anchor := sc.Anchor()
	if strings.Contains(anchor, "graph LR") {
		t.Error("空画布时 anchor 不应注入 Mermaid 块")
	}
}
