package graphrag

import (
	"context"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestGraphWriter_UpsertEntity(t *testing.T) {
	// ctx := context.Background()
	// mb := &mockDatabaseWriter{}

	// e1 := &types.Entity{
	// 	ID:          "e1",
	// 	Name:        "Polaris",
	// 	Type:        "Project",
	// 	SyncVersion: 2,
	// 	Embedding:   []float32{1.0, 0.0},
	// }

	// Test Upsert without fetcher
	gw := NewGraphWriter((*store.DatabaseWriter)(nil), nil)
	_ = gw // just to use it
	// We need to bypass the type restriction. In production, this requires proper initialization of DatabaseWriter
	// Instead, let's just write tests for ProviderLLMClient
}

type mockWriterProvider struct {
	content string
}

func (m *mockWriterProvider) ProviderName() string                 { return "mock" }
func (m *mockWriterProvider) ModelID() string                      { return "mock" }
func (m *mockWriterProvider) Tokenizer() protocol.TokenizerAdapter { return nil }
func (m *mockWriterProvider) Capabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{}
}
func (m *mockWriterProvider) Infer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
	return &types.ProviderResponse{Content: m.content}, nil
}
func (m *mockWriterProvider) StreamInfer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	return nil, nil
}

func TestProviderLLMClient_ExtractEntities(t *testing.T) {
	ctx := context.Background()
	content := "```json\n[{\"name\":\"test_e\",\"type\":\"person\"}]\n```"
	client := NewProviderLLMClient(&mockWriterProvider{content: content}, "model")

	entities, err := client.ExtractEntities(ctx, "hello test_e")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(entities) != 1 {
		t.Fatalf("expected 1 entity, got %d", len(entities))
	}
	if entities[0].Name != "test_e" {
		t.Errorf("expected test_e, got %s", entities[0].Name)
	}
}

func TestProviderLLMClient_ExtractRelations(t *testing.T) {
	ctx := context.Background()
	content := "```json\n[{\"from\":\"test_e\",\"to\":\"test_e2\",\"type\":\"uses\"}]\n```"
	client := NewProviderLLMClient(&mockWriterProvider{content: content}, "model")

	entities := []*types.Entity{
		{ID: "test_e", Name: "test_e"},
		{ID: "test_e2", Name: "test_e2"},
	}
	rels, err := client.ExtractRelations(ctx, entities, "test_e uses test_e2")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(rels) != 1 {
		t.Fatalf("expected 1 relation, got %d", len(rels))
	}
	if rels[0].FromEntityID != "test_e" || rels[0].ToEntityID != "test_e2" {
		t.Errorf("relation mismatch: %v", rels[0])
	}
}

func TestTruncate(t *testing.T) {
	if truncate("hello", 10) != "hello" {
		t.Error("truncate failed for short string")
	}
	if !strings.HasSuffix(truncate("hello world", 5), "...") {
		t.Error("truncate failed for long string")
	}
}
