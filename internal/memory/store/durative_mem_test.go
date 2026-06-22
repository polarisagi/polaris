package store

import (
	"context"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/internal/memory/testutil"
)

func TestDurativeMemoryManager(t *testing.T) {
	ctx := context.Background()
	store := testutil.NewMockStore()
	episodic := NewEpisodicMem(store)
	provider := &testutil.MockProvider{Resp: `{"is_continuous": true, "summary": "s", "label": "l"}`}

	dm := NewDurativeMemoryManager(episodic, provider, store)

	// Consolidate with no events
	err := dm.Consolidate(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Add enough events to trigger clustering
	for i := 0; i < 5; i++ {
		_ = episodic.Append(ctx, types.Event{
			ID:        "e1",
			Payload:   []byte(`{"a": 1}`),
			CreatedAt: time.Now(),
		})
	}

	err = dm.Consolidate(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Store a DurativeGroup manually to test RetrieveGroups
	dgJSON := `{"id":"g1","status":"active","summary":"test summary","label":"test label"}`
	_ = store.Put(ctx, []byte("durative_group:g1"), []byte(dgJSON))
	_ = store.Put(ctx, []byte("durative_group:g2"), []byte(`{"status":"archived"}`))

	groups := dm.RetrieveGroups(ctx, "test", 5)
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
}
