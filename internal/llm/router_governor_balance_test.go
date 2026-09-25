package llm

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
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

// pressureGovernor 记录请求优先级；priority≥1 恒被水位线拒绝，WaitForLLMCapacity 立即返回
// （与真实实现一致：它只等并发额度，不等内存恢复）。
type pressureGovernor struct {
	mu         sync.Mutex
	priorities []int
}

func (g *pressureGovernor) AdmitLLM(priority int) (bool, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.priorities = append(g.priorities, priority)
	return priority == 0, 3
}

func (g *pressureGovernor) WaitForLLMCapacity(context.Context) error { return nil }
func (g *pressureGovernor) ReleaseLLM()                              {}

func (g *pressureGovernor) snapshot() []int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]int(nil), g.priorities...)
}

// TestAcquireLLMCapacity_BackgroundUsesDegradablePriority 后台工作以 priority=1 申请，
// 被水位线拒绝时挂起到 ctx 结束，不得忙等（WaitForLLMCapacity 立即返回会让循环空转）。
func TestAcquireLLMCapacity_BackgroundUsesDegradablePriority(t *testing.T) {
	gov := &pressureGovernor{}
	router := NewInferenceRouter(NewProviderRegistry(config.M1RouterThresholds{}), nil, WithGovernor(gov))

	ctx, cancel := context.WithTimeout(protocol.WithBackgroundWork(context.Background()), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- router.acquireLLMCapacity(ctx) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("持续水位线压力下后台推理应在 ctx 到期后放弃")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 到期后仍未返回：被水位线拒绝后在忙等（WaitForLLMCapacity 立即返回）")
	}
	bg := gov.snapshot()
	if len(bg) == 0 || bg[0] != 1 {
		t.Fatalf("后台工作应以 priority=1 申请，got %v", bg)
	}
	if len(bg) > 2 {
		t.Fatalf("被水位线拒绝后应挂起而非忙等，100ms 内申请了 %d 次", len(bg))
	}

	if err := router.acquireLLMCapacity(context.Background()); err != nil {
		t.Fatalf("用户可见推理不受水位线约束: %v", err)
	}
	if all := gov.snapshot(); all[len(all)-1] != 0 {
		last := all[len(all)-1]
		t.Fatalf("未标记后台的请求应以 priority=0 申请，got %d", last)
	}
}

// TestAcquireLLMCapacity_DeferrableReturnsImmediately 可推迟的后台工作（outbox）被水位线
// 拒绝时不挂起，立即以 ErrBackgroundDeferred 返回——串行队列里挂起一条会阻塞其后记录。
func TestAcquireLLMCapacity_DeferrableReturnsImmediately(t *testing.T) {
	gov := &pressureGovernor{}
	router := NewInferenceRouter(NewProviderRegistry(config.M1RouterThresholds{}), nil, WithGovernor(gov))

	ctx, cancel := context.WithCancel(protocol.WithDeferrableBackgroundWork(context.Background()))
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- router.acquireLLMCapacity(ctx) }()
	select {
	case err := <-done:
		if !errors.Is(err, protocol.ErrBackgroundDeferred) {
			t.Fatalf("应返回 ErrBackgroundDeferred，got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("可推迟工作被水位线拒绝后在挂起，未立即返回")
	}
	if p := gov.snapshot(); len(p) == 0 || p[0] != 1 {
		t.Fatalf("可推迟工作仍按后台优先级申请，got %v", p)
	}
}
