package plugin

import (
	"context"
	"testing"
)

type mockCognitiveIndexer struct {
	ftsCalls []string
	vecCalls map[string][]float32
}

func (m *mockCognitiveIndexer) FTSIndex(docID, text string) error {
	m.ftsCalls = append(m.ftsCalls, docID)
	return nil
}

func (m *mockCognitiveIndexer) VecUpsert(id string, embedding []float32) error {
	if m.vecCalls == nil {
		m.vecCalls = make(map[string][]float32)
	}
	m.vecCalls[id] = embedding
	return nil
}

type mockEmbedder struct {
	callCount int
	retVec    []float32
}

func (m *mockEmbedder) Embed(text string) []float32 {
	m.callCount++
	return m.retVec
}

func TestEmbeddingIndexer(t *testing.T) {
	ci := &mockCognitiveIndexer{}
	e := &mockEmbedder{retVec: []float32{1.0, 2.0}}
	idx := NewEmbeddingIndexer(ci, e)

	entries := []CatalogEntry{
		{ID: "123", Name: "TestExt", Description: "Test Desc"},
	}

	idx.IndexEntries(context.Background(), entries)

	if len(ci.ftsCalls) != 1 || ci.ftsCalls[0] != "ext_123" {
		t.Fatalf("unexpected FTSIndex calls: %v", ci.ftsCalls)
	}
	if len(ci.vecCalls) != 1 || len(ci.vecCalls["ext_123"]) != 2 {
		t.Fatalf("unexpected VecUpsert calls: %v", ci.vecCalls)
	}
}
