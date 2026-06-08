package hitl

import (
	"context"
	"strings"
	"testing"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
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

func TestGatewayImpl_PromptAndRespond(t *testing.T) {
	store := newMockStore()
	gw := NewGateway(store)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	p := protocol.HITLPrompt{
		ID:             "hitl-123",
		CheckpointType: "test",
		PromptText:     "Approve execution?",
	}

	// 异步响应
	go func() {
		time.Sleep(50 * time.Millisecond)
		err := gw.Respond(context.Background(), p.ID, protocol.HITLResponse{
			OptionKey: "approve",
		})
		if err != nil {
			t.Errorf("respond failed: %v", err)
		}
	}()

	resp, err := gw.Prompt(ctx, p)
	if err != nil {
		t.Fatalf("prompt failed: %v", err)
	}
	if resp.OptionKey != "approve" {
		t.Fatalf("expected approve, got %s", resp.OptionKey)
	}
}

func TestGatewayImpl_PromptTimeout(t *testing.T) {
	store := newMockStore()
	gw := NewGateway(store)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := gw.Prompt(ctx, protocol.HITLPrompt{ID: "hitl-456"})
	if err != context.DeadlineExceeded {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
}
