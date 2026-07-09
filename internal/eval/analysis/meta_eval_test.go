package analysis

import (
	"github.com/polarisagi/polaris/internal/eval/harness"

	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/eval/control"
	"github.com/polarisagi/polaris/internal/protocol"
)

type mockSQLiteStore struct {
	protocol.Store
	vals [][]byte
}

func (m *mockSQLiteStore) Scan(ctx context.Context, prefix []byte) (protocol.Iterator, error) {
	return &mockIterator{values: m.vals}, nil
}

func TestMetaEvalSentinel(t *testing.T) {
	s := NewMetaEvalSentinel(harness.NewSQLiteEvalStore(&mockSQLiteStore{}, control.NewEngine(nil)))

	// Empty store should fail
	res, err := s.RunMetaEvalSuite(context.Background(), "m9_optimizer")
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed {
		t.Errorf("expected failure on empty store")
	}

	// Create some mock cases
	c1 := `{"id":"1", "behavior_type":"tool_call_sequence", "falsifiability_score": 0.9}`
	c2 := `{"id":"2", "behavior_type":"semantic_quality", "falsifiability_score": 0.8}`
	c3 := `{"id":"3", "behavior_type":"safety_boundary", "falsifiability_score": 0.7}`

	store := &mockSQLiteStore{
		vals: [][]byte{[]byte(c1), []byte(c2), []byte(c3)},
	}
	s.store = harness.NewSQLiteEvalStore(store, control.NewEngine(nil))

	// We only have 1 of each behavior type, but min required is 3, so it should fail
	res, err = s.RunMetaEvalSuite(context.Background(), "m9_optimizer")
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed {
		t.Errorf("expected failure due to insufficient behavior type coverage")
	}

	if res.MedianFalsifiability != 0.8 {
		t.Errorf("expected 0.8 median falsifiability, got %v", res.MedianFalsifiability)
	}
}

func TestMedianF64(t *testing.T) {
	if medianF64([]float64{1, 2, 3}) != 2 {
		t.Errorf("expected 2")
	}
	if medianF64([]float64{1, 2, 3, 4}) != 2.5 {
		t.Errorf("expected 2.5")
	}
}

type mockIterator struct {
	values [][]byte
	idx    int
}

func (m *mockIterator) Next() bool {
	if m.idx < len(m.values) {
		m.idx++
		return true
	}
	return false
}

func (m *mockIterator) Key() []byte   { return nil }
func (m *mockIterator) Value() []byte { return m.values[m.idx-1] }
func (m *mockIterator) Err() error    { return nil }
func (m *mockIterator) Close() error  { return nil }
func (m *mockIterator) Seek([]byte)   {}
