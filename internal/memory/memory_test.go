package memory

import (
	"testing"

	"github.com/polarisagi/polaris/internal/memory/testutil"
)

func TestMemImpl(t *testing.T) {
	store := testutil.NewMockStore()
	mem := NewMemImpl(store)
	if mem.Working() == nil {
		t.Fatal("Working() should not be nil")
	}
	if mem.Episodic() == nil {
		t.Fatal("Episodic() should not be nil")
	}
	if mem.Semantic() == nil {
		t.Fatal("Semantic() should not be nil")
	}
	if mem.Retriever() == nil {
		t.Fatal("Retriever() should not be nil")
	}
	if mem.Reflection() == nil {
		t.Fatal("Reflection() should not be nil")
	}

	mem.SetVectorMode(1)
	mem.InjectSkillRegistry(nil)

	stats, err := mem.StoreStats()
	if err != nil {
		t.Fatal("StoreStats should not fail")
	}
	if stats == "" {
		t.Fatal("StoreStats returned empty")
	}

	// Test fallback new methods
	memFull := NewMemImplFull(store, &testutil.MockGraphTraverser{}, nil, nil)
	if memFull == nil {
		t.Fatal("NewMemImplFull returned nil")
	}

	memDB := NewMemImplWithDB(store, nil)
	if memDB == nil {
		t.Fatal("NewMemImplWithDB returned nil")
	}
}
