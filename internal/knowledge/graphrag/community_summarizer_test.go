package graphrag

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

type mockProvider struct {
	content string
}

func (m *mockProvider) Infer(ctx context.Context, messages []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
	return &types.ProviderResponse{Content: m.content}, nil
}

func (m *mockProvider) Name() string {
	return "mock"
}

func (m *mockProvider) Capabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{}
}

func (m *mockProvider) ModelID() string {
	return "mock-model"
}

func (m *mockProvider) StreamInfer(ctx context.Context, messages []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	return nil, nil
}

func (m *mockProvider) Tokenizer() protocol.TokenizerAdapter {
	return nil
}

func TestCommunityGenerativeSummarizer_Summarize(t *testing.T) {
	mockJSON := `{"summary":"这是一个关于聚类的测试社区。","keywords":["聚类", "测试"]}`
	provider := &mockProvider{content: mockJSON}
	summarizer := NewCommunityGenerativeSummarizer(provider)

	communities := map[int][]string{
		1: {"NodeA", "NodeB"},
	}

	summaries, err := summarizer.Summarize(context.Background(), communities)
	if err != nil {
		t.Fatalf("Summarize failed: %v", err)
	}

	if len(summaries) != 1 {
		t.Fatalf("Expected 1 summary, got %d", len(summaries))
	}

	summary := summaries[0]
	if summary.CommunityID != 1 {
		t.Errorf("Expected CommunityID 1, got %d", summary.CommunityID)
	}
	if summary.Summary == "" {
		t.Error("Expected Summary to not be empty")
	}
	if len(summary.Keywords) == 0 {
		t.Error("Expected Keywords to not be empty")
	}
	if summary.Summary != "这是一个关于聚类的测试社区。" {
		t.Errorf("Unexpected summary content: %s", summary.Summary)
	}
}

func TestCommunityGenerativeSummarizer_Fallback(t *testing.T) {
	mockText := `Here is a summary that is not JSON`
	provider := &mockProvider{content: mockText}
	summarizer := NewCommunityGenerativeSummarizer(provider)

	communities := map[int][]string{
		2: {"NodeC", "NodeD"},
	}

	summaries, err := summarizer.Summarize(context.Background(), communities)
	if err != nil {
		t.Fatalf("Summarize failed: %v", err)
	}

	if len(summaries) != 1 {
		t.Fatalf("Expected 1 summary, got %d", len(summaries))
	}

	summary := summaries[0]
	if summary.Summary != "Here is a summary that is not JSON" {
		t.Errorf("Expected fallback summary, got: %s", summary.Summary)
	}
}
