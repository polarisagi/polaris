package llm

import (
	"context"
	"errors"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// overflowProvider 每次调用都以上下文超限拒绝（DeepSeek/OpenAI 兼容 400 格式）。
type overflowProvider struct {
	mockProvider
	calls int
}

var errOverflow400 = apperr.New(apperr.CodeInternal,
	`api error (status 400): {"error":{"message":"This model's maximum context length is 65536 tokens. However, you requested 70000 tokens","type":"invalid_request_error","code":"context_length_exceeded"}}`)

func (p *overflowProvider) Infer(context.Context, []types.Message, ...types.InferOption) (*types.ProviderResponse, error) {
	p.calls++
	return nil, errOverflow400
}

func (p *overflowProvider) StreamInfer(context.Context, []types.Message, ...types.InferOption) (<-chan types.StreamEvent, error) {
	p.calls++
	return nil, errOverflow400
}

func newOverflowRouter(t *testing.T) (*InferenceRouter, *overflowProvider, *mockProvider, *ProviderRegistry) {
	t.Helper()
	reg := NewProviderRegistry(config.M1RouterThresholds{})
	over := &overflowProvider{mockProvider: mockProvider{caps: types.ProviderCapabilities{CostPer1KInput: 0.1}}}
	backup := &mockProvider{caps: types.ProviderCapabilities{CostPer1KInput: 9.0}}
	reg.Register("over", "Overflow", over)
	reg.Register("backup", "Backup", backup)
	return NewInferenceRouter(reg, nil), over, backup, reg
}

// 超长请求是请求侧故障：路由不得把它换 Provider 重发（每一家都会拒绝且计费），
// 须以 ErrContextOverflow 立即交还调用方，由持有消息语义的一方缩减后重试。
func TestRouter_ContextOverflow_ReturnsSentinelWithoutFailover(t *testing.T) {
	for _, stream := range []bool{false, true} {
		router, over, backup, _ := newOverflowRouter(t)
		msgs := []types.Message{{Role: "user", Content: "hi"}}
		var err error
		if stream {
			_, err = router.StreamInfer(context.Background(), msgs)
		} else {
			_, err = router.Infer(context.Background(), msgs)
		}
		if !errors.Is(err, protocol.ErrContextOverflow) {
			t.Fatalf("stream=%v: want ErrContextOverflow, got %v", stream, err)
		}
		if errors.Is(err, protocol.ErrAllProvidersFailed) {
			t.Fatalf("stream=%v: overflow must not be reported as provider exhaustion", stream)
		}
		if over.calls != 1 || backup.callCount != 0 {
			t.Fatalf("stream=%v: want 1 attempt and no failover, got over=%d backup=%d", stream, over.calls, backup.callCount)
		}
	}
}

// 超长请求不说明 Provider 不健康：反复超限也不得打开该 Provider 的熔断器。
func TestRouter_ContextOverflow_DoesNotTripBreaker(t *testing.T) {
	router, _, _, reg := newOverflowRouter(t)
	msgs := []types.Message{{Role: "user", Content: "hi"}}
	for range 20 {
		_, _ = router.Infer(context.Background(), msgs)
	}
	e := reg.entries["over"]
	e.mu.Lock()
	rate := e.successRate
	e.mu.Unlock()
	if rate < 0.999 {
		t.Fatalf("success rate must stay untouched by request faults, got %v", rate)
	}
	if !e.cb.Allow() {
		t.Fatal("circuit breaker must stay closed after request faults")
	}
}

// 本地小窗口模型超限时应自动升到窗口更大的 Provider；窗口不大于失败方的 Provider
// 装不下同一请求，不得尝试。
func TestRouter_ContextOverflow_FailsOverOnlyToLargerWindow(t *testing.T) {
	for _, stream := range []bool{false, true} {
		reg := NewProviderRegistry(config.M1RouterThresholds{})
		small := &overflowProvider{mockProvider: mockProvider{caps: types.ProviderCapabilities{CostPer1KInput: 0.1, MaxContextTokens: 8192}}}
		same := &mockProvider{caps: types.ProviderCapabilities{CostPer1KInput: 0.2, MaxContextTokens: 8192}}
		large := &mockProvider{caps: types.ProviderCapabilities{CostPer1KInput: 9.0, MaxContextTokens: 128000}}
		reg.Register("small", "Small", small)
		reg.Register("same", "Same", same)
		reg.Register("large", "Large", large)
		router := NewInferenceRouter(reg, nil)
		msgs := []types.Message{{Role: "user", Content: "hi"}}

		var err error
		if stream {
			var ch <-chan types.StreamEvent
			ch, err = router.StreamInfer(context.Background(), msgs)
			for range ch {
			}
		} else {
			_, err = router.Infer(context.Background(), msgs)
		}
		if err != nil {
			t.Fatalf("stream=%v: larger-window provider should serve, got %v", stream, err)
		}
		if small.calls != 1 {
			t.Fatalf("stream=%v: cheapest small-window provider should be tried first, got %d calls", stream, small.calls)
		}
		if same.callCount != 0 {
			t.Fatalf("stream=%v: same-window provider must be skipped, got %d calls", stream, same.callCount)
		}
		if !stream && large.callCount != 1 {
			t.Fatalf("large provider should be called once, got %d", large.callCount)
		}
	}
}
