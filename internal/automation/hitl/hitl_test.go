package hitl

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

// mockStore 实现了 protocol.Store，用于单元测试
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

func TestGatewayImpl_PromptAndRespond(t *testing.T) {
	store := newMockStore()
	gw := NewGateway(store)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	p := types.HITLPrompt{
		ID:             "hitl-123",
		CheckpointType: "test",
		PromptText:     "Approve execution?",
	}

	// 异步响应
	go func() {
		time.Sleep(50 * time.Millisecond)
		err := gw.Respond(context.Background(), p.ID, types.HITLResponse{
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

	_, err := gw.Prompt(ctx, types.HITLPrompt{ID: "hitl-456"})
	if err != context.DeadlineExceeded {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
}

// TestGatewayImpl_PromptTimeout_DeviceControlFullAccess 验证 inv_M13_07：
// 电脑操控 checkpoint 在 full_access 权限模式下超时应兜底为 auto_approve，
// 与"设置 → 设备操控 → 完全访问(上帝模式)"的产品承诺一致。
func TestGatewayImpl_PromptTimeout_DeviceControlFullAccess(t *testing.T) {
	store := newMockStore()
	gw := NewGateway(store)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	resp, err := gw.Prompt(ctx, types.HITLPrompt{
		ID:             "hitl-full-access",
		CheckpointType: types.CheckpointDeviceControlReview,
		PermissionMode: types.ModeFullAccess,
	})
	if err != nil {
		t.Fatalf("expected no error (auto_approve), got %v", err)
	}
	if resp == nil || !resp.Approved {
		t.Fatalf("expected auto-approved response, got %+v", resp)
	}
}

// TestGatewayImpl_PromptTimeout_DeviceControlAutoReview 验证 auto_review/default
// 模式下电脑操控 checkpoint 超时不受权限模式影响，维持既有 kill_pause 行为——
// 这两个模式的产品语义是"高危操作需要人审"，超时不应被自动放行。
func TestGatewayImpl_PromptTimeout_DeviceControlAutoReview(t *testing.T) {
	store := newMockStore()
	gw := NewGateway(store)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := gw.Prompt(ctx, types.HITLPrompt{
		ID:             "hitl-auto-review",
		CheckpointType: types.CheckpointDeviceControlReview,
		PermissionMode: types.ModeAutoReview,
	})
	if err != context.DeadlineExceeded {
		t.Fatalf("expected deadline exceeded (kill_pause), got %v", err)
	}
}

// TestGatewayImpl_PromptTimeout_DeviceControlFullAccessButTainted 验证
// TaintLevel 硬地板优先级高于权限模式：即使 full_access，TaintLevel>=Medium
// 时超时仍必须 auto_deny，防止被污染的 Agent 拿设备操控设置当挡箭牌。
// TestGatewayImpl_PromptTimeout_DeadlineNsIsAbsolute 回归测试：DeadlineNs 曾被
// Prompt() 误当作相对 Duration 又叠加一次 time.Now()，导致所有调用方构造的
// "N 分钟/小时后超时"实际上被推迟到约 56 年后，超时机制形同虚设（2026-07-07
// 修复）。本测试模拟真实调用方写法：不设置外层 ctx 超时（用 context.Background()），
// 完全依赖 DeadlineNs 自己建立截止时间，验证 Prompt() 确实会在 DeadlineNs
// 指定的绝对时间点附近返回，而不是永久阻塞。
func TestGatewayImpl_PromptTimeout_DeadlineNsIsAbsolute(t *testing.T) {
	store := newMockStore()
	gw := NewGateway(store)

	done := make(chan struct{})
	go func() {
		_, _ = gw.Prompt(context.Background(), types.HITLPrompt{
			ID:         "hitl-deadline-ns",
			DeadlineNs: time.Now().Add(50 * time.Millisecond).UnixNano(),
		})
		close(done)
	}()

	select {
	case <-done:
		// 正常：在 DeadlineNs 附近及时返回
	case <-time.After(2 * time.Second):
		t.Fatal("Prompt() did not honor DeadlineNs as an absolute deadline — timeout mechanism is broken")
	}
}

func TestGatewayImpl_PromptTimeout_DeviceControlFullAccessButTainted(t *testing.T) {
	store := newMockStore()
	gw := NewGateway(store)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	resp, err := gw.Prompt(ctx, types.HITLPrompt{
		ID:             "hitl-full-access-tainted",
		CheckpointType: types.CheckpointDeviceControlReview,
		PermissionMode: types.ModeFullAccess,
		TaintLevel:     types.TaintMedium,
	})
	if err != nil {
		t.Fatalf("expected no error (auto_deny resolves without propagating ctx err), got %v", err)
	}
	if resp == nil || resp.Approved {
		t.Fatalf("expected auto-denied response despite full_access, got %+v", resp)
	}
}
