package compact

import (
	"context"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// cancelledStreamProvider 只产出一个 StreamCancelled 事件即关闭流，模拟
// ctx 超时/预算守卫硬阻断在还没输出任何摘要文本前就打断 Summarize 调用。
type cancelledStreamProvider struct{}

func (p *cancelledStreamProvider) Infer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
	return nil, nil
}

func (p *cancelledStreamProvider) StreamInfer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	ch := make(chan types.StreamEvent, 1)
	ch <- types.StreamEvent{Type: types.StreamCancelled, Content: "context deadline exceeded"}
	close(ch)
	return ch, nil
}

func (p *cancelledStreamProvider) Capabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{}
}
func (p *cancelledStreamProvider) Tokenizer() protocol.TokenizerAdapter { return nil }
func (p *cancelledStreamProvider) ModelID() string                      { return "mock-cancelled" }

var _ protocol.Provider = (*cancelledStreamProvider)(nil)

// 以下测试 2026-07-22 从 internal/gateway/server/chat/compressor_test.go 随
// 算法本体一并迁移（M4/M5 共享压缩算法抽取，见 compact.go doc 注释），
// 断言逻辑与迁移前完全一致。

func TestRoughTokens(t *testing.T) {
	msgs := []types.Message{
		{Content: "1234"},     // 4 chars = 1 token
		{Content: "12345678"}, // 8 chars = 2 tokens
	}
	toks := RoughTokens(msgs)
	if toks != 3 {
		t.Errorf("expected 3, got %d", toks)
	}
}

func TestSplitMessages(t *testing.T) {
	msgs := []types.Message{
		{Content: strings.Repeat("a", 4000)}, // msg 0
		{Content: strings.Repeat("b", 4000)}, // msg 1
		{Content: strings.Repeat("c", 4000)}, // msg 2
	}
	// Total 12000 chars. tailTokens=2000 => 8000 chars.
	// So tail needs 8000 chars.
	// msg 2 is 4000. msg 1 + msg 2 = 8000.
	// So msg 0 goes to middle, msg 1 and msg 2 go to tail.
	middle, tail := SplitMessages(msgs, 2000)
	if len(middle) != 1 || len(tail) != 2 {
		t.Errorf("expected 1 middle, 2 tail, got %d, %d", len(middle), len(tail))
	}
	if middle[0].Content[0] != 'a' {
		t.Errorf("wrong middle")
	}
}

func TestBuildTranscript(t *testing.T) {
	msgs := []types.Message{
		{Role: "user", Content: "hello"},
	}
	ts := BuildTranscript(msgs)
	if !strings.Contains(ts, "[user]: hello") {
		t.Errorf("wrong transcript: %s", ts)
	}

	large := []types.Message{
		{Role: "user", Content: strings.Repeat("a", 10000)},
	}
	ts2 := BuildTranscript(large)
	if !strings.Contains(ts2, "(truncated)") {
		t.Errorf("expected truncation")
	}
}

func TestCalcSummaryBudget(t *testing.T) {
	msgs := []types.Message{
		{Content: strings.Repeat("a", 40000)}, // 10000 tokens
	}
	// budget: 10000 * 0.20 = 2000
	budget := CalcSummaryBudget(msgs, DefaultSummaryRatio, DefaultMinSummaryTokens, DefaultMaxSummaryTokens)
	if budget != 2000 {
		t.Errorf("expected 2000, got %d", budget)
	}

	small := []types.Message{
		{Content: "short"},
	}
	if CalcSummaryBudget(small, DefaultSummaryRatio, DefaultMinSummaryTokens, DefaultMaxSummaryTokens) != DefaultMinSummaryTokens {
		t.Errorf("expected %d for small", DefaultMinSummaryTokens)
	}
}

// TestSummarize_StreamCancelled 复现 empty_response 同族根因在 compact.Summarize
// 里的版本：流被中断且未产出任何摘要文本时必须返回错误，不能返回 ("", nil)——
// 那会被 orchestrator_interactive.go 的自动压缩逻辑当作"摘要成功但为空"，
// 静默丢弃压缩前的原始上下文。
func TestSummarize_StreamCancelled(t *testing.T) {
	msgs := []types.Message{{Role: "user", Content: "hello"}}
	summary, err := Summarize(context.Background(), msgs, 100, &cancelledStreamProvider{})
	if err == nil {
		t.Fatalf("expected error, got nil (summary=%q) — StreamCancelled 被静默当作成功", summary)
	}
	if !apperr.IsCode(err, apperr.CodeCancelled) {
		t.Errorf("expected error code %s, got err: %v", apperr.CodeCancelled, err)
	}
}

func TestInjectTaskCanvas(t *testing.T) {
	tests := []struct {
		name    string
		mmd     string
		summary string
		want    string
	}{
		{
			name:    "empty mmd",
			mmd:     "",
			summary: "some summary",
			want:    "some summary",
		},
		{
			name:    "non-empty mmd",
			mmd:     "graph LR\n  A-->B",
			summary: "some summary",
			want:    "## Task State (node_id → read_tool_ref)\ngraph LR\n  A-->B\n## Summary\nsome summary",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := InjectTaskCanvas(tt.mmd, tt.summary)
			if got != tt.want {
				t.Errorf("InjectTaskCanvas() = %v, want %v", got, tt.want)
			}
		})
	}
}

type mockToolRefOffloader struct {
	fail bool
	id   string
}

func (m *mockToolRefOffloader) Offload(_ context.Context, _ string, _ []byte) (string, error) {
	if m.fail {
		return "", apperr.New(apperr.CodeInternal, "forced error")
	}
	return m.id, nil
}

func TestOffloadLargeToolResults(t *testing.T) {
	ctx := context.Background()
	taskID := "test-session"

	// Create a tool message exactly at threshold
	thresholdContent := strings.Repeat("a", ToolOffloadThreshold)

	// Create a large tool message
	largeContent := strings.Repeat("b", ToolOffloadThreshold+1)

	msgs := []types.Message{
		{Role: "user", Content: largeContent},     // Large but not a tool, should not be offloaded
		{Role: "tool", Content: thresholdContent}, // Tool but not large enough, should not be offloaded
		{Role: "tool", Content: largeContent},     // Large tool, should be offloaded
	}

	// 1. Test nil offloader (should do nothing)
	out := OffloadLargeToolResults(ctx, taskID, msgs, nil)
	if len(out) != 3 || out[2].Content != largeContent {
		t.Fatalf("expected nil offloader to not change messages")
	}

	// 2. Test offload failure (should keep original)
	failingOffloader := &mockToolRefOffloader{fail: true}
	out = OffloadLargeToolResults(ctx, taskID, msgs, failingOffloader)
	if len(out) != 3 || out[2].Content != largeContent {
		t.Fatalf("expected failing offload to keep original")
	}

	// 3. Test successful offload
	successOffloader := &mockToolRefOffloader{id: "mock-id-123"}
	out = OffloadLargeToolResults(ctx, taskID, msgs, successOffloader)

	if out[0].Content != largeContent {
		t.Errorf("user message should not be offloaded")
	}
	if out[1].Content != thresholdContent {
		t.Errorf("small tool message should not be offloaded")
	}

	expectedStub := "[offloaded: 10241 bytes → read_tool_ref(task_id=\"test-session\", id=\"mock-id-123\")]"
	if out[2].Content != expectedStub {
		t.Errorf("large tool message offload mismatch, got %q, want %q", out[2].Content, expectedStub)
	}
}
