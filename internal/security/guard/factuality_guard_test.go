package guard

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

type mockLLMProvider struct {
	response string
}

func (m *mockLLMProvider) Infer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
	return &types.ProviderResponse{Content: m.response}, nil
}

func (m *mockLLMProvider) StreamInfer(ctx context.Context, messages []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	return nil, nil
}

func (m *mockLLMProvider) Capabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{}
}

func (m *mockLLMProvider) Tokenizer() protocol.TokenizerAdapter {
	return nil
}

func (m *mockLLMProvider) ModelID() string {
	return "mock-model"
}

func TestFactualityGuardVerify(t *testing.T) {
	fg := NewFactualityGuard()
	fg.sampleRate = 1.0 // force sampling

	ctx := context.Background()

	// 1. Citation Fail
	res, err := fg.Verify(ctx, "according to the research indicates specificword", "context doc", types.TaintMedium)
	if err != nil || res.Verdict != FactualityFail || res.Layer != "citation" {
		t.Errorf("expected citation fail, got %v %v", res, err)
	}

	// 2. Numerical Fail
	res, err = fg.Verify(ctx, "it has a 120% probability", "context doc", types.TaintMedium)
	if err != nil || res.Verdict != FactualityFail || res.Layer != "numerical" {
		t.Errorf("expected numerical fail, got %v %v", res, err)
	}

	// 3. Semantic Fail
	fg.InjectLLMProvider(&mockLLMProvider{response: "FAIL: contradicted"})
	res, err = fg.Verify(ctx, "claim", "context doc", types.TaintHigh)
	if err != nil || res.Verdict != FactualityFail || res.Layer != "semantic" {
		t.Errorf("expected semantic fail, got %v %v", res, err)
	}

	// 4. Semantic Uncertain
	fg.InjectLLMProvider(&mockLLMProvider{response: "UNCERTAIN"})
	res, err = fg.Verify(ctx, "claim", "context doc", types.TaintHigh)
	if err != nil || res.Verdict != FactualityUncertain || res.Layer != "semantic" {
		t.Errorf("expected semantic uncertain, got %v %v", res, err)
	}

	// 5. Semantic Pass
	fg.InjectLLMProvider(&mockLLMProvider{response: "PASS"})
	res, err = fg.Verify(ctx, "claim", "context doc", types.TaintHigh)
	if err != nil || res.Verdict != FactualityPass {
		t.Errorf("expected semantic pass, got %v %v", res, err)
	}
}
