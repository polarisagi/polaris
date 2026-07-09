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
			crash_recovery_count INTEGER NOT NULL DEFAULT 0
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
	w := NewOutboxWorker(db, 0, 0)
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
	w := NewOutboxWorker(db, 5, 3)
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

	w := NewOutboxWorker(db, 5, 3)
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

	w := NewOutboxWorker(db, 5, 3)
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

	w := NewOutboxWorker(db, 5, 3)
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
	w := NewOutboxWorker(db, 5, 3)

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
	w := NewOutboxWorker(db, 5, 3)

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
	w := NewOutboxWorker(db, 5, 3)
	record := &OutboxRecord{ID: 1, TargetEngine: "unknown_engine", CrashRecoveryCount: 0}

	if err := w.Process(context.Background(), record); !errors.Is(err, ErrUnknownTargetEngine) {
		t.Fatalf("expected ErrUnknownTargetEngine, got: %v", err)
	}
}

func TestProcess_VersionCheck(t *testing.T) {
	db := setupOutboxDB(t)
	defer db.Close()
	w := NewOutboxWorker(db, 5, 3)

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
