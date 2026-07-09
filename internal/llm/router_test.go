package llm

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ─── CircuitBreaker 测试 ───────────────────────────────────────────────────────

func TestCircuitBreaker_OpenAfter5Failures(t *testing.T) {
	cb := newCircuitBreaker(config.M1RouterThresholds{})
	for i := 0; i < 5; i++ {
		if !cb.Allow() {
			t.Fatalf("circuit should be closed at failure %d", i)
		}
		cb.RecordFailure()
	}
	if cb.Allow() {
		t.Fatal("circuit must be open after 5 consecutive failures")
	}
}

func TestCircuitBreaker_RecoveryOnSuccess(t *testing.T) {
	cb := newCircuitBreaker(config.M1RouterThresholds{})
	for i := 0; i < 4; i++ {
		cb.RecordFailure()
	}
	cb.RecordSuccess()
	// 成功后失败计数归零，再失败 5 次才 open
	for i := 0; i < 4; i++ {
		cb.RecordFailure()
	}
	if !cb.Allow() {
		t.Fatal("circuit should still be closed (only 4 failures after reset)")
	}
}

// ─── mockProvider 测试 Provider ───────────────────────────────────────────────

type mockProvider struct {
	failCount int
	callCount int
	caps      types.ProviderCapabilities
}

func (m *mockProvider) Infer(_ context.Context, _ []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
	m.callCount++
	if m.callCount <= m.failCount {
		return nil, errProviderUnavailable
	}
	return &types.ProviderResponse{Content: "ok", FinishReason: "stop"}, nil
}

func (m *mockProvider) StreamInfer(_ context.Context, _ []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	ch := make(chan types.StreamEvent, 1)
	ch <- types.StreamEvent{Type: types.StreamTextDelta, Content: "ok"}
	close(ch)
	return ch, nil
}

func (m *mockProvider) Capabilities() types.ProviderCapabilities { return m.caps }
func (m *mockProvider) Tokenizer() protocol.TokenizerAdapter     { return &SimpleTokenizer{} }
func (m *mockProvider) ModelID() string                          { return "mock" }

var errProviderUnavailable = apperr.New(apperr.CodeProviderExhausted, "provider unavailable")

// ─── InferenceRouter 测试 ─────────────────────────────────────────────────────

func TestInferenceRouter_Failover(t *testing.T) {
	reg := NewProviderRegistry(config.M1RouterThresholds{})
	// primary: 第一次调用失败
	primary := &mockProvider{failCount: 1, caps: types.ProviderCapabilities{CostPer1KInput: 1.0}}
	// secondary: 始终成功
	secondary := &mockProvider{caps: types.ProviderCapabilities{CostPer1KInput: 2.0}}

	reg.Register("primary", "Primary", primary)
	reg.Register("secondary", "Secondary", secondary)

	router := NewInferenceRouter(reg, nil)
	resp, err := router.Infer(context.Background(), []types.Message{{Role: "user", Content: "hello"}})
	// primary 失败后应 failover 至 secondary
	if err != nil {
		t.Fatalf("expected failover success, got err: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("expected 'ok', got '%s'", resp.Content)
	}
}

func TestInferenceRouter_AllProvidersCircuitOpen(t *testing.T) {
	reg := NewProviderRegistry(config.M1RouterThresholds{})
	p := &mockProvider{failCount: 100}
	reg.Register("only", "Only", p)
	// 手动打开熔断器
	reg.entries["only"].cb.state.Store(int32(circuitOpen))
	reg.entries["only"].cb.openUntil.Store(^int64(0)) // 永不恢复

	router := NewInferenceRouter(reg, nil)
	_, err := router.Infer(context.Background(), []types.Message{})
	if err == nil {
		t.Fatal("should fail when all circuits are open")
	}
}

func TestInferenceRouter_HealthScorePreference(t *testing.T) {
	reg := NewProviderRegistry(config.M1RouterThresholds{})
	// cheap: 低成本，始终成功
	cheap := &mockProvider{caps: types.ProviderCapabilities{CostPer1KInput: 0.1, SupportsStreaming: true}}
	// expensive: 高成本
	expensive := &mockProvider{caps: types.ProviderCapabilities{CostPer1KInput: 9.0, SupportsStreaming: true}}

	reg.Register("cheap", "Cheap", cheap)
	reg.Register("expensive", "Expensive", expensive)

	// cheap 的 healthScore 应更高（低成本 → 高 costScore）
	cheapEntry := reg.entries["cheap"]
	expEntry := reg.entries["expensive"]
	if cheapEntry.healthScore() <= expEntry.healthScore() {
		t.Fatalf("cheap provider (cost=0.1) should have higher health score than expensive (cost=9.0)")
	}
}

func TestClearBytes(t *testing.T) {
	s := []byte("secret-api-key-12345")
	ClearBytes(s)
	for _, v := range s {
		if v != 0 {
			t.Fatal("ClearBytes should zero the slice")
		}
	}
}

type mockOutboxWriter struct {
	mu      sync.Mutex
	entries []protocol.OutboxEntry
}

func (m *mockOutboxWriter) Write(ctx context.Context, entry protocol.OutboxEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, entry)
	return nil
}

func (m *mockOutboxWriter) getEntries() []protocol.OutboxEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := make([]protocol.OutboxEntry, len(m.entries))
	copy(res, m.entries)
	return res
}

func TestCircuitBreaker_OnRecovery(t *testing.T) {
	reg := NewProviderRegistry(config.M1RouterThresholds{})
	p := &mockProvider{failCount: 0}
	reg.Register("recover_test", "RecoverTest", p)

	outbox := &mockOutboxWriter{}
	ir := NewInferenceRouter(reg, nil)
	ir.InjectOutboxWriter(outbox)

	entry := reg.entries["recover_test"]
	// 强制状态为 HalfOpen
	entry.cb.state.Store(int32(circuitHalfOpen))

	// 触发 RecordSuccess
	entry.recordOutcome(true, func() {
		reg.mu.RLock()
		fn := reg.onRecovery
		name := entry.name
		reg.mu.RUnlock()
		if fn != nil {
			fn(name)
		}
	})

	// 等待异步回调执行
	time.Sleep(50 * time.Millisecond)
	entries := outbox.getEntries()

	if len(entries) == 0 {
		t.Fatal("expected outbox entry to be created on recovery")
	}

	found := false
	for _, evt := range entries {
		if evt.Operation == "provider_recovery" {
			found = true
			if !strings.Contains(string(evt.Payload), "m4_provider_recovery") {
				t.Errorf("payload missing event_type: %s", evt.Payload)
			}
			if !strings.Contains(string(evt.Payload), "recover_test") {
				t.Errorf("payload missing provider_name: %s", evt.Payload)
			}
		}
	}
	if !found {
		t.Errorf("did not find provider_recovery outbox event")
	}
}

func TestErrAllProvidersFailed(t *testing.T) {
	reg := NewProviderRegistry(config.M1RouterThresholds{})
	router := NewInferenceRouter(reg, nil)

	// Without any providers, it should fail with protocol.ErrAllProvidersFailed
	_, err := router.Infer(context.Background(), []types.Message{{Role: "user", Content: "Hello"}})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, protocol.ErrAllProvidersFailed) {
		t.Fatalf("expected protocol.ErrAllProvidersFailed, got %v", err)
	}

	// For stream as well
	_, err = router.StreamInfer(context.Background(), []types.Message{{Role: "user", Content: "Hello"}})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, protocol.ErrAllProvidersFailed) {
		t.Fatalf("expected protocol.ErrAllProvidersFailed, got %v", err)
	}
}
