package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

func totalContent(msgs []types.Message) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Content)
	}
	return n
}

// windowProvider 模拟上下文窗口：请求总字节超过 limit 时以 ErrContextOverflow 拒绝
// （与 InferenceRouter 的包装方式一致），否则返回一段文本。
type windowProvider struct {
	stubSummaryProvider
	limit int
	err   error // 非 nil 时一律返回该错误
	calls int
	sizes []int
}

func (p *windowProvider) StreamInfer(_ context.Context, msgs []types.Message, _ ...types.InferOption) (<-chan types.StreamEvent, error) {
	p.calls++
	p.sizes = append(p.sizes, totalContent(msgs))
	if p.err != nil {
		return nil, p.err
	}
	if totalContent(msgs) > p.limit {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "router: request exceeds model limits", protocol.ErrContextOverflow)
	}
	ch := make(chan types.StreamEvent, 1)
	ch <- types.StreamEvent{Type: types.StreamTextDelta, Content: "ok"}
	close(ch)
	return ch, nil
}

func overflowMsgs() []types.Message {
	return []types.Message{
		{Role: "system", Content: "IMMUTABLE CORE " + strings.Repeat("s", 2_000)},
		{Role: "user", Content: "<<DATA>>" + strings.Repeat("h", 30_000) + "<</DATA>>"},
		{Role: "user", Content: "<<DATA>>" + strings.Repeat("o", 10_000) + "<</DATA>>"},
		{Role: "user", Content: "what failed?"},
		{Role: "system", Content: "reply reminder"},
	}
}

func TestPruneForOverflow_ShrinksLargestKeepsPinnedAndSmall(t *testing.T) {
	in := overflowMsgs()
	out, ok := pruneForOverflow(in)
	if !ok {
		t.Fatal("expected progress")
	}
	if out[0].Content != in[0].Content || out[3].Content != in[3].Content || out[4].Content != in[4].Content {
		t.Fatal("pinned head, short intent and system messages must be untouched")
	}
	var before, after int
	for i := 1; i <= 3; i++ {
		before += len(in[i].Content)
		after += len(out[i].Content)
	}
	if after > before*6/10 {
		t.Fatalf("prunable bytes should drop to about half: %d -> %d", before, after)
	}
	// 首尾保留：污点围栏的起止标记都必须还在，否则数据会逃出 spotlight 围栏。
	for i := 1; i <= 2; i++ {
		if !strings.HasPrefix(out[i].Content, "<<DATA>>") || !strings.HasSuffix(out[i].Content, "<</DATA>>") {
			t.Fatalf("message %d lost fence markers", i)
		}
	}
	if len(in[1].Content) != 30_017 {
		t.Fatal("input slice must not be mutated")
	}
}

func TestPruneForOverflow_NothingPrunable(t *testing.T) {
	msgs := []types.Message{{Role: "system", Content: strings.Repeat("s", 50_000)}, {Role: "user", Content: "hi"}}
	if _, ok := pruneForOverflow(msgs); ok {
		t.Fatal("pinned-only overflow cannot be pruned")
	}
}

func TestStreamInferWithOverflowRecovery_PrunesOnceAndRetries(t *testing.T) {
	a := NewAgentWithDefaults("sess-overflow")
	p := &windowProvider{limit: 30_000}
	a.provider = p
	before := metrics.GlobalContextOverflowRecoveryTotal.Load()

	msgs := overflowMsgs()
	resp, sent, err := a.streamInferWithOverflowRecovery(context.Background(), msgs, msgs, nil, protocol.AudienceInternal)
	if err != nil || resp == nil || resp.Content != "ok" {
		t.Fatalf("expected recovered response, got resp=%v err=%v", resp, err)
	}
	if p.calls != 2 || p.sizes[1] >= p.sizes[0] {
		t.Fatalf("expected exactly one retry with a smaller request, sizes=%v", p.sizes)
	}
	if totalContent(sent) != p.sizes[1] {
		t.Fatal("returned messages must be the ones actually sent")
	}
	if metrics.GlobalContextOverflowRecoveryTotal.Load() != before+1 {
		t.Fatal("recovery must be counted")
	}
}

// 修剪后仍超限时只重试一次，并如实上抛 ErrContextOverflow（不得循环）。
func TestStreamInferWithOverflowRecovery_BoundedToOneRetry(t *testing.T) {
	a := NewAgentWithDefaults("sess-overflow-bounded")
	p := &windowProvider{limit: 1}
	a.provider = p
	msgs := overflowMsgs()
	_, _, err := a.streamInferWithOverflowRecovery(context.Background(), msgs, msgs, nil, protocol.AudienceInternal)
	if !errors.Is(err, protocol.ErrContextOverflow) || p.calls != 2 {
		t.Fatalf("want overflow after exactly 2 calls, got calls=%d err=%v", p.calls, err)
	}
}

func TestStreamInferWithOverflowRecovery_OtherErrorsNotRetried(t *testing.T) {
	a := NewAgentWithDefaults("sess-overflow-other")
	p := &windowProvider{err: apperr.Wrap(apperr.CodeInternal, "router", protocol.ErrAllProvidersFailed)}
	a.provider = p
	msgs := overflowMsgs()
	_, _, err := a.streamInferWithOverflowRecovery(context.Background(), msgs, msgs, nil, protocol.AudienceInternal)
	if !errors.Is(err, protocol.ErrAllProvidersFailed) || p.calls != 1 {
		t.Fatalf("non-overflow errors must pass through without retry, calls=%d err=%v", p.calls, err)
	}
}
