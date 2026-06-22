package reflexion

import (
	"github.com/polarisagi/polaris/internal/learning"
	"github.com/polarisagi/polaris/pkg/types"

	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/prompt/optimizer"

	_ "modernc.org/sqlite"
)

// MockSurrealWriter for testing
type MockSurrealWriter struct {
	mu               sync.Mutex
	FTSIndexCalls    int
	GraphRelateCalls int
}

func (m *MockSurrealWriter) FTSIndex(docID, text string) error {
	m.mu.Lock()
	m.FTSIndexCalls++
	m.mu.Unlock()
	return nil
}

func (m *MockSurrealWriter) GraphRelate(fromID, edgeType, toID string, weight float64) error {
	m.mu.Lock()
	m.GraphRelateCalls++
	m.mu.Unlock()
	return nil
}

func (m *MockSurrealWriter) GetFTSIndexCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.FTSIndexCalls
}

func (m *MockSurrealWriter) GetGraphRelateCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.GraphRelateCalls
}

func TestReflexionEngine(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("Failed to open sqlite memory DB: %v", err)
	}
	defer db.Close()

	// Setup heuristics_memory table
	_, err = db.Exec(`
		CREATE TABLE heuristics_memory (
			id TEXT PRIMARY KEY,
			content TEXT,
			task_type TEXT,
			success_rate REAL,
			use_count INTEGER,
			keywords_json TEXT,
			created_at INTEGER
		)
	`)
	if err != nil {
		t.Fatalf("Failed to create heuristics_memory table: %v", err)
	}

	// Setup memf table (fallacy_records)
	_, err = db.Exec(`
		CREATE TABLE fallacy_records (
			id TEXT PRIMARY KEY,
			task_type TEXT,
			failure_type TEXT,
			keywords TEXT,
			reflection TEXT,
			occurrence_count INTEGER,
			node_quality_score REAL,
			created_at INTEGER
		)
	`)
	if err != nil {
		t.Fatalf("Failed to create fallacy_records table: %v", err)
	}

	// Setup reflection_memory table
	_, err = db.Exec(`
		CREATE TABLE reflection_memory (
			task_id TEXT PRIMARY KEY,
			reflection_type TEXT,
			content TEXT,
			created_at INTEGER
		)
	`)
	if err != nil {
		t.Fatalf("Failed to create reflection_memory table: %v", err)
	}

	memf := optimizer.NewFallacyMemoryPool(db)
	heuristics := &optimizer.HeuristicsMemory{DB: db}

	llmInferFunc := func(ctx context.Context, prompt string) (string, error) {
		return "Mocked reason from LLM.", nil
	}

	engine := NewReflexionEngine(memf, heuristics, llmInferFunc)

	mockSurreal := &MockSurrealWriter{}
	engine.InjectDependencies(db, mockSurreal)

	heuristicCh := make(chan types.HeuristicGeneratedPayload, 10)
	engine.SetHeuristicChannel(heuristicCh)

	ctx := context.Background()

	t.Run("Reflect_Success_Replan", func(t *testing.T) {
		result := &learning.TaskResult{Success: true}
		traj := []learning.Step{
			{Index: 1, Action: "act1", Result: "res1", Success: false},
			{Index: 2, Action: "act2", Result: "res2", Success: true},
		}
		// Uses custom LLM infer for replaySuccess JSON
		engine.llmInfer = func(ctx context.Context, prompt string) (string, error) {
			return `{"insight": "Try act2 instead of act1", "tags": ["test"]}`, nil
		}

		ref, err := engine.Reflect(ctx, "task1", "type1", result, traj, 1)
		if err != nil {
			t.Errorf("Reflect failed: %v", err)
		}
		if ref == nil {
			t.Fatalf("Expected non-nil reflection")
		}
		if ref.Cause != "success_after_replan" {
			t.Errorf("Expected cause success_after_replan, got %s", ref.Cause)
		}

		// Wait for goroutine
		time.Sleep(100 * time.Millisecond)

		if mockSurreal.GetFTSIndexCalls() != 1 {
			t.Errorf("Expected 1 FTSIndex call, got %d", mockSurreal.GetFTSIndexCalls())
		}
		if mockSurreal.GetGraphRelateCalls() != 1 {
			t.Errorf("Expected 1 GraphRelate call, got %d", mockSurreal.GetGraphRelateCalls())
		}
	})

	t.Run("Reflect_Uncontrollable_Failure", func(t *testing.T) {
		result := &learning.TaskResult{Success: false, FailureClass: "uncontrollable"}
		traj := []learning.Step{
			{Index: 1, Action: "act1", Result: "timeout", Success: false},
		}

		engine.llmInfer = llmInferFunc

		ref, err := engine.Reflect(ctx, "task2", "type1", result, traj, 0)
		if err != nil {
			t.Errorf("Reflect failed: %v", err)
		}
		if ref == nil {
			t.Fatalf("Expected non-nil reflection")
		}
		if ref.MEMFRecordID != "" {
			t.Errorf("Expected empty MEMFRecordID for uncontrollable failure, got %s", ref.MEMFRecordID)
		}

		// Check heuristic channel
		select {
		case msg := <-heuristicCh:
			if msg.TaskID != "task2" {
				t.Errorf("Expected task2 in heuristic, got %s", msg.TaskID)
			}
		case <-time.After(100 * time.Millisecond):
			t.Errorf("Expected message on heuristic channel")
		}
	})

	t.Run("Reflect_Controllable_Failure", func(t *testing.T) {
		result := &learning.TaskResult{Success: false, FailureClass: "logic_error"}
		traj := []learning.Step{
			{Index: 1, Action: "act1", Result: "logic error", Success: false},
		}

		ref, err := engine.Reflect(ctx, "task3", "type1", result, traj, 0)
		if err != nil {
			t.Errorf("Reflect failed: %v", err)
		}
		if ref == nil {
			t.Fatalf("Expected non-nil reflection")
		}
		if ref.MEMFRecordID == "" {
			t.Errorf("Expected non-empty MEMFRecordID for controllable failure")
		}

		// Check heuristics memory DB
		var count int
		db.QueryRow("SELECT COUNT(*) FROM heuristics_memory").Scan(&count)
		if count == 0 {
			t.Errorf("Expected heuristics_memory to have records")
		}
	})

	t.Run("Reflect_FallbackRules", func(t *testing.T) {
		// Nil llmInfer will fallback to rule-based cause inference
		engineNoLLM := NewReflexionEngine(nil, nil, nil)

		result := &learning.TaskResult{Success: false, FailureClass: "logic_error"}
		traj := []learning.Step{
			{Index: 1, Action: "bad_action", Result: "bad_res", Success: false},
		}
		ref, err := engineNoLLM.Reflect(ctx, "task4", "type1", result, traj, 0)

		if err != nil {
			t.Errorf("Reflect failed: %v", err)
		}
		if ref.Cause == "Mocked reason from LLM." {
			t.Errorf("Expected rule-based fallback cause, got LLM output")
		}
	})
}

