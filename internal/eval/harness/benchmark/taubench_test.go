package benchmark

import (
	"context"
	"path/filepath"
	"testing"
)

func TestTauBenchAdapter_Load(t *testing.T) {
	adapter := &TauBenchAdapter{}
	cases, err := adapter.Load(context.Background(), filepath.Join("testdata", "taubench_sample.json"))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if len(cases) != 1 {
		t.Fatalf("Expected 1 case, got %d", len(cases))
	}

	c := cases[0]
	if c.ID != "test-1" {
		t.Errorf("Expected ID test-1, got %s", c.ID)
	}
	if c.Source != "tau-bench" {
		t.Errorf("Expected Source tau-bench, got %s", c.Source)
	}
}
