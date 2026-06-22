package synthetic

import (
	"context"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestSyntheticSkillGen(t *testing.T) {
	ctx := context.Background()

	t.Run("NilProvider", func(t *testing.T) {
		gen := NewSyntheticSkillGen(nil)
		_, err := gen.Generate(ctx, "test", "test desc")
		if err == nil {
			t.Errorf("Expected error for nil provider")
		}
	})

	t.Run("ValidJSON", func(t *testing.T) {
		prov := &MockProvider{
			InferFunc: func(ctx context.Context, messages []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
				return &types.ProviderResponse{
					Content: "```json\n" + `{"name": "test_skill", "description": "desc", "version": "1.0.0", "input_schema": {"type": "object"}}` + "\n```",
				}, nil
			},
		}

		gen := NewSyntheticSkillGen(prov)
		tool, err := gen.Generate(ctx, "test_skill", "desc")
		if err != nil {
			t.Errorf("Generate failed: %v", err)
		}
		if tool.Name != "test_skill" {
			t.Errorf("Expected name test_skill, got %s", tool.Name)
		}
		if tool.Description != "desc" {
			t.Errorf("Expected desc, got %s", tool.Description)
		}
		if tool.Version != "1.0.0" {
			t.Errorf("Expected 1.0.0, got %s", tool.Version)
		}
		if tool.Source != types.ToolLLMGenerated {
			t.Errorf("Expected source LLMGenerated, got %s", tool.Source)
		}
	})

	t.Run("InvalidJSON", func(t *testing.T) {
		prov := &MockProvider{
			InferFunc: func(ctx context.Context, messages []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
				return &types.ProviderResponse{
					Content: `invalid json`,
				}, nil
			},
		}

		gen := NewSyntheticSkillGen(prov)
		_, err := gen.Generate(ctx, "test", "test desc")
		if err == nil || !strings.Contains(err.Error(), "failed to parse") {
			t.Errorf("Expected parse error, got: %v", err)
		}
	})
}

// MockProvider for testing
type MockProvider struct {
	InferFunc func(ctx context.Context, messages []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error)
}

func (m *MockProvider) Name() string                   { return "mock" }
func (m *MockProvider) Ping(ctx context.Context) error { return nil }
func (m *MockProvider) Capabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{}
}
func (m *MockProvider) MaxContextLength() int                    { return 4096 }
func (m *MockProvider) CountTokens(text string) int              { return len(text) }
func (m *MockProvider) CountMessageTokens(msg types.Message) int { return len(msg.Content) }
func (m *MockProvider) Tokenizer() protocol.TokenizerAdapter     { return nil }
func (m *MockProvider) ModelID() string                          { return "mock" }
func (m *MockProvider) SetOptions(opts ...types.InferOption)     {}
func (m *MockProvider) Infer(ctx context.Context, messages []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
	if m.InferFunc != nil {
		return m.InferFunc(ctx, messages, opts...)
	}
	return &types.ProviderResponse{}, nil
}
func (m *MockProvider) StreamInfer(ctx context.Context, messages []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	return nil, nil
}