func TestHeuristicsMemory(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("Failed to open sqlite memory DB: %v", err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE heuristics_memory (
			id TEXT PRIMARY KEY,
			content TEXT,
			task_type TEXT,
			success_rate REAL,
			use_count INTEGER,
			keywords_json TEXT,
			created_at INTEGER
		)
	`)
	if err != nil {
		t.Fatalf("Failed to create heuristics_memory table: %v", err)
	}

	hm := &optimizer.HeuristicsMemory{DB: db}
	ctx := context.Background()

	// Test Add
	h := &optimizer.Heuristic{
		ID:          "h1",
		Content:     "content",
		TaskType:    "t1",
		SuccessRate: 0.5,
		UseCount:    0,
		Keywords:    []string{"k1"},
	}
	err = hm.Add(ctx, h)
	if err != nil {
		t.Errorf("Add failed: %v", err)
	}

	// Test Add Upsert
	h.SuccessRate = 1.0
	h.UseCount = 1
	err = hm.Add(ctx, h)
	if err != nil {
		t.Errorf("Add upsert failed: %v", err)
	}

	var rate float64
	db.QueryRow("SELECT success_rate FROM heuristics_memory WHERE id='h1'").Scan(&rate)
	if rate != 1.0 { // (0.5*0 + 1.0) / 1 = 1.0
		t.Errorf("Expected rate 1.0, got %f", rate)
	}

	// Test UpdateSuccessRate
	err = hm.UpdateSuccessRate(ctx, "h1", true)
	if err != nil {
		t.Errorf("UpdateSuccessRate failed: %v", err)
	}
	db.QueryRow("SELECT success_rate FROM heuristics_memory WHERE id='h1'").Scan(&rate)
	// 0.9 * 1.0 + 0.1 * 1.0 = 1.0
	if rate != 1.0 {
		t.Errorf("Expected rate 1.0, got %f", rate)
	}

	err = hm.UpdateSuccessRate(ctx, "h1", false)
	if err != nil {
		t.Errorf("UpdateSuccessRate failed: %v", err)
	}
	db.QueryRow("SELECT success_rate FROM heuristics_memory WHERE id='h1'").Scan(&rate)
	// 0.9 * 1.0 + 0.1 * 0.0 = 0.9
	if rate != 0.9 {
		t.Errorf("Expected rate 0.9, got %f", rate)
	}
}
