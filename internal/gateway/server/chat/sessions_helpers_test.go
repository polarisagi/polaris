package chat

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// flakyChatRepo 包装真实 repo.SQLiteChatRepository，AppendMessage 的前
// failCount 次调用恒定失败，之后正常透传给底层实现（GD-13-004 复核修复的
// 重试/outbox 兜底逻辑测试专用）。
type flakyChatRepo struct {
	protocol.ChatRepository
	failCount int
	calls     int
}

func (f *flakyChatRepo) AppendMessage(ctx context.Context, row types.ChatMessageRow) error {
	f.calls++
	if f.calls <= f.failCount {
		return apperr.New(apperr.CodeInternal, "flaky: simulated failure")
	}
	return f.ChatRepository.AppendMessage(ctx, row)
}

// stubOutboxWriter 记录最近一次 Write 调用的 entry，供断言 fallback 是否触发。
type stubOutboxWriter struct {
	entries []protocol.OutboxEntry
	err     error
}

func (s *stubOutboxWriter) Write(_ context.Context, entry protocol.OutboxEntry) error {
	if s.err != nil {
		return s.err
	}
	s.entries = append(s.entries, entry)
	return nil
}

func newTestChatDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS chat_sessions (
			id TEXT PRIMARY KEY, title TEXT, created_at DATETIME, updated_at DATETIME
		);
		CREATE TABLE IF NOT EXISTS chat_messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT, role TEXT, content TEXT,
			reasoning_content TEXT NOT NULL DEFAULT '',
			tool_calls TEXT NOT NULL DEFAULT '',
			file_offset INTEGER NOT NULL DEFAULT 0,
			file_length INTEGER NOT NULL DEFAULT 0,
			dedupe_key TEXT UNIQUE,
			created_at DATETIME, updated_at DATETIME
		);
	`)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// TestSaveMessage_SucceedsWithoutRetry 验证正常路径（无故障）不受重试/outbox
// 兜底逻辑影响，一次写入即完成。
func TestSaveMessage_SucceedsWithoutRetry(t *testing.T) {
	db := newTestChatDB(t)
	h := &ChatHandler{ChatRepo: repo.NewSQLiteChatRepository(db)}
	ctx := context.Background()

	if err := h.SaveMessage(ctx, "sess-1", "user", "hello", "", "", 0); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs, err := h.ListMessages(ctx, "sess-1")
	if err != nil {
		t.Fatalf("ListMessages failed: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Content != "hello" {
		t.Fatalf("unexpected messages: %+v", msgs)
	}
}

// TestSaveMessage_RetriesThenSucceeds 验证 GD-13-004 复核修复的重试逻辑：
// 前两次写入失败，第三次（重试次数上限内）成功，不触发 outbox 兜底。
func TestSaveMessage_RetriesThenSucceeds(t *testing.T) {
	db := newTestChatDB(t)
	flaky := &flakyChatRepo{ChatRepository: repo.NewSQLiteChatRepository(db), failCount: 2}
	outbox := &stubOutboxWriter{}
	h := &ChatHandler{ChatRepo: flaky, OutboxWriter: outbox}
	ctx := context.Background()

	if err := h.SaveMessage(ctx, "sess-1", "assistant", "reply", "", "", 0); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if flaky.calls != saveMessageRetryAttempts {
		t.Errorf("expected exactly %d attempts (2 failed + 1 succeeded), got %d", saveMessageRetryAttempts, flaky.calls)
	}
	if len(outbox.entries) != 0 {
		t.Errorf("expected no outbox fallback when direct retry succeeds, got %d entries", len(outbox.entries))
	}
	msgs, err := h.ListMessages(ctx, "sess-1")
	if err != nil {
		t.Fatalf("ListMessages failed: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Content != "reply" {
		t.Fatalf("unexpected messages: %+v", msgs)
	}
}

// TestSaveMessage_RetriesExhausted_FallsBackToOutbox 验证 GD-13-004 复核修复的
// 核心场景：重试耗尽后不再直接丢弃错误，而是投递到 OutboxWriter 做异步兜底，
// SaveMessage 本身返回 nil（对调用方而言消息"已受理"，具体持久化交给 outbox）。
func TestSaveMessage_RetriesExhausted_FallsBackToOutbox(t *testing.T) {
	db := newTestChatDB(t)
	// failCount 大于重试次数，确保全部尝试都失败。
	flaky := &flakyChatRepo{ChatRepository: repo.NewSQLiteChatRepository(db), failCount: 100}
	outbox := &stubOutboxWriter{}
	h := &ChatHandler{ChatRepo: flaky, OutboxWriter: outbox}
	ctx := context.Background()

	err := h.SaveMessage(ctx, "sess-1", "assistant", "永久失败重试后的回复", "", "", 0)
	if err != nil {
		t.Fatalf("expected nil error when outbox fallback succeeds, got: %v", err)
	}
	if flaky.calls != saveMessageRetryAttempts {
		t.Errorf("expected %d direct attempts before falling back, got %d", saveMessageRetryAttempts, flaky.calls)
	}
	if len(outbox.entries) != 1 {
		t.Fatalf("expected exactly 1 outbox fallback entry, got %d", len(outbox.entries))
	}
	entry := outbox.entries[0]
	if entry.TargetEngine != protocol.TopicChatMessagePersistRetry {
		t.Errorf("unexpected outbox topic: %q", entry.TargetEngine)
	}
	if entry.IdempotencyKey == "" {
		t.Error("expected non-empty idempotency key")
	}

	// 消息此刻应尚未出现在 chat_messages（直写全部失败，仅停留在 outbox）。
	msgs, err := h.ListMessages(ctx, "sess-1")
	if err != nil {
		t.Fatalf("ListMessages failed: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected message not yet persisted directly, got %+v", msgs)
	}
}

// TestSaveMessage_RetriesExhausted_NoOutboxWriter 验证未注入 OutboxWriter 时
// （测试/未接入 outbox 的最小部署）行为与修复前一致：重试耗尽后返回错误。
func TestSaveMessage_RetriesExhausted_NoOutboxWriter(t *testing.T) {
	db := newTestChatDB(t)
	flaky := &flakyChatRepo{ChatRepository: repo.NewSQLiteChatRepository(db), failCount: 100}
	h := &ChatHandler{ChatRepo: flaky} // OutboxWriter 未注入

	if err := h.SaveMessage(context.Background(), "sess-1", "user", "hi", "", "", 0); err == nil {
		t.Fatal("expected error when both direct write and outbox fallback are unavailable")
	}
}

// TestChatMessagePersistHandler_Handle 验证 outbox 消费端能正确解析 payload 并
// 幂等写入；重复调用（模拟 OutboxWorker at-least-once 重投）不产生重复行。
func TestChatMessagePersistHandler_Handle(t *testing.T) {
	db := newTestChatDB(t)
	chatRepo := repo.NewSQLiteChatRepository(db)
	handler := NewChatMessagePersistHandler(chatRepo)

	// 先创建 outbox fallback 场景以拿到真实的 payload 字节。
	flaky := &flakyChatRepo{ChatRepository: chatRepo, failCount: 100}
	outbox := &stubOutboxWriter{}
	h := &ChatHandler{ChatRepo: flaky, OutboxWriter: outbox}
	if err := h.SaveMessage(context.Background(), "sess-1", "assistant", "outbox兜底内容", "", "", 0); err != nil {
		t.Fatalf("SaveMessage failed: %v", err)
	}
	if len(outbox.entries) != 1 {
		t.Fatalf("expected 1 outbox entry, got %d", len(outbox.entries))
	}

	record := &store.OutboxRecord{Payload: outbox.entries[0].Payload}
	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatalf("Handle failed: %v", err)
	}
	// 模拟 OutboxWorker at-least-once 重投：再次调用不应报错也不应重复插入。
	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatalf("Handle (retry) failed: %v", err)
	}

	msgs, err := chatRepo.ListMessages(context.Background(), "sess-1", 0)
	if err != nil {
		t.Fatalf("ListMessages failed: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected exactly 1 message after idempotent retry, got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].Content != "outbox兜底内容" {
		t.Errorf("unexpected content: %q", msgs[0].Content)
	}
}
