package memory_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/memory"
	"github.com/polarisagi/polaris/internal/memory/consolidation"
	"github.com/polarisagi/polaris/internal/memory/retrieval"
	"github.com/polarisagi/polaris/internal/memory/testutil"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

func setupTestMem(t *testing.T) (*memory.MemImpl, *sql.DB, protocol.Store) {
	schemaFS := os.DirFS("../../internal/protocol/schema").(fs.ReadDirFS)
	st, err := store.OpenSQLite(":memory:", schemaFS)
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	db := st.DB()
	mem := memory.NewMemImplWithDB(st, db)
	mem.InjectEmbedder(&dummyEmbedder{})
	return mem, db, st
}

type dummyEmbedder struct{}

func (e *dummyEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	return make([]float32, 1536), nil
}

func (e *dummyEmbedder) ModelVersion() string {
	return "dummy-v1"
}

type dummySurreal struct {
}

func (d *dummySurreal) FTSIndex(docID, text string) error { return nil }
func (d *dummySurreal) FTSSearch(query string, limit int) ([]types.CognitiveSearchResult, error) {
	return nil, nil
}
func (d *dummySurreal) FTSDelete(docID string) error                   { return nil }
func (d *dummySurreal) VecUpsert(docID string, vector []float32) error { return nil }
func (d *dummySurreal) VecKNN(vector []float32, limit int) ([]types.CognitiveSearchResult, error) {
	return nil, nil
}
func (d *dummySurreal) VecDelete(docID string) error                                    { return nil }
func (d *dummySurreal) GraphRelate(fromID, toID, relation string, weight float64) error { return nil }

