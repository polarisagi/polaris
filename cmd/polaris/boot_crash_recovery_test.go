package main

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/eval/harness"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// fakeKVStore — protocol.Store 最小内存实现，供 TrajectoryRecorder 扫描。
// ============================================================================

type fakeKVStore struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newFakeKVStore() *fakeKVStore { return &fakeKVStore{data: make(map[string][]byte)} }

func (s *fakeKVStore) Get(_ context.Context, key []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data[string(key)], nil
}

func (s *fakeKVStore) Put(_ context.Context, key, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[string(key)] = append([]byte(nil), value...)
	return nil
}

func (s *fakeKVStore) Delete(_ context.Context, key []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, string(key))
	return nil
}

type fakeKVEntry struct{ key, value []byte }

type fakeKVIterator struct {
	entries []fakeKVEntry
	idx     int
}

func (it *fakeKVIterator) Next() bool    { it.idx++; return it.idx < len(it.entries) }
func (it *fakeKVIterator) Key() []byte   { return it.entries[it.idx].key }
func (it *fakeKVIterator) Value() []byte { return it.entries[it.idx].value }
func (it *fakeKVIterator) Err() error    { return nil }
func (it *fakeKVIterator) Close() error  { return nil }

func (s *fakeKVStore) Scan(_ context.Context, prefix []byte) (protocol.Iterator, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for k := range s.data {
		if strings.HasPrefix(k, string(prefix)) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	entries := make([]fakeKVEntry, 0, len(keys))
	for _, k := range keys {
		entries = append(entries, fakeKVEntry{key: []byte(k), value: s.data[k]})
	}
	return &fakeKVIterator{entries: entries, idx: -1}, nil
}

func (s *fakeKVStore) BatchWrite(_ context.Context, _ []types.Op) error { return nil }
func (s *fakeKVStore) Txn(_ context.Context, _ func(tx protocol.Transaction) error) error {
	return nil
}
func (s *fakeKVStore) Capabilities() types.StoreCapabilities { return types.StoreCapabilities{} }
func (s *fakeKVStore) Close() error                          { return nil }

// ============================================================================
// fakeAgentController / fakeAgentPool
// ============================================================================

type fakeAgentController struct {
	replayCalls []protocol.ReplayLLMCall
	intent      []byte
	sendCalled  bool
	streamCh    chan types.AgentStreamEvent
}

func newFakeAgentController() *fakeAgentController {
	return &fakeAgentController{streamCh: make(chan types.AgentStreamEvent)}
}

func (f *fakeAgentController) AgentID() string                                 { return "fake" }
func (f *fakeAgentController) SetTaskIntent(intent []byte)                     { f.intent = intent }
func (f *fakeAgentController) SurpriseIndex() float64                          { return 0 }
func (f *fakeAgentController) Memory() protocol.MemoryFacade                   { return nil }
func (f *fakeAgentController) Interrupt(_ types.InterruptRequest)              {}
func (f *fakeAgentController) SetPreferences(map[string]string)                {}
func (f *fakeAgentController) CurrentState() types.AgentState                  { return types.AgentStateComplete }
func (f *fakeAgentController) ConfigInfo() map[string]any                      { return nil }
func (f *fakeAgentController) SetMonthlyBudgetUSD(float64)                     {}
func (f *fakeAgentController) InjectReplayData(calls []protocol.ReplayLLMCall) { f.replayCalls = calls }
func (f *fakeAgentController) SubscribeStream(_ context.Context) <-chan types.AgentStreamEvent {
	return f.streamCh
}

// SendIntent 模拟"立即处理完成"：关闭流，驱动 recoverOneSession 的消费循环退出。
func (f *fakeAgentController) SendIntent(_ types.AgentTrigger) error {
	f.sendCalled = true
	close(f.streamCh)
	return nil
}

type fakeAgentPool struct {
	acquireCalled     bool
	acquiredSessionID string
	ctrl              *fakeAgentController
}

func (p *fakeAgentPool) Acquire(_ context.Context, sessionID string) (protocol.AgentController, func(), error) {
	p.acquireCalled = true
	p.acquiredSessionID = sessionID
	return p.ctrl, func() {}, nil
}

func (p *fakeAgentPool) AcquireHeadless(_ context.Context, _ types.Intent, _ ...types.HeadlessOption) (*types.AgentResult, error) {
	return nil, nil
}

// ============================================================================
// lastUserMessage
// ============================================================================

func newTestChatMessagesDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE chat_messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL,
		role TEXT NOT NULL,
		content TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("create chat_messages: %v", err)
	}
	return db
}

