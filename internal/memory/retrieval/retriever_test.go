package retrieval

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type fakeIter struct {
	done bool
}

func (f *fakeIter) Next() bool {
	if !f.done {
		f.done = true
		return true
	}
	return false
}
func (f *fakeIter) Key() []byte   { return []byte("episodic:seed1") }
func (f *fakeIter) Value() []byte { return []byte("query matched content") }
func (f *fakeIter) Close() error  { return nil }

func (f *fakeIter) Err() error { return nil }

type fakeStore struct {
	protocol.Store
	getErr error
	getVal []byte
}

func (f *fakeStore) Get(ctx context.Context, key []byte) ([]byte, error) {
	// Let the seed load properly if it's queried via Get (though graph node is what we test)
	if string(key) == "episodic:node1" || string(key) == "episodic:node2" {
		if f.getErr != nil {
			return nil, f.getErr
		}
		return f.getVal, nil
	}
	return []byte(`{"Payload":"valid"}`), nil
}

func (f *fakeStore) Scan(ctx context.Context, prefix []byte) (protocol.Iterator, error) {
	// Returns a seed so BM25 is not empty, triggering graph search
	return &fakeIter{}, nil
}

type fakeGraph struct {
	protocol.GraphTraverser
	nodes []types.ScoredNode
}

func (f *fakeGraph) SpreadingActivation(seedIDs []string, maxDepth int, energyDecay float64, dormancyThreshold float64, fanOutLimit int) ([]types.ScoredNode, error) {
	return f.nodes, nil
}

func TestHybridRetriever_GraphNodeSkip(t *testing.T) {
	ctx := context.Background()

	// Test case 1: kvErr != nil
	store1 := &fakeStore{getErr: apperr.New(apperr.CodeInternal, "kv missing")}
	graph1 := &fakeGraph{nodes: []types.ScoredNode{{ID: "node1", Score: 1.0}}}
	hr1 := NewHybridRetrieverFull(store1, graph1, nil, nil)

	res1, _ := hr1.Search(ctx, "query", types.SearchScope{Type: "memory"}, types.RetrievalConfig{})
	for _, r := range res1 {
		if r.Source == "node1" {
			t.Errorf("expected node1 to be skipped on kv error")
		}
	}

	// Test case 2: jsonErr != nil
	store2 := &fakeStore{getVal: []byte("invalid json")}
	graph2 := &fakeGraph{nodes: []types.ScoredNode{{ID: "node2", Score: 1.0}}}
	hr2 := NewHybridRetrieverFull(store2, graph2, nil, nil)

	res2, _ := hr2.Search(ctx, "query", types.SearchScope{Type: "memory"}, types.RetrievalConfig{})
	for _, r := range res2 {
		if r.Source == "node2" {
			t.Errorf("expected node2 to be skipped on json error")
		}
	}
}

type fakeSemantic struct {
	protocol.SemanticMemory
	entities []types.Entity
}

func (f *fakeSemantic) SearchEntities(ctx context.Context, query string, topK int, asOf int64) ([]types.Entity, error) {
	return f.entities, nil
}

func TestHybridRetriever_MacroIntentCommunityPriority(t *testing.T) {
	ctx := context.Background()
	store := &fakeStore{}
	hr := NewHybridRetrieverFull(store, nil, nil, nil)
	hr.semantic = &fakeSemantic{
		entities: []types.Entity{
			{ID: "ent1", Name: "常规实体总结", Type: "Person", Properties: map[string]any{"level": float64(1)}},
			{ID: "ent2", Name: "社区摘要总结", Type: "Community", Properties: map[string]any{"level": float64(1)}},
			{ID: "ent3", Name: "低级社区摘要总结", Type: "Community", Properties: map[string]any{"level": float64(0)}},
		},
	}

	res, _ := hr.Search(ctx, "总结一下全局架构", types.SearchScope{Type: "semantic"}, types.RetrievalConfig{})

	var ent1Score, ent2Score, ent3Score float64
	for _, r := range res {
		if r.Source == "ent1" {
			ent1Score = r.Score
		}
		if r.Source == "ent2" {
			ent2Score = r.Score
		}
		if r.Source == "ent3" {
			ent3Score = r.Score
		}
	}

	if ent2Score <= ent1Score {
		t.Errorf("expected community entity to be boosted over normal entity (ent2:%v ent1:%v)", ent2Score, ent1Score)
	}
	if ent2Score <= ent3Score {
		t.Errorf("expected level>=1 community to be boosted heavily (ent2:%v ent3:%v)", ent2Score, ent3Score)
	}
}
