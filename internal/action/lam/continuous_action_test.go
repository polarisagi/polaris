package lam

import (
	"math"
	"testing"
)

type mockProjector struct {
	toolName string
	args     map[string]any
	err      error
}

func (m *mockProjector) Project(vec []float64) (string, map[string]any, error) {
	return m.toolName, m.args, m.err
}

func TestContinuousAction_Discretize(t *testing.T) {
	d := &ActionDiscretizer{
		ActionMap: map[string]ActionProjector{
			"toolA": &mockProjector{toolName: "toolA", args: map[string]any{"a": 1}},
			"toolB": &mockProjector{toolName: "toolB", args: map[string]any{"b": 2}},
		},
	}

	// Test 1: Confidence < 0.3
	_, _, err := d.Discretize(ContinuousAction{Confidence: 0.2})
	if err == nil {
		t.Fatalf("expected error for confidence < 0.3, got nil")
	}

	// Test 2: Empty action vector
	_, _, err = d.Discretize(ContinuousAction{Confidence: 0.8, ActionVector: []float64{}})
	if err == nil {
		t.Fatalf("expected error for empty action vector, got nil")
	}

	// Test 3: No projectors
	emptyD := &ActionDiscretizer{}
	_, _, err = emptyD.Discretize(ContinuousAction{Confidence: 0.8, ActionVector: []float64{1.0}})
	if err == nil {
		t.Fatalf("expected error for empty discretizer, got nil")
	}

	// Test 4: Single projector
	singleD := &ActionDiscretizer{
		ActionMap: map[string]ActionProjector{
			"toolA": &mockProjector{toolName: "toolA", args: map[string]any{"a": 1}},
		},
	}
	name, args, err := singleD.Discretize(ContinuousAction{Confidence: 0.8, ActionVector: []float64{1.0}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "toolA" {
		t.Fatalf("expected toolA, got %s", name)
	}
	if args["a"] != 1 {
		t.Fatalf("expected args[a] = 1, got %v", args["a"])
	}

	// Test 5: Multiple projectors (choose based on cosine sim)
	// We craft a vector that is exactly the centroid of "toolA".
	cToolA := keyToCentroid("toolA", 4)
	name, _, err = d.Discretize(ContinuousAction{Confidence: 0.8, ActionVector: cToolA})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "toolA" {
		t.Fatalf("expected toolA, got %s", name)
	}
}

func TestKeyToCentroid(t *testing.T) {
	if keyToCentroid("test", 0) != nil {
		t.Fatalf("expected nil for dim 0")
	}
	vec := keyToCentroid("A", 2)
	// A is 65. i=0, 0%2=0. vec[0] gets 65. vec = [65, 0]. normalized: [1, 0]
	if math.Abs(vec[0]-1.0) > 1e-6 || math.Abs(vec[1]-0.0) > 1e-6 {
		t.Fatalf("unexpected centroid: %v", vec)
	}
}

func TestCosineSim(t *testing.T) {
	sim := cosineSim([]float64{1, 0}, []float64{1, 0})
	if math.Abs(sim-1.0) > 1e-6 {
		t.Fatalf("expected 1.0, got %f", sim)
	}
	sim = cosineSim([]float64{1, 0}, []float64{0, 1})
	if math.Abs(sim-0.0) > 1e-6 {
		t.Fatalf("expected 0.0, got %f", sim)
	}
	sim = cosineSim([]float64{0, 0}, []float64{1, 0})
	if sim != 0 {
		t.Fatalf("expected 0.0 for zero vector, got %f", sim)
	}
}

func TestNormalizeVec(t *testing.T) {
	vec := normalizeVec([]float64{3, 4})
	if math.Abs(vec[0]-0.6) > 1e-6 || math.Abs(vec[1]-0.8) > 1e-6 {
		t.Fatalf("unexpected normalized vec: %v", vec)
	}

	vecZero := normalizeVec([]float64{0, 0})
	if vecZero[0] != 0 || vecZero[1] != 0 {
		t.Fatalf("unexpected zero normalized vec: %v", vecZero)
	}
}