func TestLastUserMessage_ReturnsMostRecent(t *testing.T) {
	db := newTestChatMessagesDB(t)
	ctx := context.Background()
	for _, m := range []struct{ role, content string }{
		{"user", "first message"},
		{"assistant", "reply"},
		{"user", "second message"},
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO chat_messages (session_id, role, content) VALUES (?, ?, ?)`, "sess-1", m.role, m.content); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	got, err := lastUserMessage(ctx, db, "sess-1")
	if err != nil {
		t.Fatalf("lastUserMessage failed: %v", err)
	}
	if got != "second message" {
		t.Errorf("expected latest user message, got %q", got)
	}
}

func TestLastUserMessage_NoRows(t *testing.T) {
	db := newTestChatMessagesDB(t)
	if _, err := lastUserMessage(context.Background(), db, "unknown-session"); err == nil {
		t.Error("expected error (sql.ErrNoRows) when no user message exists for session")
	}
}

// ============================================================================
// recoverOneSession
// ============================================================================

func TestRecoverOneSession_SkipsUnsafeLastState(t *testing.T) {
	ctx := context.Background()
	kv := newFakeKVStore()
	writer := newStoreEventWriter(kv)
	// 模拟"崩溃前最后已知状态落在 S_EXECUTE"——不安全，不应触碰 AgentPool。
	writer.WriteStateTransEvent("sess-unsafe", fmt.Sprintf("%d", types.AgentStateExecute))

	db := newTestChatMessagesDB(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO chat_messages (session_id, role, content) VALUES (?, ?, ?)`, "sess-unsafe", "user", "do something"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	pool := &fakeAgentPool{ctrl: newFakeAgentController()}
	recorder := harness.NewTrajectoryRecorder(kv)

	recoverOneSession(ctx, db, pool, recorder, "sess-unsafe")

	if pool.acquireCalled {
		t.Error("expected AgentPool.Acquire NOT to be called for a session whose last known state is unsafe (S_EXECUTE)")
	}
}

func TestRecoverOneSession_SkipsWhenNoUserMessage(t *testing.T) {
	ctx := context.Background()
	kv := newFakeKVStore()
	writer := newStoreEventWriter(kv)
	writer.WriteStateTransEvent("sess-no-msg", fmt.Sprintf("%d", types.AgentStatePerceive))

	db := newTestChatMessagesDB(t) // 无任何消息

	pool := &fakeAgentPool{ctrl: newFakeAgentController()}
	recorder := harness.NewTrajectoryRecorder(kv)

	recoverOneSession(ctx, db, pool, recorder, "sess-no-msg")

	if pool.acquireCalled {
		t.Error("expected AgentPool.Acquire NOT to be called when no user message exists to re-drive the session")
	}
}

func TestRecoverOneSession_SafeState_DrivesAgentWithReplayData(t *testing.T) {
	ctx := context.Background()
	kv := newFakeKVStore()
	writer := newStoreEventWriter(kv)
	writer.WriteLLMCallEvent("sess-safe", map[string]any{"messages": "perceive prompt"}, map[string]any{"content": "mock_success"})
	writer.WriteStateTransEvent("sess-safe", fmt.Sprintf("%d", types.AgentStatePerceive))

	db := newTestChatMessagesDB(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO chat_messages (session_id, role, content) VALUES (?, ?, ?)`, "sess-safe", "user", "please continue"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	ctrl := newFakeAgentController()
	pool := &fakeAgentPool{ctrl: ctrl}
	recorder := harness.NewTrajectoryRecorder(kv)

	protocol.SetReplayMode(false) // 确保初始状态干净

	eg, _ := errgroup.WithContext(ctx)
	eg.Go(func() error {
		recoverOneSession(ctx, db, pool, recorder, "sess-safe")
		return nil
	})
	done := make(chan error, 1)
	go func() { done <- eg.Wait() }() //nolint:gocritic // 等待 errgroup 而非业务协程

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("recoverOneSession timed out")
	}

	if !pool.acquireCalled || pool.acquiredSessionID != "sess-safe" {
		t.Errorf("expected AgentPool.Acquire called with session ID %q, got called=%v id=%q", "sess-safe", pool.acquireCalled, pool.acquiredSessionID)
	}
	if len(ctrl.replayCalls) != 1 {
		t.Fatalf("expected 1 replay LLM call injected, got %d", len(ctrl.replayCalls))
	}
	if ctrl.replayCalls[0].Response["content"] != "mock_success" {
		t.Errorf("unexpected replay response: %+v", ctrl.replayCalls[0].Response)
	}
	if string(ctrl.intent) != "please continue" {
		t.Errorf("expected intent set to last user message, got %q", ctrl.intent)
	}
	if !ctrl.sendCalled {
		t.Error("expected SendIntent to have been called")
	}
	if protocol.IsReplaying() {
		t.Error("expected ReplayMode to be reset to false after recoverOneSession returns (defer safety net)")
	}
}
