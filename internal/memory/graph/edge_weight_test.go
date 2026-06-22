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

func TestEvidenceSubgraphExtractor(t *testing.T) {
	ctx := context.Background()
	store := testutil.NewMockStore()
	ese := NewEvidenceSubgraphExtractor(store)

	_, err := ese.Extract(ctx, nil)
	if err == nil {
		t.Fatal("expected error for nil seed")
	}

	res, err := ese.Extract(ctx, []string{"seed1"})
	if err != nil {
		t.Fatal(err)
	}
	if res == "" {
		t.Fatal("expected result string")
	}
}

func TestSynapticPlasticityManager(t *testing.T) {
	spm := NewSynapticPlasticityManager()
	if spm.PruneThreshold() != 0.1 {
		t.Fatal("wrong prune threshold")
	}

	w := spm.ReinforcePath(0.5, time.Now().UnixMilli())
	if w <= 0.5 {
		t.Fatal("expected weight to increase")
	}

	weights := map[string]float64{"e1": 0.5, "e2": 0.5}
	spm.FeedbackCalibrate([]string{"e1"}, []string{"e2"}, weights, 0.5)
	if weights["e1"] <= 0.5 {
		t.Fatal("expected e1 to increase")
	}
	if weights["e2"] >= 0.5 {
		t.Fatal("expected e2 to decrease")
	}

	w2 := spm.DecayUnused(1.0, 60)
	if w2 >= 1.0 {
		t.Fatal("expected weight to decay")
	}
}
