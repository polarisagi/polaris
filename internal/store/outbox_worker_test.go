package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

func setupOutboxDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// :memory: 每条连接都是独立空库（无 cache=shared），池开出第二条即读到空表。
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`
		CREATE TABLE outbox (
			id                   INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at           INTEGER NOT NULL,
			target_engine        TEXT NOT NULL,
			operation            TEXT NOT NULL,
			scope                TEXT NOT NULL,
			payload              BLOB NOT NULL,
			idempotency_key      TEXT NOT NULL UNIQUE,
			status               TEXT NOT NULL DEFAULT 'pending',
			attempts             INTEGER NOT NULL DEFAULT 0,
			last_error           TEXT,
			next_retry_at        INTEGER,
			crash_recovery_count INTEGER NOT NULL DEFAULT 0,
			updated_at           INTEGER,
			processed_at         INTEGER
		)`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	return db
}

func insertOutboxRow(t *testing.T, db *sql.DB, id int64, engine, status string, nextRetryAt *int64) {
	t.Helper()
	nr := sql.NullInt64{}
	if nextRetryAt != nil {
		nr = sql.NullInt64{Int64: *nextRetryAt, Valid: true}
	}
	_, err := db.Exec(`
		INSERT INTO outbox (id, created_at, target_engine, operation, scope, payload, idempotency_key, status, next_retry_at)
		VALUES (?, ?, 'surrealdb', 'upsert', 'memory', X'CAFE', ?, ?, ?)`,
		id, time.Now().UnixMilli(), types.BuildIdempotencyKey("sqlite", "event", "e"+string(rune('0'+id)), "create", int(id)), status, nr,
	)
	if err != nil {
		t.Fatalf("insert outbox row: %v", err)
	}
}

func TestNewOutboxWorker_Defaults(t *testing.T) {
	db := setupOutboxDB(t)
	defer db.Close()
	w := NewOutboxWorker(db, 0, 0, 0, 0)
	if w.pollInterval != 5 {
		t.Errorf("expected default pollInterval=5, got %d", w.pollInterval)
	}
	if w.maxRetries != 3 {
		t.Errorf("expected default maxRetries=3, got %d", w.maxRetries)
	}
}

func TestListBatch_NilDB(t *testing.T) {
	w := &OutboxWorker{handlers: make(map[string]OutboxHandler)}
	_, err := w.ListBatch(context.Background(), 0, 10)
	if err == nil {
		t.Fatal("expected error for nil db")
	}
	var pe *apperr.Error
	if e, ok := err.(*apperr.Error); ok {
		pe = e
	}
	if pe == nil || pe.Code != apperr.CodeInternal {
		t.Errorf("expected CodeInternal, got: %v", err)
	}
}

func TestListBatch_Empty(t *testing.T) {
	db := setupOutboxDB(t)
	defer db.Close()
	w := NewOutboxWorker(db, 5, 3, 100, 8000)
	records, err := w.ListBatch(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("expected 0 records, got %d", len(records))
	}
}

