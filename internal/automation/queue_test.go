package automation

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// mockStore 实现了 protocol.Store，用于单元测试。
// 加锁保护（同 internal/automation/hitl/hitl_test.go 既有 mockStore 模式）：
// TestSQLiteScheduler_NotifyTaskTerminal_BackgroundFailure 等测试会连续两次
// 调用 scanAndDispatch，第一次内部经 SafeGo 派生的后台 goroutine 回写任务状态
// 与测试主 goroutine 发起的第二次 Scan 之间没有 happens-before 关系，裸 map
// 读写在 `go test -race` 下会被正确判定为数据竞争。
type mockStore struct {
	mu   sync.RWMutex
	data map[string][]byte
}

func newMockStore() *mockStore {
	return &mockStore{data: make(map[string][]byte)}
}

func (m *mockStore) Get(ctx context.Context, key []byte) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if val, ok := m.data[string(key)]; ok {
		return val, nil
	}
	return nil, apperr.New(apperr.CodeNotFound, "not found")
}

func (m *mockStore) Put(ctx context.Context, key, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[string(key)] = value
	return nil
}

func (m *mockStore) Delete(ctx context.Context, key []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, string(key))
	return nil
}

func (m *mockStore) Scan(ctx context.Context, prefix []byte) (protocol.Iterator, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
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

func (m *mockStore) BatchWrite(ctx context.Context, ops []types.Op) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, op := range ops {
		if op.Type == types.OpPut {
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

func (m *mockStore) Capabilities() types.StoreCapabilities {
	return types.StoreCapabilities{}
}

func (m *mockStore) Close() error { return nil }

type mockTxn struct {
	store *mockStore
}

func (txn *mockTxn) Get(key []byte) ([]byte, error) {
	txn.store.mu.RLock()
	defer txn.store.mu.RUnlock()
	if val, ok := txn.store.data[string(key)]; ok {
		return val, nil
	}
	return nil, apperr.New(apperr.CodeNotFound, "not found")
}

func (txn *mockTxn) Put(key, value []byte) error {
	txn.store.mu.Lock()
	defer txn.store.mu.Unlock()
	txn.store.data[string(key)] = value
	return nil
}

func (txn *mockTxn) Delete(key []byte) error {
	txn.store.mu.Lock()
	defer txn.store.mu.Unlock()
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
		it.store.mu.RLock()
		defer it.store.mu.RUnlock()
		return it.store.data[it.keys[it.index]]
	}
	return nil
}

func (it *mockIterator) Err() error   { return nil }
func (it *mockIterator) Close() error { return nil }

func TestSQLiteScheduler_SubmitAndGet(t *testing.T) {
	store := newMockStore()
	scheduler := NewSQLiteScheduler(store)

	ctx := context.Background()
	task := types.Task{
		Type:     "test_task",
		Priority: 1,
	}

	id, err := scheduler.Submit(ctx, task)
	if err != nil {
		t.Fatalf("submit failed: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty id")
	}

	savedTask, err := scheduler.Get(ctx, id)
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if savedTask.Type != task.Type {
		t.Fatalf("expected task type %s, got %s", task.Type, savedTask.Type)
	}
}

func TestSQLiteScheduler_Subscribe(t *testing.T) {
	store := newMockStore()
	scheduler := NewSQLiteScheduler(store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	taskID := "task_sub_1"
	ch, err := scheduler.Subscribe(ctx, taskID)
	if err != nil {
		t.Fatalf("subscribe failed: %v", err)
	}

	// Trigger event via Submit with same ID
	scheduler.Submit(ctx, types.Task{ID: taskID})

	select {
	case ev := <-ch:
		if ev.State != "submitted" {
			t.Fatalf("expected state submitted, got %s", ev.State)
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("timeout waiting for event")
	}
}