func TestE2EMemoryLoop(t *testing.T) {
	mem, db, st := setupTestMem(t)
	defer st.Close()
	ctx := context.Background()

	// 1. 测试 Episodic Projector
	projector := consolidation.EpisodicProjectorHandler(db, nil)
	ev := types.Event{
		TaskID:  "session-1",
		Payload: []byte("Hello E2E"),
	}
	evBytes, _ := json.Marshal(ev)
	record := &store.OutboxRecord{
		TargetEngine: "episodic",
		Payload:      evBytes,
	}
	err := projector(ctx, record)
	if err != nil {
		t.Fatalf("Episodic Projector failed: %v", err)
	}

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM episodic_events WHERE session_id='session-1'").Scan(&count); err != nil {
		t.Fatalf("Failed to query episodic_events: %v", err)
	}
	if count != 1 {
		t.Fatalf("Expected 1 episodic event, got %d", count)
	}

	// 2. 测试 Semantic 双写与 Retrieval
	semanticWriter := mem.Semantic()
	fact := types.Entity{
		Type:       "Preference",
		Name:       "Language",
		Properties: map[string]any{"description": "User prefers Go"},
		TaintLevel: types.TaintMedium,
	}
	if err := semanticWriter.UpsertFact(ctx, fact, 0); err != nil {
		t.Fatalf("UpsertFact failed: %v", err)
	}

	hr := retrieval.NewHybridRetrieverWithCognitive(st, &testutil.MockGraphTraverser{}, nil, mem.Reflection(), &dummySurreal{}, semanticWriter)
	res, err := hr.Search(ctx, "Language", types.SearchScope{Type: "memory"}, types.RetrievalConfig{FinalTopK: 5})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}

	found := false
	for _, e := range res {
		if strings.HasPrefix(e.Source, "semantic") || strings.HasPrefix(e.Source, "entity:") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Failed to recall semantic entity immediately after write")
	}

	// 3. 失败任务 -> reflection_memory 闭环写入
	err = mem.Reflection().AppendReflection(ctx, types.ReflectionEntry{
		SessionID: "E2ESession",
		Strategy:  "Test strategy",
		Decision:  "Test decision",
	})
	if err != nil {
		t.Fatalf("Failed to write reflection: %v", err)
	}
	refRes, err := mem.Reflection().ListReflections(ctx, types.ReflectionQuery{SessionID: "E2ESession", K: 5})
	if err != nil || len(refRes) == 0 {
		t.Fatalf("Failed to recall reflection: %v", err)
	}

	// 4. ForgettingManager (模拟 40 天前数据)
	_, err = db.Exec("INSERT INTO episodic_events (session_id, seq, timestamp, event_type, source, content, salience, occurred_at) VALUES ('e2e-sess', 1, ?, 'observation', 'test', 'old data', 0.1, ?)", time.Now().UnixMilli(), time.Now().Add(-40*24*time.Hour).UnixMilli())
	if err != nil {
		t.Fatalf("Failed to insert old episodic event: %v", err)
	}
	fm := consolidation.NewForgettingManager(st, &dummySurreal{}, 0.5)

	if err := fm.PeriodicCleanup(); err != nil {
		t.Fatalf("PeriodicCleanup failed: %v", err)
	}

	var arch int
	var archOffset int64
	if err := db.QueryRow("SELECT archived, COALESCE(archive_offset, 0) FROM episodic_events WHERE session_id='e2e-sess'").Scan(&arch, &archOffset); err != nil {
		t.Fatalf("Failed to query old event status: %v", err)
	}
	if arch != 1 {
		t.Fatalf("Old event was not archived, arch=%d offset=%d", arch, archOffset)
	}

	// 5. Cognitive Replayer (模拟重启恢复)
	replayer := retrieval.NewCognitiveReplayer(db, &dummySurreal{})
	if err := replayer.Start(ctx); err != nil {
		t.Fatalf("Cognitive replayer failed: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // Allow goroutine to start and finish
}

type mockOutboxWriter struct {
	entries []protocol.OutboxEntry
}

func (m *mockOutboxWriter) Write(ctx context.Context, entry protocol.OutboxEntry) error {
	m.entries = append(m.entries, entry)
	return nil
}

type oomSummarizer struct{}

func (s *oomSummarizer) Summarize(ctx context.Context, text string, maxTokens int) (string, error) {
	return "", apperr.New(apperr.CodeResourceExhausted, "OOM")
}
func (s *oomSummarizer) InferRaw(ctx context.Context, prompt string, maxTokens int) (string, error) {
	return "", apperr.New(apperr.CodeResourceExhausted, "OOM")
}

func TestConsolidation_OOMRetry(t *testing.T) {
	mem, db, st := setupTestMem(t)
	defer st.Close()
	ctx := context.Background()

	// Seed some episodic events to trigger consolidation
	for i := 0; i < 50; i++ {
		_ = mem.Episodic().Append(ctx, types.Event{
			TaskID: "test-session",
			Type:   "user",
		}, types.TaintNone)
	}

	outbox := &mockOutboxWriter{}
	summarizer := &oomSummarizer{}

	pipeline := consolidation.NewConsolidationPipelineFull(
		mem.Episodic(),
		mem.Semantic(),
		nil, // skill registry not needed for this test
		summarizer,
		nil,
		nil,
		db,
	).WithOutbox(outbox)

	err := pipeline.Run(ctx, "test-session")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if aerr, ok := err.(*apperr.Error); !ok || aerr.Code != apperr.CodeResourceExhausted {
		t.Fatalf("expected CodeResourceExhausted, got %v", err)
	}

	if len(outbox.entries) != 1 {
		t.Fatalf("expected 1 outbox entry, got %d", len(outbox.entries))
	}

	entry := outbox.entries[0]
	if entry.Operation != "memory_consolidate_retry" {
		t.Errorf("expected operation memory_consolidate_retry, got %s", entry.Operation)
	}
	if entry.TargetEngine != "memory" {
		t.Errorf("expected target engine memory, got %s", entry.TargetEngine)
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(entry.Payload, &payload); err != nil {
		t.Fatalf("failed to parse payload: %v", err)
	}
	if payload["session_id"] != "test-session" {
		t.Errorf("expected session_id test-session, got %v", payload["session_id"])
	}
}
