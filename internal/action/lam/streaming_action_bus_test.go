package lam

import (
	"context"
	"testing"
	"time"
)

type mockDisplayServer struct {
	lastAction any
	err        error
}

func (m *mockDisplayServer) SendAction(action any) error {
	m.lastAction = action
	return m.err
}

func (m *mockDisplayServer) GetFrame() ([]byte, error) {
	return nil, nil
}

func TestStreamingActionBus(t *testing.T) {
	ds := &mockDisplayServer{}
	bus := NewStreamingActionBus(ds, 5, 100)

	// Test StepCount and Reset
	if bus.StepCount() != 0 {
		t.Fatalf("expected 0, got %d", bus.StepCount())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	err := bus.StreamAction(ctx, ContinuousAction{ActionVector: []float64{1, 2}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bus.StepCount() != 1 {
		t.Fatalf("expected 1, got %d", bus.StepCount())
	}

	bus.Reset()
	if bus.StepCount() != 0 {
		t.Fatalf("expected 0 after reset")
	}

	// Test max steps
	bus.maxSteps = 1
	err = bus.StreamAction(ctx, ContinuousAction{ActionVector: []float64{1, 2}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	err = bus.StreamAction(ctx, ContinuousAction{ActionVector: []float64{1, 2}})
	if err == nil {
		t.Fatalf("expected max steps error, got nil")
	}

	// Test rate limit timeout
	bus.Reset()
	bus.rateLimiter.tokens = 0
	bus.rateLimiter.tokensPerSec = 0.001 // very slow

	ctx2, cancel2 := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel2()
	err = bus.StreamAction(ctx2, ContinuousAction{ActionVector: []float64{1, 2}})
	if err == nil {
		t.Fatalf("expected context deadline error, got nil")
	}
}

func TestStreamingActionBus_WithClipper(t *testing.T) {
	ds := &mockDisplayServer{}
	bus := NewStreamingActionBus(ds, 10, 100).WithClipper([]float64{0, -1}, []float64{10, 1})

	ctx := context.Background()
	err := bus.StreamAction(ctx, ContinuousAction{ActionVector: []float64{-5, 5}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check clipped vector
	actionMap, ok := ds.lastAction.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", ds.lastAction)
	}
	vec := actionMap["vector"].([]float64)
	if vec[0] != 0 || vec[1] != 1 {
		t.Fatalf("expected clipped [0, 1], got %v", vec)
	}
}

func TestStreamingActionBus_NoDisplayServer(t *testing.T) {
	bus := NewStreamingActionBus(nil, 10, 100)
	ctx := context.Background()
	err := bus.StreamAction(ctx, ContinuousAction{ActionVector: []float64{1, 2}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestActionRateLimiter_Acquire(t *testing.T) {
	rl := &ActionRateLimiter{
		maxActionsPerWindow: 2,
		windowDurationMs:    100,
		tokensPerSec:        1000, // virtually infinite tokens
		tokens:              1000,
		lastRefill:          time.Now(),
		windowEnd:           time.Now().Add(100 * time.Millisecond),
	}

	ctx := context.Background()

	// Should acquire 2 immediately
	if err := rl.Acquire(ctx); err != nil {
		t.Fatalf("failed to acquire token 1: %v", err)
	}
	if err := rl.Acquire(ctx); err != nil {
		t.Fatalf("failed to acquire token 2: %v", err)
	}

	// 3rd should block until window rolls over
	ctxTimeout, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := rl.Acquire(ctxTimeout); err == nil {
		t.Fatalf("expected timeout for token 3 due to window limit")
	}

	// Advance time
	time.Sleep(150 * time.Millisecond)
	if err := rl.Acquire(ctx); err != nil {
		t.Fatalf("failed to acquire token after window reset: %v", err)
	}
}

func TestActionClipper_Clip(t *testing.T) {
	c := &ActionClipper{
		mins: []float64{0, -5},
		maxs: []float64{10, 5},
	}

	v := c.Clip([]float64{-1, 10, 50})
	if v[0] != 0 {
		t.Fatalf("expected 0, got %f", v[0])
	}
	if v[1] != 5 {
		t.Fatalf("expected 5, got %f", v[1])
	}
	if v[2] != 50 {
		t.Fatalf("expected 50 (unclipped), got %f", v[2])
	}
}
