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

// ADR-0105 决策十一（WP10）：溢出修剪先只动 L0..L2 缓存前缀之后的回合内容。
func layeredOverflowMsgs(l4Bytes int) []types.Message {
	return []types.Message{
		{Role: "system", Content: "L0 contracts", CacheBreakpoint: true},
		{Role: "system", Content: "L0 stable", CacheBreakpoint: true},
		{Role: "user", Content: "<<DATA>>" + strings.Repeat("h", 8_000) + "<</DATA>>"}, // L2
		{Role: "assistant", Content: "<<DATA>>" + strings.Repeat("a", 8_000) + "<</DATA>>", CacheBreakpoint: true},
		{Role: "system", Content: "# ACTIVE PHASE: PLAN"},
		{Role: "user", Content: "<<DATA>>" + strings.Repeat("r", l4Bytes) + "<</DATA>>"}, // L4
	}
}

func TestPruneForOverflow_PrefersTurnContentOverCachedPrefix(t *testing.T) {
	in := layeredOverflowMsgs(40_000)
	out, ok := pruneForOverflow(in)
	if !ok {
		t.Fatal("expected progress")
	}
	for i := 0; i < 5; i++ {
		if out[i].Content != in[i].Content || out[i].CacheBreakpoint != in[i].CacheBreakpoint {
			t.Fatalf("L0..L2 与 L3 选择器不得被改写：msgs[%d]", i)
		}
	}
	if len(out[5].Content) >= len(in[5].Content)/2 {
		t.Fatalf("回合内容应承担全部削减：%d -> %d", len(in[5].Content), len(out[5].Content))
	}
	if !strings.HasPrefix(out[5].Content, "<<DATA>>") || !strings.HasSuffix(out[5].Content, "<</DATA>>") {
		t.Fatal("修剪后围栏标记必须保留")
	}
}

// 回合内容太小、无法独自达成削减目标时，才退回连 L2 一起截（此时别无他法让请求放进窗口）。
func TestPruneForOverflow_FallsBackToHistoryWhenTurnContentTooSmall(t *testing.T) {
	in := layeredOverflowMsgs(300)
	out, ok := pruneForOverflow(in)
	if !ok {
		t.Fatal("expected progress via fallback")
	}
	if len(out[2].Content) >= len(in[2].Content) && len(out[3].Content) >= len(in[3].Content) {
		t.Fatal("回合内容过小时应退回截断 L2 历史")
	}
	if out[0].Content != in[0].Content || out[1].Content != in[1].Content {
		t.Fatal("开头连续 system 始终固定")
	}
}

// 同一 L2、不同大小的回合内容（不同阶段）→ 修剪后 L0..L2 字节相同。
func TestPruneForOverflow_PrefixIndependentOfTurnContentSize(t *testing.T) {
	a, okA := pruneForOverflow(layeredOverflowMsgs(40_000))
	b, okB := pruneForOverflow(layeredOverflowMsgs(60_000))
	if !okA || !okB {
		t.Fatal("expected progress")
	}
	for i := 0; i < 5; i++ {
		if a[i].Content != b[i].Content {
			t.Fatalf("前缀 msgs[%d] 随回合内容大小而变", i)
		}
	}
}
