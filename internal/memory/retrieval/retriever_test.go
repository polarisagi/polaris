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
	hr1 := NewHybridRetrieverWithGraph(store1, graph1)

	res1, _ := hr1.Search(ctx, "query", types.SearchScope{Type: "memory"}, types.RetrievalConfig{})
	for _, r := range res1 {
		if r.Source == "node1" {
			t.Errorf("expected node1 to be skipped on kv error")
		}
	}

	// Test case 2: jsonErr != nil
	store2 := &fakeStore{getVal: []byte("invalid json")}
	graph2 := &fakeGraph{nodes: []types.ScoredNode{{ID: "node2", Score: 1.0}}}
	hr2 := NewHybridRetrieverWithGraph(store2, graph2)

	res2, _ := hr2.Search(ctx, "query", types.SearchScope{Type: "memory"}, types.RetrievalConfig{})
	for _, r := range res2 {
		if r.Source == "node2" {
			t.Errorf("expected node2 to be skipped on json error")
		}
	}
}
