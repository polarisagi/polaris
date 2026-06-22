package agents

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"

	_ "github.com/mattn/go-sqlite3"
)

func TestMemoryAgent_Distill(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE episodic_memory (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			content TEXT,
			meta TEXT,
			created_at INTEGER,
			distilled_at INTEGER
		)
	`)
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.Exec(`
		INSERT INTO episodic_memory (content, meta, created_at, distilled_at)
		VALUES ('event 1', '{"cold":1}', 100, NULL),
		       ('event 2', '{"cold":1}', 200, NULL),
		       ('event 3', '{"cold":0}', 300, NULL)
	`)
	if err != nil {
		t.Fatal(err)
	}

	ms := &mockSurreal{
		indexed:   make(map[string]string),
		relations: make([]string, 0),
	}

	llm := func(ctx context.Context, prompt string, opts ...types.InferOption) (string, error) {
		return `[{"subject": "sub", "predicate": "pred", "object": "obj"}]`, nil
	}

	// Use wrapper to match signature
	llmWrapper := func(ctx context.Context, prompt string, opts ...types.InferOption) (string, error) {
		return llm(ctx, prompt, opts...)
	}

	ma := NewMemoryAgent(db, ms, LLMInferFunc(llmWrapper), nil, nil, nil)

	err = ma.distill(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ms.relations) != 1 {
		t.Errorf("expected 1 relation, got %d", len(ms.relations))
	}

	// check if distilled_at updated in meta
	var count int
	db.QueryRow("SELECT count(*) FROM episodic_memory WHERE json_extract(meta, '$.distilled_at') IS NOT NULL").Scan(&count)
	if count != 2 {
		t.Errorf("expected 2 distilled items, got %d", count)
	}
}

func TestMemoryAgent_Run_Pressure(t *testing.T) {
	ga, pressure := NewGovernanceAgent(nil, nil)
	_ = ga

	// Distill shouldn't trigger if pressure is high
	pressure.Store(2)

	ma := NewMemoryAgent(nil, nil, nil, nil, nil, pressure)
	ma.distillInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	go ma.Run(ctx)
	time.Sleep(50 * time.Millisecond)
	cancel()
}
