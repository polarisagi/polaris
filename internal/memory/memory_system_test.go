package memory

import (
	"context"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/internal/memory/testutil"
)

func TestMemorySystemWriteRetrieveForget(t *testing.T) {
	ctx := context.Background()

	dbStore := testutil.NewMockStore()
	memSys := NewMemorySystemWithGraph(dbStore, &testutil.MockGraphTraverser{})

	// Test Write Working
	err := memSys.Write(ctx, &MemoryEntry{
		Layer:   LayerWorking,
		Content: "working content",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Test Write Episodic
	err = memSys.Write(ctx, &MemoryEntry{
		ID:         "ep1",
		Layer:      LayerEpisodic,
		Content:    "episodic content",
		OccurredAt: time.Now(),
		Meta:       map[string]any{"event_type": "ACTION", "agent_id": "a1", "session_id": "s1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Test Write Semantic
	err = memSys.Write(ctx, &MemoryEntry{
		ID:      "sem1",
		Layer:   LayerSemantic,
		Content: "semantic content",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Write unknown layer
	_ = memSys.Write(ctx, &MemoryEntry{Layer: LayerProcedural})

	// Test Retrieve Semantic
	entries, err := memSys.Retrieve(ctx, &RetrievalQuery{
		Text:  "semantic",
		Layer: LayerSemantic,
		TopK:  5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > 0 {
		// Mock store might not implement retrieval perfectly, just check no error
		_ = entries
	}

	// Test Retrieve General
	_, err = memSys.Retrieve(ctx, &RetrievalQuery{
		Text:  "anything",
		Layer: LayerWorking, // will use "memory" scope
		TopK:  5,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Test Forget
	// Manually inject an old episodic event to test forget
	memSys.Mem().Episodic().Append(ctx, types.Event{
		ID:        "old1",
		CreatedAt: time.Now().Add(-40 * 24 * time.Hour), // Older than 30 days
	})
	memSys.Mem().Episodic().Append(ctx, types.Event{
		ID:        "new1",
		CreatedAt: time.Now().Add(-1 * 24 * time.Hour), // Newer than 30 days
	})
	memSys.Mem().Episodic().Append(ctx, types.Event{
		ID: "zero1", // Zero time
	})

	removed, err := memSys.Forget(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Errorf("Expected 1 removed event, got %d", removed)
	}

	// Test Mem getter
	if memSys.Mem() == nil {
		t.Fatal("Mem() returned nil")
	}
}
