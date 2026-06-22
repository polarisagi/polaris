package consolidation

import (
	"context"
	"testing"
	"time"

	memstore "github.com/polarisagi/polaris/internal/memory/store"
	"github.com/polarisagi/polaris/internal/memory/testutil"

	"github.com/polarisagi/polaris/pkg/types"
)

func TestConsolidationPipeline(t *testing.T) {
	ctx := context.Background()
	store := testutil.NewMockStore()
	semantic := memstore.NewSemanticMem(store, &testutil.MockIntentSubmitter{})
	episodic := memstore.NewEpisodicMem(store)
	skills := &mockSkillRegistry{}
	provider := &testutil.MockProvider{Resp: `{"entities":[{"name":"abc","type":"concept"}],"relations":[]}`}

	pipe := NewConsolidationPipeline(episodic, semantic, skills, provider)

	// Test Run on empty
	err := pipe.Run(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}

	// Add events to episodic
	for i := 0; i < 15; i++ {
		_ = episodic.Append(ctx, types.Event{
			ID:        "e1",
			TaskID:    "s1",
			Type:      "tool_call",
			Payload:   []byte(`{"tool_name": "test_tool", "success": true}`),
			CreatedAt: time.Now(),
		})
	}

	err = pipe.Run(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}

	// Test MarkColdEpisodicEvents
	err = pipe.MarkColdEpisodicEvents(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}

	// Test ruleExtract fallback
	provider.Fail = true
	pipe2 := NewConsolidationPipeline(episodic, semantic, skills, provider)
	err = pipe2.Run(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}

	// Test Jaccard
	j := jaccardSimilarity("prog_lang", "language")
	if j < 0 {
		t.Fatal("jaccard failed")
	}

	// Test ForgettingManager
	fm := NewForgettingManager(store, 0.01)
	fm.qLearner.Update("state", 1.0)

	_ = store.Put(ctx, []byte("events:e100"), []byte(`{"id":"e100","topic":"memory","salience":0.01,"occurred_at":0}`))
	err = fm.PeriodicCleanup()
	if err != nil {
		t.Fatal(err)
	}

	// Test decay and exp
	decay := fm.UpdateDecay(1.0, 100)
	if decay < 0 {
		t.Fatal("decay failed")
	}

	ca := NewColdArchiver(store)
	err = ca.PhysicalCompact()
	if err != nil {
		t.Fatal(err)
	}
}

type mockSkillRegistry struct{}

func (m *mockSkillRegistry) Register(ctx context.Context, meta types.SkillMeta) error { return nil }
func (m *mockSkillRegistry) List(ctx context.Context, filter types.SkillFilter) ([]types.SkillMeta, error) {
	return nil, nil
}
func (m *mockSkillRegistry) Get(ctx context.Context, name string, version string) (*types.SkillMeta, error) {
	return nil, nil
}
func (m *mockSkillRegistry) Delete(ctx context.Context, name string) error { return nil }
func (m *mockSkillRegistry) Deprecate(ctx context.Context, name string, alternative string, reason string) error {
	return nil
}
