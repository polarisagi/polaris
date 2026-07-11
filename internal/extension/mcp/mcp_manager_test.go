package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

type dummySamplingProvider struct {
	content string
}

func (d *dummySamplingProvider) Infer(ctx context.Context, messages []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
	return &types.ProviderResponse{
		Content: d.content,
		Model:   "dummy",
	}, nil
}
func (d *dummySamplingProvider) StreamInfer(ctx context.Context, messages []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	return nil, nil
}
func (d *dummySamplingProvider) Capabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{}
}
func (d *dummySamplingProvider) Tokenizer() protocol.TokenizerAdapter { return nil }
func (d *dummySamplingProvider) ModelID() string                      { return "dummy" }
func (d *dummySamplingProvider) Close() error                         { return nil }

func TestMakeSamplingHandler(t *testing.T) {
	mgr := NewMCPManager(nil, nil, &mockPolicyGate{})
	mgr.SetSamplingProvider(&dummySamplingProvider{content: "dummy resp"})

	handler := mgr.makeSamplingHandler()

	// Test sampling/createMessage
	reqJSON := `{"messages":[{"role":"user","content":"hello"}],"maxTokens":100}`
	ctx := context.Background()
	res, err := handler(ctx, "sampling/createMessage", 1, []byte(reqJSON))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var resMap map[string]any
	if err := json.Unmarshal(res, &resMap); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if resMap["role"] != "assistant" || resMap["model"] != "dummy" {
		t.Errorf("unexpected response: %v", resMap)
	}

	// Test roots/list
	res, err = handler(ctx, "roots/list", 2, []byte(`{}`))
	if err != nil {
		t.Fatalf("unexpected error for roots/list: %v", err)
	}
	if string(res) != `{"roots":[]}` {
		t.Errorf("expected roots:[], got %s", string(res))
	}

	// Test unknown
	_, err = handler(ctx, "unknown/method", 3, []byte(`{}`))
	if err == nil {
		t.Errorf("expected error for unknown method")
	}
}

type mockPolicyGate struct{}

func (m *mockPolicyGate) IsAuthorized(ctx context.Context, principal, action, resource string, context map[string]any) (bool, error) {
	return true, nil
}
func (m *mockPolicyGate) Review(ctx context.Context, req types.PolicyReviewRequest) (types.PolicyReviewResult, error) {
	return types.PolicyReviewResult{Allowed: true}, nil
}
