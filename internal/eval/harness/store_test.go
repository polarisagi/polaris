package harness

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/eval/control"
	"github.com/polarisagi/polaris/internal/protocol"

	"github.com/polarisagi/polaris/pkg/types"
)

// memKVStore 是 protocol.Store 的最小内存实现，仅供本文件测试使用。
// 不复用 runner_test.go 的 mockSQLiteStore：那个类型只重写了 Scan，
// Get/Put/Delete 调用会直接 panic（内嵌的 protocol.Store 字段为 nil interface）。
type memKVStore struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newMemKVStore() *memKVStore {
	return &memKVStore{data: make(map[string][]byte)}
}

func (m *memKVStore) Get(_ context.Context, key []byte) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[string(key)]
	if !ok {
		return nil, nil
	}
	return v, nil
}

func (m *memKVStore) Put(_ context.Context, key, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[string(key)] = value
	return nil
}

func (m *memKVStore) Delete(_ context.Context, key []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, string(key))
	return nil
}

func (m *memKVStore) Scan(_ context.Context, prefix []byte) (protocol.Iterator, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var keys []string
	for k := range m.data {
		if bytes.HasPrefix([]byte(k), prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return &memIterator{store: m, keys: keys, idx: -1}, nil
}

func (m *memKVStore) BatchWrite(ctx context.Context, ops []types.Op) error {
	for _, op := range ops {
		if err := m.Put(ctx, op.Key, op.Value); err != nil {
			return err
		}
	}
	return nil
}

func (m *memKVStore) Txn(ctx context.Context, fn func(tx protocol.Transaction) error) error {
	return fn(&memTxn{store: m, ctx: ctx})
}

func (m *memKVStore) Capabilities() types.StoreCapabilities { return types.StoreCapabilities{} }
func (m *memKVStore) Close() error                          { return nil }

type memIterator struct {
	store *memKVStore
	keys  []string
	idx   int
}

func (it *memIterator) Next() bool {
	it.idx++
	return it.idx < len(it.keys)
}
func (it *memIterator) Key() []byte { return []byte(it.keys[it.idx]) }
func (it *memIterator) Value() []byte {
	it.store.mu.Lock()
	defer it.store.mu.Unlock()
	return it.store.data[it.keys[it.idx]]
}
func (it *memIterator) Err() error   { return nil }
func (it *memIterator) Close() error { return nil }

type memTxn struct {
	store *memKVStore
	ctx   context.Context
}

func (t *memTxn) Get(key []byte) ([]byte, error) { return t.store.Get(t.ctx, key) }
func (t *memTxn) Put(key, value []byte) error    { return t.store.Put(t.ctx, key, value) }
func (t *memTxn) Delete(key []byte) error        { return t.store.Delete(t.ctx, key) }
func (t *memTxn) Scan(prefix []byte) (protocol.Iterator, error) {
	return t.store.Scan(t.ctx, prefix)
}

// newTestMetaEvalStore 构造一个未配置公钥（放行模式）的 SQLiteEvalStore，
// 白名单只放行 control.RoleMetaAuditor:control.PartitionMetaHoldout。
func newTestMetaEvalStore() *SQLiteEvalStore {
	engine := control.NewEngine(nil)
	return NewSQLiteEvalStore(newMemKVStore(), engine)
}

func TestPutMetaHoldoutCase_RequiresValidSignatureWhenConfigured(t *testing.T) {
	s := newTestMetaEvalStore()
	c := EvalCase{ID: "case-1", FalsifiabilityScore: 0.8, BehaviorType: BehaviorSemanticQuality}

	// 1. 未配置公钥：必须 fail-closed（GR-10-001 修复漏洞后不再允许放行）
	if err := s.PutMetaHoldoutCase(context.Background(), c, nil); err == nil {
		t.Fatalf("expected error in no-pubkey mode due to fail-closed, got nil")
	}
	// 不再检查是否写入，因为写入失败了

	// 2. 配置公钥后，无效签名必须被拒绝。
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	// Create a new store that has the pubkey configured
	sWithKey := NewSQLiteEvalStore(newMemKVStore(), control.NewEngine(map[string]ed25519.PublicKey{control.RoleMetaAuditor: pub}))

	c2 := EvalCase{ID: "case-2", FalsifiabilityScore: 0.9, BehaviorType: BehaviorSafetyBoundary}
	if err := sWithKey.PutMetaHoldoutCase(context.Background(), c2, nil); err == nil {
		t.Fatal("expected error when pubkey configured but signature is nil")
	}

	// 3. 正确签名（control.RoleMetaAuditor:control.PartitionMetaHoldout）可写入。
	payload := []byte(fmt.Sprintf("%s:%s:%d", control.RoleMetaAuditor, control.PartitionMetaHoldout, time.Now().Unix()))
	sig := ed25519.Sign(priv, payload)
	if err := sWithKey.PutMetaHoldoutCase(context.Background(), c2, sig); err != nil {
		t.Fatalf("expected nil error with valid signature, got %v", err)
	}

	cases, err := sWithKey.GetMetaHoldoutCases(context.Background(), control.RoleMetaAuditor, sig)
	if err != nil {
		t.Fatalf("GetMetaHoldoutCases failed: %v", err)
	}
	if len(cases) != 1 {
		t.Fatalf("expected 1 case after put, got %d", len(cases))
	}
}

func TestGetMetaHoldoutCases_RejectsNonMetaAuditorRole(t *testing.T) {
	s := newTestMetaEvalStore()
	// control.RoleM9Optimizer 不在 meta_holdout 分区白名单中（V8-S2 隔离核心保证）。
	_, err := s.GetMetaHoldoutCases(context.Background(), control.RoleM9Optimizer, nil)
	if err == nil {
		t.Fatal("expected error: RoleM9Optimizer must not be able to read meta_holdout partition")
	}
}

func TestRecordMetaAuditResult_And_LatestMetaAudit_RoundTrip(t *testing.T) {
	s := newTestMetaEvalStore()
	ctx := context.Background()

	// 1. 从未审计过：ok=false。
	_, _, ok, err := s.LatestMetaAudit(ctx)
	if err != nil {
		t.Fatalf("LatestMetaAudit failed: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false before any RecordMetaAuditResult call")
	}

	// 2. 写入一条结论并读回。
	rec := MetaAuditRecord{
		Passed:               true,
		MedianFalsifiability: 0.75,
		TotalCases:           12,
		Reasons:              nil,
		ComputedAt:           time.Now().Unix(),
	}
	if err := s.RecordMetaAuditResult(ctx, rec); err != nil {
		t.Fatalf("RecordMetaAuditResult failed: %v", err)
	}
	passed, computedAt, ok, err := s.LatestMetaAudit(ctx)
	if err != nil {
		t.Fatalf("LatestMetaAudit failed: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true after RecordMetaAuditResult")
	}
	if !passed {
		t.Fatal("expected passed=true")
	}
	if computedAt.Unix() != rec.ComputedAt {
		t.Fatalf("expected computedAt=%d, got %d", rec.ComputedAt, computedAt.Unix())
	}

	// 3. 只保留最新一次结论（覆盖写）。
	rec2 := rec
	rec2.Passed = false
	rec2.ComputedAt = time.Now().Add(time.Hour).Unix()
	if err := s.RecordMetaAuditResult(ctx, rec2); err != nil {
		t.Fatalf("RecordMetaAuditResult (2nd) failed: %v", err)
	}
	passed, computedAt, ok, err = s.LatestMetaAudit(ctx)
	if err != nil || !ok {
		t.Fatalf("LatestMetaAudit (after 2nd write) failed: err=%v ok=%v", err, ok)
	}
	if passed {
		t.Fatal("expected passed=false after overwriting with rec2")
	}
	if computedAt.Unix() != rec2.ComputedAt {
		t.Fatalf("expected computedAt=%d, got %d", rec2.ComputedAt, computedAt.Unix())
	}
}

func TestPutCase_RejectsUnknownPartition(t *testing.T) {
	s := newTestMetaEvalStore()
	c := EvalCase{ID: "x"}
	err := s.PutCase(context.Background(), "not_a_real_partition", control.RoleMetaAuditor, c)
	if err == nil {
		t.Fatal("expected error for unknown partition")
	}
	if !strings.Contains(err.Error(), "invalid partition") {
		t.Fatalf("expected 'invalid partition' in error, got %v", err)
	}
}
