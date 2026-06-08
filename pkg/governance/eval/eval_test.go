package eval

import (
	"context"
	"strings"
	"testing"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/governance/policy"
)

// mockStore 实现了 protocol.Store，用于单元测试
type mockStore struct {
	data map[string][]byte
}

func newMockStore() *mockStore {
	return &mockStore{data: make(map[string][]byte)}
}

func (m *mockStore) Get(ctx context.Context, key []byte) ([]byte, error) {
	if val, ok := m.data[string(key)]; ok {
		return val, nil
	}
	return nil, perrors.New(perrors.CodeNotFound, "not found")
}

func (m *mockStore) Put(ctx context.Context, key, value []byte) error {
	m.data[string(key)] = value
	return nil
}

func (m *mockStore) Delete(ctx context.Context, key []byte) error {
	delete(m.data, string(key))
	return nil
}

func (m *mockStore) Scan(ctx context.Context, prefix []byte) (protocol.Iterator, error) {
	var keys []string
	for k := range m.data {
		if strings.HasPrefix(k, string(prefix)) {
			keys = append(keys, k)
		}
	}
	return &mockIterator{
		store: m,
		keys:  keys,
		index: -1,
	}, nil
}

func (m *mockStore) BatchWrite(ctx context.Context, ops []protocol.Op) error {
	for _, op := range ops {
		if op.Type == protocol.OpPut {
			m.data[string(op.Key)] = op.Value
		} else {
			delete(m.data, string(op.Key))
		}
	}
	return nil
}

func (m *mockStore) Txn(ctx context.Context, fn func(protocol.Transaction) error) error {
	return fn(&mockTxn{store: m})
}

func (m *mockStore) Capabilities() protocol.StoreCapabilities {
	return protocol.StoreCapabilities{}
}

func (m *mockStore) Close() error { return nil }

type mockTxn struct {
	store *mockStore
}

func (txn *mockTxn) Get(key []byte) ([]byte, error) {
	if val, ok := txn.store.data[string(key)]; ok {
		return val, nil
	}
	return nil, perrors.New(perrors.CodeNotFound, "not found")
}

func (txn *mockTxn) Put(key, value []byte) error {
	txn.store.data[string(key)] = value
	return nil
}

func (txn *mockTxn) Delete(key []byte) error {
	delete(txn.store.data, string(key))
	return nil
}

func (txn *mockTxn) Scan(prefix []byte) (protocol.Iterator, error) {
	return txn.store.Scan(context.Background(), prefix)
}

type mockIterator struct {
	store *mockStore
	keys  []string
	index int
}

func (it *mockIterator) Next() bool {
	it.index++
	return it.index < len(it.keys)
}

func (it *mockIterator) Key() []byte {
	if it.index >= 0 && it.index < len(it.keys) {
		return []byte(it.keys[it.index])
	}
	return nil
}

func (it *mockIterator) Value() []byte {
	if it.index >= 0 && it.index < len(it.keys) {
		return it.store.data[it.keys[it.index]]
	}
	return nil
}

func (it *mockIterator) Err() error   { return nil }
func (it *mockIterator) Close() error { return nil }

func TestSQLiteEvalStore_PutAndGetCases(t *testing.T) {
	store := newMockStore()
	evalStore := NewSQLiteEvalStore(store)

	ctx := context.Background()

	// Put cases
	c1 := EvalCase{ID: "case-1", Level: Level1Assert}
	c2 := EvalCase{ID: "case-2", Level: Level2Schema}

	err := evalStore.PutCase(ctx, "training", policy.RoleM9Optimizer, c1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	err = evalStore.PutCase(ctx, "validation", policy.RoleM9Optimizer, c2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Get training cases
	trainingCases, err := evalStore.GetTrainingCases(ctx, policy.RoleM9Optimizer, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(trainingCases) != 1 {
		t.Fatalf("expected 1 training case, got %d", len(trainingCases))
	}

	// Get validation cases
	validationCases, err := evalStore.GetValidationCases(ctx, policy.RoleM9Optimizer, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(validationCases) != 1 {
		t.Fatalf("expected 1 validation case, got %d", len(validationCases))
	}
}

func TestRunnerImpl_RunSuite(t *testing.T) {
	store := newMockStore()
	evalStore := NewSQLiteEvalStore(store)
	runner := NewRunner(store, evalStore)

	ctx := context.Background()
	evalStore.PutCase(ctx, "training", policy.RoleM9Optimizer, EvalCase{ID: "case-1"})
	evalStore.PutCase(ctx, "training", policy.RoleM9Optimizer, EvalCase{ID: "case-2"})

	report, err := runner.RunSuite(ctx, "training", "candidate-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if report.TotalCases != 2 {
		t.Fatalf("expected 2 total cases, got %d", report.TotalCases)
	}
	if report.PassCount != 2 {
		t.Fatalf("expected 2 passes, got %d", report.PassCount)
	}
	if report.Status != "completed" {
		t.Fatalf("expected status completed, got %s", report.Status)
	}
}

func TestRunnerImpl_RunReplay(t *testing.T) {
	store := newMockStore()
	evalStore := NewSQLiteEvalStore(store)
	runner := NewRunner(store, evalStore)

	report, err := runner.RunReplay(context.Background(), "session-123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !report.Consistent {
		t.Fatal("expected replay to be consistent")
	}
}
