package llm

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// 本文件覆盖 2026-07-12 P1 修复引入的两条新行为：
//  1. router.go/router_failover.go 接入 ErrorClassifier 后，不可恢复错误（Retryable=false
//     且 ShouldFallback=false）应直接短路返回，不再对每个 provider 重复失败。
//  2. router_stream.go 接入 StreamBudgetGuard 后，单流 token 预算耗尽应硬阻断，
//     不再无限转发下去。

// nonRetryableProvider 始终返回一个 Classify() 会判定为 ReasonFormatError
// （Retryable=false, ShouldFallback=false）的错误，模拟请求本身格式错误的场景——
// 换任何 provider 都无法恢复。
type nonRetryableProvider struct {
	callCount int
	caps      types.ProviderCapabilities
}

func (m *nonRetryableProvider) Infer(_ context.Context, _ []types.Message, _ ...types.InferOption) (*types.ProviderResponse, error) {
	m.callCount++
	return nil, apperr.New(apperr.CodeInternal, "adapter: HTTP 400: request body is not valid json")
}

func (m *nonRetryableProvider) StreamInfer(_ context.Context, _ []types.Message, _ ...types.InferOption) (<-chan types.StreamEvent, error) {
	m.callCount++
	return nil, apperr.New(apperr.CodeInternal, "adapter: HTTP 400: request body is not valid json")
}

func (m *nonRetryableProvider) Capabilities() types.ProviderCapabilities { return m.caps }
func (m *nonRetryableProvider) Tokenizer() protocol.TokenizerAdapter     { return &SimpleTokenizer{} }
func (m *nonRetryableProvider) ModelID() string                          { return "non-retryable-mock" }

func TestInferenceRouter_NonRetryableErrorSkipsFailover(t *testing.T) {
	reg := NewProviderRegistry(config.M1RouterThresholds{})
	// 显式拉开 CostPer1KInput 差距，保证 best() 在 map 遍历顺序随机的情况下
	// 仍确定性地优先选中 primary（否则 healthScore 打平时命中哪个纯看 map 迭代顺序，
	// 会导致本测试间歇性失败——两者曾经都用零值 caps 导致 flaky）。
	primary := &nonRetryableProvider{caps: types.ProviderCapabilities{CostPer1KInput: 0.1}}
	secondary := &mockProvider{caps: types.ProviderCapabilities{CostPer1KInput: 9.0}}
	reg.Register("primary", "Primary", primary)
	reg.Register("secondary", "Secondary", secondary)

	router := NewInferenceRouter(reg, nil)
	_, err := router.Infer(context.Background(), []types.Message{{Role: "user", Content: "hello"}})
	if err == nil {
		t.Fatal("expected non-retryable error to be returned, got nil")
	}
	if primary.callCount != 1 {
		t.Fatalf("expected primary to be called exactly once, got %d", primary.callCount)
	}
	if secondary.callCount != 0 {
		t.Fatalf("expected secondary to never be tried on a non-retryable error, got %d calls", secondary.callCount)
	}
}

func TestInferenceRouter_NonRetryableStreamErrorSkipsFailover(t *testing.T) {
	reg := NewProviderRegistry(config.M1RouterThresholds{})
	primary := &nonRetryableProvider{caps: types.ProviderCapabilities{CostPer1KInput: 0.1}}
	secondary := &mockProvider{caps: types.ProviderCapabilities{CostPer1KInput: 9.0}}
	reg.Register("primary", "Primary", primary)
	reg.Register("secondary", "Secondary", secondary)

	router := NewInferenceRouter(reg, nil)
	_, err := router.StreamInfer(context.Background(), []types.Message{{Role: "user", Content: "hello"}})
	if err == nil {
		t.Fatal("expected non-retryable stream error to be returned, got nil")
	}
	if secondary.callCount != 0 {
		t.Fatalf("expected secondary to never be tried on a non-retryable stream error, got %d calls", secondary.callCount)
	}
}

// burnyProvider 流式返回可配置数量/大小的 StreamTextDelta 事件，用于驱动
// StreamBudgetGuard 的预算耗尽路径。
type burnyProvider struct {
	chunks []string
	caps   types.ProviderCapabilities
}

func (m *burnyProvider) Infer(_ context.Context, _ []types.Message, _ ...types.InferOption) (*types.ProviderResponse, error) {
	return &types.ProviderResponse{Content: "ok"}, nil
}

func (m *burnyProvider) StreamInfer(_ context.Context, _ []types.Message, _ ...types.InferOption) (<-chan types.StreamEvent, error) {
	ch := make(chan types.StreamEvent, len(m.chunks)+1)
	for _, c := range m.chunks {
		ch <- types.StreamEvent{Type: types.StreamTextDelta, Content: c}
	}
	close(ch)
	return ch, nil
}

func (m *burnyProvider) Capabilities() types.ProviderCapabilities { return m.caps }
func (m *burnyProvider) Tokenizer() protocol.TokenizerAdapter     { return &SimpleTokenizer{} }
func (m *burnyProvider) ModelID() string                          { return "burny-mock" }

func TestWrapStreamChannel_BudgetExhaustionAborts(t *testing.T) {
	reg := NewProviderRegistry(config.M1RouterThresholds{})
	// 单个 chunk 携带远超预算的文本，MaxTokens=1 保证第一块就把预算打穿。
	p := &burnyProvider{chunks: []string{
		"this is a very long chunk of streamed text that will estimate to many tokens at once",
	}}
	reg.Register("only", "Only", p)
	router := NewInferenceRouter(reg, nil)

	ch, err := router.StreamInfer(context.Background(), []types.Message{{Role: "user", Content: "hi"}}, types.WithMaxTokens(1))
	if err != nil {
		t.Fatalf("unexpected error establishing stream: %v", err)
	}

	sawCancelled := false
	sawTextDelta := false
	for ev := range ch {
		switch ev.Type {
		case types.StreamCancelled:
			sawCancelled = true
		case types.StreamTextDelta:
			sawTextDelta = true
		}
	}
	if !sawCancelled {
		t.Fatal("expected StreamCancelled event when token budget is exhausted on the first chunk")
	}
	if sawTextDelta {
		t.Fatal("expected the over-budget chunk itself to be dropped, not relayed to the caller")
	}
}
