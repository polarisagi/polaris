package graph

import (
	"context"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/memory/testutil"
)

func TestEdgeWeightManager(t *testing.T) {
	ctx := context.Background()
	store := testutil.NewMockStore()
	ewm := NewEdgeWeightManager(store)

	w := ewm.ReinforcePath(ctx, "edge1", 0.5)
	if w <= 0.5 {
		t.Fatal("expected weight to increase")
	}

	w = ewm.ReinforcePath(ctx, "edge2", 1.0)
	if w > 1.0 {
		t.Fatal("weight exceeded 1.0")
	}

	w2 := ewm.DecayUnused(1.0, time.Now().Add(-60*24*time.Hour))
	if w2 >= 1.0 {
		t.Fatal("expected weight to decay")
	}

	w3 := ewm.DecayUnused(1.0, time.Now().Add(1*time.Hour))
	if w3 != 1.0 {
		t.Fatal("expected no decay for future/present")
	}

	err := ewm.FeedbackCalibrate(ctx, []string{"e1"})
	if err != nil {
		t.Fatal(err)
	}

	err = ewm.PeriodicPrune(ctx)
	if err != nil {
		t.Fatal(err)
	}
}
