package graphrag

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/store"
)

type mockLeaseChecker struct{}

func (m *mockLeaseChecker) Verify(taskID, agentID string, version int64) bool { return true }

type mockEntityFetcher struct {
	entities map[string]*Entity
}

func (m *mockEntityFetcher) GetEntityByName(ctx context.Context, name string) (*Entity, error) {
	e, ok := m.entities[name]
	if !ok {
		return nil, sql.ErrNoRows
	}
	return e, nil
}

func setupGraphTestDB(t *testing.T) (*sql.DB, func()) {
	dir, err := os.MkdirTemp("", "polaris_graph_db")
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "graph.db")
	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)

	// MutationBus 乐观锁用 WHERE id = ? AND version = ?
	_, err = db.Exec(`CREATE TABLE entities (
		id TEXT PRIMARY KEY,
		name TEXT,
		type TEXT,
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

// TestGraphWriter_ConcurrentSingleWriter 验证 P1-3: GraphWriter 单写者串行化。
// 50 worker 并发写同实体，乐观锁保证无覆盖写，version 高者保留。
func TestGraphWriter_ConcurrentSingleWriter(t *testing.T) {
	db, cleanup := setupGraphTestDB(t)
	defer cleanup()

	lc := &mockLeaseChecker{}
	dw := store.NewDatabaseWriter(db, lc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go dw.Run(ctx)

	// 预插入初始实体 id="entity1", version=1
	_, err := db.Exec(`INSERT INTO entities (id, name, type, payload, version, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"entity1", "Apple", "Company", "init_payload", 1, "now")
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	workers := 50

	type result struct {
		workerID int
		err      error
	}
	resultCh := make(chan result, workers)

	// 所有 worker 尝试将 version 从 1 升级到 2
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			execCh := make(chan error, 1)
			intent := &store.MutationIntent{
				Table:          "entities",
				Operation:      "upsert",
				Key:            []byte("entity1"),
				Payload:        []byte("updated_payload"),
				ClaimedVersion: 2, // expected oldVersion = 1
				ResultCh:       execCh,
			}

			if submitErr := dw.Submit(ctx, intent); submitErr != nil {
				resultCh <- result{workerID, submitErr}
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
		} else if r.err == store.ErrStaleLease {
			staleLeaseCount++
		} else {
			t.Errorf("worker %d: unexpected error: %v", r.workerID, r.err)
		}
	}

	if successCount != 1 {
		t.Errorf("expected exactly 1 success (optimistic lock), got %d", successCount)
	}
	if staleLeaseCount != workers-1 {
		t.Errorf("expected %d stale lease, got %d", workers-1, staleLeaseCount)
	}

	// 验证最终 version=2
	var version int
	err = db.QueryRow("SELECT version FROM entities WHERE id = 'entity1'").Scan(&version)
	if err != nil {
		t.Fatalf("query version failed: %v", err)
	}
	if version != 2 {
		t.Errorf("expected version=2, got %d", version)
	}
}

func TestGraphWriter_Disambiguation(t *testing.T) {
	fetcher := &mockEntityFetcher{
		entities: map[string]*Entity{
			"Apple": {
				ID:          "entity_apple",
				Name:        "Apple",
				Embedding:   []float32{1.0, 0.0, 0.0},
				SyncVersion: 5,
			},
		},
	}

	db, cleanup := setupGraphTestDB(t)
	defer cleanup()
	lc := &mockLeaseChecker{}
	dw := store.NewDatabaseWriter(db, lc)

	gw := NewGraphWriter(dw, fetcher)

	ctx := context.Background()

	// Test 1: High similarity but lower version -> should be disambiguated and not submit to DB
	err := gw.UpsertEntity(ctx, &Entity{
		ID:          "entity_apple_new",
		Name:        "Apple",
		Embedding:   []float32{0.99, 0.1, 0.0}, // Very high similarity
		SyncVersion: 3,                         // Lower version
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Test 2: Low similarity -> should still be submitted despite same name
	err = gw.UpsertEntity(ctx, &Entity{
		ID:          "entity_apple_fruit",
		Name:        "Apple",
		Embedding:   []float32{0.0, 1.0, 0.0}, // Orthogonal vector
		SyncVersion: 1,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCosineSimilarity(t *testing.T) {
	a := []float32{1.0, 0.0, 0.0}
	b := []float32{1.0, 0.0, 0.0}
	if CosineSimilarity(a, b) < 0.99 {
		t.Errorf("expected highly similar")
	}

	c := []float32{0.0, 1.0, 0.0}
	if CosineSimilarity(a, c) > 0.01 {
		t.Errorf("expected low similarity")
	}
}
