package llm

import (
	"context"
	"errors"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

type mockRouterProvider struct {
	id              string
	caps            types.ProviderCapabilities
	inferFunc       func() (*types.ProviderResponse, error)
	streamInferFunc func() (<-chan types.StreamEvent, error)
}

func (m *mockRouterProvider) ModelID() string                          { return m.id }
func (m *mockRouterProvider) Capabilities() types.ProviderCapabilities { return m.caps }
func (m *mockRouterProvider) Tokenizer() protocol.TokenizerAdapter     { return nil }

func (m *mockRouterProvider) Infer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
	if m.inferFunc != nil {
		return m.inferFunc()
	}
	return &types.ProviderResponse{Content: m.id}, nil
}

func (m *mockRouterProvider) StreamInfer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	if m.streamInferFunc != nil {
		return m.streamInferFunc()
	}
	ch := make(chan types.StreamEvent, 1)
	ch <- types.StreamEvent{Type: types.StreamTextDelta, Content: m.id}
	close(ch)
	return ch, nil
}

func TestRouterExtra(t *testing.T) {
	cfg := config.M1RouterThresholds{
		CircuitBreakerFailureCount:    3,
		CircuitBreakerCooldownSeconds: 10,
	}
	reg := NewProviderRegistry(cfg)

	router := NewInferenceRouter(reg, nil)

	p1 := &mockRouterProvider{id: "p1", caps: types.ProviderCapabilities{MaxContextTokens: 1000}}

	reg.RegisterWithRole("p1", "p1", "primary", p1)

	if reg.BestForRole("primary", nil) == nil {
		t.Errorf("expected primary to be found")
	}

	msgs := []types.Message{{Role: "user", Content: "Hi"}}

	resp, err := router.Infer(context.Background(), msgs)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if resp.Content != "p1" {
		t.Errorf("expected p1, got %s", resp.Content)
	}

	stream, err := router.StreamInfer(context.Background(), msgs)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	ev := <-stream
	if ev.Content != "p1" {
		t.Errorf("expected p1, got %s", ev.Content)
	}

	reg.Unregister("p1")
	if reg.PickProviderName("primary") != "" {
		t.Errorf("expected p1 to be unregistered")
	}

	p2 := &mockRouterProvider{id: "p2", caps: types.ProviderCapabilities{MaxContextTokens: 2000}}
	reg.RegisterWithRole("p2", "p2", "fallback", p2)

	reg.UnregisterAll()
	if reg.PickProviderName("fallback") != "" {
		t.Errorf("expected all unregistered")
	}
}

func TestRouterFailover(t *testing.T) {
	cfg := config.M1RouterThresholds{
		CircuitBreakerFailureCount:    1,
		CircuitBreakerCooldownSeconds: 10,
	}
	reg := NewProviderRegistry(cfg)
	router := NewInferenceRouter(reg, nil)

	p1 := &mockRouterProvider{
		id:   "p1",
		caps: types.ProviderCapabilities{MaxContextTokens: 1000},
		inferFunc: func() (*types.ProviderResponse, error) {
			return nil, errors.New("rate limited")
		},
	}
	p2 := &mockRouterProvider{id: "p2", caps: types.ProviderCapabilities{MaxContextTokens: 2000}}

	reg.RegisterWithRole("p1", "p1", "primary", p1)
	reg.RegisterWithRole("p2", "p2", "primary", p2)

	msgs := []types.Message{{Role: "user", Content: "Hi"}}

	resp, err := router.Infer(context.Background(), msgs)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if resp.Content != "p2" {
		t.Errorf("expected failover to p2, got %s", resp.Content)
	}
}
