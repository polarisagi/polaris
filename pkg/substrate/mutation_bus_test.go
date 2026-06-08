package substrate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	_ "modernc.org/sqlite"
)

type mockLeaseChecker struct{}

func (m *mockLeaseChecker) Verify(taskID, agentID string, version int64) bool { return true }

func setupTestDB(t *testing.T) (*sql.DB, func()) {
	dir, err := os.MkdirTemp("", "polaris_test_db")
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "test.db")
	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)

	_, err = db.Exec(`CREATE TABLE test_entities (
		id TEXT PRIMARY KEY,
		payload TEXT,
		version INTEGER,
		updated_at TEXT
	)`)
	if err != nil {
		t.Fatal(err)
	}

	return db, func() {
		db.Close()
		os.RemoveAll(dir)
	}
}

func TestMutationBus_ConcurrentWrites(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	lc := &mockLeaseChecker{}
	dw := NewDatabaseWriter(db, lc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go dw.Run(ctx)

	var wg sync.WaitGroup
	workers := 100
	opsPerWorker := 10

	errCh := make(chan error, workers*opsPerWorker)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < opsPerWorker; j++ {
				resCh := make(chan error, 1)
				// Each worker writes to its own distinct key to simulate 1000 distinct writes without version conflicts
				key := fmt.Sprintf("entity_%d_%d", workerID, j)
				intent := &MutationIntent{
					Table:          "test_entities",
					Operation:      "insert",
					Key:            []byte(key),
					Payload:        []byte(fmt.Sprintf("data_%d_%d", workerID, j)),
					ClaimedVersion: 0,
					ResultCh:       resCh,
				}
				err := dw.Submit(ctx, intent)
				if err != nil {
					errCh <- err
					continue
				}
				err = <-resCh
				if err != nil {
					errCh <- err
				}
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			t.Errorf("Unexpected error during concurrent write: %v", err)
		}
	}

	// Verify all 1000 records are written correctly
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM test_entities").Scan(&count)
	if err != nil {
		t.Fatalf("Failed to query count: %v", err)
	}
	if count != 1000 {
		t.Errorf("Expected 1000 records, got %d", count)
	}
}

// TestMutationBus_OptimisticLocking tests concurrent updates to the SAME entity with optimistic locking.
func TestMutationBus_OptimisticLocking(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	lc := &mockLeaseChecker{}
	dw := NewDatabaseWriter(db, lc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go dw.Run(ctx)

	// Pre-insert an entity
	_, err := db.Exec(`INSERT INTO test_entities (id, payload, version, updated_at) VALUES (?, ?, ?, ?)`, "entity1", "init", 0, "now")
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	workers := 10

	type result struct {
		workerID int
		err      error
	}
	resultCh := make(chan result, workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			execCh := make(chan error, 1)
			intent := &MutationIntent{
				Table:          "test_entities",
				Operation:      "upsert",
				Key:            []byte("entity1"),
				Payload:        []byte(fmt.Sprintf("w%d", workerID)),
				ClaimedVersion: 1, // 所有 worker 尝试从 version 0 升级到 1
				ResultCh:       execCh,
			}
			if err := dw.Submit(ctx, intent); err != nil {
				resultCh <- result{workerID, err}
				return
			}
			resultCh <- result{workerID, <-execCh}
		}(i)
	}

	wg.Wait()
	close(resultCh)

	successCount := 0
	staleLeaseCount := 0
	for r := range resultCh {
		if r.err == nil {
			successCount++
		} else if r.err == ErrStaleLease {
			staleLeaseCount++
		} else {
			t.Errorf("Unexpected error from worker %d: %v", r.workerID, r.err)
		}
	}

	if successCount != 1 {
		t.Errorf("Expected exactly 1 success with optimistic lock, got %d", successCount)
	}
	if staleLeaseCount != workers-1 {
		t.Errorf("Expected %d stale lease errors, got %d", workers-1, staleLeaseCount)
	}
}
