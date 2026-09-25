package llm

import (
	"context"
	"sync"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/pkg/types"
)

// countingGovernor 记录在途额度，用于断言 Acquire/Release 严格配对。
type countingGovernor struct {
	mu       sync.Mutex
	inFlight int
}

func (g *countingGovernor) AdmitLLM(int) (bool, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.inFlight++
	return true, 0
}

func (g *countingGovernor) WaitForLLMCapacity(context.Context) error { return nil }

func (g *countingGovernor) ReleaseLLM() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.inFlight--
}

func (g *countingGovernor) current() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inFlight
}

// failingStreamProvider StreamInfer 恒失败。
type failingStreamProvider struct{ mockProvider }

func (f *failingStreamProvider) StreamInfer(context.Context, []types.Message, ...types.InferOption) (<-chan types.StreamEvent, error) {
	return nil, errProviderUnavailable
}

func drainStream(ch <-chan types.StreamEvent) {
	for range ch { //nolint:revive // 仅消费至关闭，触发 wrapStreamChannel 的 ReleaseLLM
	}
}

// TestStreamInfer_EmptyPoolFallback_BalancesGovernor 目标 Pool 为空直接走
// streamPoolFallback 的分支，此前先于 acquire 返回，流关闭时"没借就还"，
// llmInFlight 变负、并发上限失效。
func TestStreamInfer_EmptyPoolFallback_BalancesGovernor(t *testing.T) {
	reg := NewProviderRegistry(config.M1RouterThresholds{})
	reg.RegisterWithRole("default-p1", "DefaultP1", "default",
		&mockProvider{caps: types.ProviderCapabilities{CostPer1KInput: 1.0}})
	gov := &countingGovernor{}
	router := NewInferenceRouter(reg, nil, WithGovernor(gov))

	for range 3 {
		ch, err := router.StreamInfer(context.Background(),
			[]types.Message{{Role: "user", Content: "hi"}}, types.WithModelPool("general"))
		if err != nil {
			t.Fatalf("pool fallback must succeed, got err: %v", err)
		}
		drainStream(ch)
	}
	if got := gov.current(); got != 0 {
		t.Fatalf("llmInFlight must return to 0 after streams close, got %d", got)
	}
}

// TestStreamInferWithTarget_ErrorReleasesGovernor 目标 Provider 失败时额度须归还。
func TestStreamInferWithTarget_ErrorReleasesGovernor(t *testing.T) {
	reg := NewProviderRegistry(config.M1RouterThresholds{})
	gov := &countingGovernor{}
	router := NewInferenceRouter(reg, nil, WithGovernor(gov))

	if _, err := router.StreamInferWithTarget(context.Background(), &failingStreamProvider{}, "target",
		[]types.Message{{Role: "user", Content: "hi"}}); err == nil {
		t.Fatal("expected error from failing provider")
	}
	if got := gov.current(); got != 0 {
		t.Fatalf("llmInFlight must return to 0 on error path, got %d", got)
	}
}