func TestListBatch_ReturnsPendingRecords(t *testing.T) {
	db := setupOutboxDB(t)
	defer db.Close()
	insertOutboxRow(t, db, 1, "surrealdb", "pending", nil)
	insertOutboxRow(t, db, 2, "surrealdb", "pending", nil)
	insertOutboxRow(t, db, 3, "surrealdb", "done", nil)

	w := NewOutboxWorker(db, 5, 3, 100, 8000)
	records, err := w.ListBatch(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(records) != 2 {
		t.Errorf("expected 2 pending records, got %d", len(records))
	}
	for _, r := range records {
		if r.TargetEngine != "surrealdb" {
			t.Errorf("unexpected engine: %s", r.TargetEngine)
		}
		if r.IdempotencyKey == "" {
			t.Error("idempotency key should be set")
		}
	}
}

func TestListBatch_CursorFiltering(t *testing.T) {
	db := setupOutboxDB(t)
	defer db.Close()
	insertOutboxRow(t, db, 1, "surrealdb", "pending", nil)
	insertOutboxRow(t, db, 2, "surrealdb", "pending", nil)
	insertOutboxRow(t, db, 3, "surrealdb", "pending", nil)

	w := NewOutboxWorker(db, 5, 3, 100, 8000)
	// cursor=2 → only id=3 returned from main query
	records, err := w.ListBatch(context.Background(), 2, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(records) != 1 || records[0].ID != 3 {
		t.Errorf("expected only record id=3, got %d records", len(records))
	}
}

func TestListBatch_SkipsFutureRetry(t *testing.T) {
	db := setupOutboxDB(t)
	defer db.Close()
	future := time.Now().Add(time.Hour).UnixMilli()
	insertOutboxRow(t, db, 1, "surrealdb", "failed", &future)
	past := time.Now().Add(-time.Hour).UnixMilli()
	insertOutboxRow(t, db, 2, "surrealdb", "failed", &past)

	w := NewOutboxWorker(db, 5, 3, 100, 8000)
	records, err := w.ListBatch(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Only record 2 (past retry time) should be returned
	if len(records) != 1 || records[0].ID != 2 {
		t.Errorf("expected 1 record with past retry time, got %d", len(records))
	}
}

func TestRegisterHandler_And_Process(t *testing.T) {
	db := setupOutboxDB(t)
	defer db.Close()
	w := NewOutboxWorker(db, 5, 3, 100, 8000)

	called := false
	w.RegisterHandler("surrealdb", func(ctx context.Context, r *OutboxRecord) error {
		called = true
		return nil
	})

	record := &OutboxRecord{ID: 1, TargetEngine: "surrealdb", CrashRecoveryCount: 0}
	if err := w.Process(context.Background(), record); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("expected handler to be called")
	}
}

func TestProcess_PoisonPill_CrashRecoveryCount(t *testing.T) {
	db := setupOutboxDB(t)
	defer db.Close()
	w := NewOutboxWorker(db, 5, 3, 100, 8000)

	handlerCalled := false
	w.RegisterHandler("surrealdb", func(ctx context.Context, r *OutboxRecord) error {
		handlerCalled = true
		return nil
	})

	// crash_recovery_count >= 3 → 直接跳过，标记 dead
	record := &OutboxRecord{ID: 1, TargetEngine: "surrealdb", CrashRecoveryCount: 3}
	if err := w.Process(context.Background(), record); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if handlerCalled {
		t.Error("handler should NOT be called for poison pill")
	}
}

func TestProcess_NoHandler(t *testing.T) {
	db := setupOutboxDB(t)
	defer db.Close()
	w := NewOutboxWorker(db, 5, 3, 100, 8000)
	record := &OutboxRecord{ID: 1, TargetEngine: "unknown_engine", CrashRecoveryCount: 0}

	if err := w.Process(context.Background(), record); !errors.Is(err, ErrUnknownTargetEngine) {
		t.Fatalf("expected ErrUnknownTargetEngine, got: %v", err)
	}
}

func TestProcess_VersionCheck(t *testing.T) {
	db := setupOutboxDB(t)
	defer db.Close()
	w := NewOutboxWorker(db, 5, 3, 100, 8000)

	handlerCalled := false
	handler := func(ctx context.Context, r *OutboxRecord) error {
		handlerCalled = true
		return nil
	}

	checker := func(ctx context.Context, r *OutboxRecord) (int64, error) {
		// pretend existing version is 5
		return 5, nil
	}
	w.RegisterHandler("surrealdb", handler, checker)

	// Old version: 4 <= 5 -> ErrVersionStale
	recordOld := &OutboxRecord{ID: 1, TargetEngine: "surrealdb", Version: 4, CrashRecoveryCount: 0}
	err := w.Process(context.Background(), recordOld)
	if !errors.Is(err, ErrVersionStale) {
		t.Errorf("expected ErrVersionStale, got: %v", err)
	}
	if handlerCalled {
		t.Error("handler should not be called for old version")
	}

	// Same version: 5 <= 5 -> ErrVersionStale
	recordSame := &OutboxRecord{ID: 2, TargetEngine: "surrealdb", Version: 5, CrashRecoveryCount: 0}
	err = w.Process(context.Background(), recordSame)
	if !errors.Is(err, ErrVersionStale) {
		t.Errorf("expected ErrVersionStale for same version, got: %v", err)
	}

	// New version: 6 > 5 -> success
	handlerCalled = false
	recordNew := &OutboxRecord{ID: 3, TargetEngine: "surrealdb", Version: 6, CrashRecoveryCount: 0}
	err = w.Process(context.Background(), recordNew)
	if err != nil {
		t.Errorf("expected success for new version, got: %v", err)
	}
	if !handlerCalled {
		t.Error("handler should be called for new version")
	}
}

func TestOutboxWorker_BackoffSequence(t *testing.T) {
	db := setupOutboxDB(t)
	defer db.Close()

	w := NewOutboxWorker(db, 5, 5, 100, 500)
	w.RegisterHandler("test", func(ctx context.Context, rec *OutboxRecord) error {
		return apperr.New(apperr.CodeInternal, "simulated failure")
	})

	now := time.Now().UnixMilli()
	_, err := db.Exec(`INSERT INTO outbox (id, created_at, target_engine, operation, scope, payload, idempotency_key, status, attempts, next_retry_at) 
		VALUES (1001, ?, 'test', 'fail', 'system', X'CAFE', 'key', 'pending', 0, NULL)`, now)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	records, err := w.ListBatch(ctx, 0, 10)
	if err != nil {
		t.Fatalf("ListBatch 1: %v", err)
	}
	t.Logf("ListBatch returned %d records. ID=%v, Engine=%v, Operation=%v", len(records), records[0].ID, records[0].TargetEngine, records[0].Operation)

	_, err = w.processBatch(ctx, 0, 10)
	if err != nil {
		t.Fatalf("processBatch 1: %v", err)
	}

	var attempts int
	var nextRetry sql.NullInt64
	var status string
	err = db.QueryRow(`SELECT attempts, next_retry_at, status FROM outbox WHERE id=1001`).Scan(&attempts, &nextRetry, &status)
	if err != nil {
		t.Fatalf("query 1: %v", err)
	}
	t.Logf("State after processBatch 1: attempts=%d, next_retry_at=%v, status=%s", attempts, nextRetry, status)

	if attempts != 1 {
		t.Errorf("expected 1 attempt, got %d (status=%s)", attempts, status)
	}
	expectedBackoff := int64(100) << 1 // 200ms

	if nextRetry.Valid {
		if nextRetry.Int64 < now+expectedBackoff-50 || nextRetry.Int64 > now+expectedBackoff+200 {
			t.Errorf("expected nextRetry around %d (backoff %d), got %d", now+expectedBackoff, expectedBackoff, nextRetry.Int64)
		}
	} else {
		t.Errorf("nextRetry is NULL")
	}
}

// TestProcessAndMark_MarkDoneFailure_ReturnsError_S02 验证阶段02修复：Process()
// 成功但落盘 status='done' 失败时，processAndMark 必须向上返回错误。
// 回归锚点：修复前 `_, _ = w.db.ExecContext(...)` 吞没该错误，记录会永久卡在
// 'processing'（ListBatch 只捡 pending/failed，永远不会被重新消费或重试）。
func TestProcessAndMark_MarkDoneFailure_ReturnsError_S02(t *testing.T) {
	db := setupOutboxDB(t)
	defer db.Close()
	// 仅在 UPDATE ... SET status='done' 时触发失败，'processing' 阶段的更新不受影响。
	if _, err := db.Exec(`
		CREATE TRIGGER reject_done BEFORE UPDATE OF status ON outbox
		WHEN NEW.status = 'done'
		BEGIN SELECT RAISE(ABORT, 'simulated done write failure'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	w := NewOutboxWorker(db, 5, 3, 100, 8000)
	w.RegisterHandler("test", func(ctx context.Context, r *OutboxRecord) error {
		return nil // Process 本身成功
	})

	now := time.Now().UnixMilli()
	if _, err := db.Exec(`INSERT INTO outbox (id, created_at, target_engine, operation, scope, payload, idempotency_key, status)
		VALUES (3001, ?, 'test', 'ok', 'system', X'CAFE', 'key3001', 'pending')`, now); err != nil {
		t.Fatalf("insert: %v", err)
	}

	record := &OutboxRecord{ID: 3001, TargetEngine: "test"}
	err := w.processAndMark(context.Background(), record)
	if err == nil {
		t.Fatal("expected error when marking done fails, got nil")
	}

	var status string
	if err := db.QueryRow(`SELECT status FROM outbox WHERE id=3001`).Scan(&status); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if status != "processing" {
		t.Errorf("expected status to remain 'processing' (not silently lost), got %q", status)
	}
}

// TestProcessBatch_SingleRecordFailure_ContinuesToNextRecord_S02 验证阶段02修复：
// 单条记录处理失败（L2）不应中断整批处理，其余记录仍需推进 cursor。
func TestProcessBatch_SingleRecordFailure_ContinuesToNextRecord_S02(t *testing.T) {
	db := setupOutboxDB(t)
	defer db.Close()

	w := NewOutboxWorker(db, 5, 3, 100, 8000)
	w.RegisterHandler("test", func(ctx context.Context, r *OutboxRecord) error {
		if r.ID == 4001 {
			return apperr.New(apperr.CodeInternal, "simulated failure")
		}
		return nil
	})

	now := time.Now().UnixMilli()
	for _, id := range []int64{4001, 4002} {
		if _, err := db.Exec(`INSERT INTO outbox (id, created_at, target_engine, operation, scope, payload, idempotency_key, status)
			VALUES (?, ?, 'test', 'ok', 'system', X'CAFE', ?, 'pending')`, id, now, "key"+string(rune('0'+id))); err != nil {
			t.Fatalf("insert %d: %v", id, err)
		}
	}

	maxID, err := w.processBatch(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("processBatch should not fail when a single record errors: %v", err)
	}
	if maxID != 4002 {
		t.Errorf("expected maxID=4002 (both records advance cursor regardless of per-record outcome), got %d", maxID)
	}

	var status2 string
	if err := db.QueryRow(`SELECT status FROM outbox WHERE id=4002`).Scan(&status2); err != nil {
		t.Fatalf("query status 4002: %v", err)
	}
	if status2 != "done" {
		t.Errorf("expected record 4002 to complete despite 4001 failing, got status=%q", status2)
	}
}

func TestProcessAndMark_DBError(t *testing.T) {
	db := setupOutboxDB(t)
	defer db.Close()
	w := NewOutboxWorker(db, 5, 3, 100, 8000)
	w.RegisterHandler("test", func(ctx context.Context, r *OutboxRecord) error {
		return ErrUnknownTargetEngine
	})

	now := time.Now().UnixMilli()
	_, err := db.Exec(`INSERT INTO outbox (id, created_at, target_engine, operation, scope, payload, idempotency_key, status, attempts, next_retry_at) 
		VALUES (2001, ?, 'test', 'fail', 'system', X'CAFE', 'key2001', 'pending', 0, NULL)`, now)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	_, _ = db.Exec("DROP TABLE outbox")

	record := &OutboxRecord{ID: 2001, TargetEngine: "test"}
	err = w.processAndMark(context.Background(), record)
	if err == nil {
		t.Errorf("expected DB error, got nil")
	}
}

// TestLoadCursorSafe_NoRows_ReturnsZeroOK_S02 验证首次启动（sys_config 尚无
// outbox_cursor 记录）是合法场景：cursor=0 且 ok=true，Run() 应正常从 0 开始消费。
func TestLoadCursorSafe_NoRows_ReturnsZeroOK_S02(t *testing.T) {
	db := setupOutboxDB(t)
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE sys_config (key TEXT PRIMARY KEY, value TEXT)`); err != nil {
		t.Fatalf("create sys_config: %v", err)
	}

	w := NewOutboxWorker(db, 5, 3, 100, 8000)
	cursor, ok := w.loadCursorSafe(context.Background())
	if !ok {
		t.Fatal("expected ok=true for legitimate first-run ErrNoRows")
	}
	if cursor != 0 {
		t.Errorf("expected cursor=0, got %d", cursor)
	}
}

// TestLoadCursorSafe_QueryFailure_ReturnsNotOK_S02 验证阶段02修复：游标查询真实
// 失败（如 sys_config 表缺失/损坏）时必须返回 ok=false，不得静默退回 cursor=0。
// 回归锚点：修复前 `_ = row.Scan(&cursor)` 吞没错误，函数总是返回 0，Run() 会
// 误以为"从未消费过"而重放全部 outbox。
func TestLoadCursorSafe_QueryFailure_ReturnsNotOK_S02(t *testing.T) {
	db := setupOutboxDB(t)
	defer db.Close()
	// 故意不创建 sys_config 表，令查询本身失败（而非 ErrNoRows）。

	w := NewOutboxWorker(db, 5, 3, 100, 8000)
	cursor, ok := w.loadCursorSafe(context.Background())
	if ok {
		t.Fatal("expected ok=false when the underlying query fails (not ErrNoRows)")
	}
	if cursor != 0 {
		t.Errorf("expected cursor=0 as safe zero-value alongside ok=false, got %d", cursor)
	}
}
