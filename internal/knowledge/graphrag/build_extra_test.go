package graphrag

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/internal/protocol"
)

type mockSemanticMemory struct {
	protocol.SemanticMemory
}

func (m *mockSemanticMemory) UpsertFact(ctx context.Context, e types.Entity, taint types.TaintLevel) error {
	return nil
}
func (m *mockSemanticMemory) UpsertRelation(ctx context.Context, r types.Relation, taint types.TaintLevel) error {
	return nil
}

type mockDocFetcher struct{}

func (m *mockDocFetcher) FetchText(ctx context.Context, docID string) (string, error) {
	return "Polaris is a good engine.", nil
}

func TestGraphBuildPipeline_Run(t *testing.T) {
	ctx := context.Background()

	pipeline := NewGraphBuildPipeline(nil, 0, &mockSemanticMemory{})
	pipeline.SetDocFetcher(&mockDocFetcher{})

	err := pipeline.Run(ctx, "doc1")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
}
