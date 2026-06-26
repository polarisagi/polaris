package llm

import (
	"context"
	"testing"
	"time"
)

func TestStreamBudgetGuard(t *testing.T) {
	budget := &TokenBudget{remaining: 100}
	detector := NewTokenBurnDetector(5000)
	guard := NewStreamBudgetGuard(budget, detector, 50)

	if guard.GetMaxBufferSize() != 50 {
		t.Errorf("expected 50 max buffer size")
	}

	err := guard.GuardChunk(context.Background(), 1)
	if err != nil {
		t.Errorf("expected nil error on first chunk")
	}

	guard.chunkCount = 99 // next will be 100
	err = guard.GuardChunk(context.Background(), 1)
	if err != nil {
		t.Errorf("expected nil error on 100th chunk")
	}

	budget.remaining = 0
	guard.chunkCount = 199
	err = guard.GuardChunk(context.Background(), 1)
	if err != ErrStreamBudgetExhausted {
		t.Errorf("expected ErrStreamBudgetExhausted")
	}
}

func TestTokenBurnDetector(t *testing.T) {
	detector := NewTokenBurnDetector(5000)
	if detector.GetWindow() != 5000 {
		t.Errorf("expected 5000 window")
	}

	err := detector.CheckAcceleration(5)
	if err != nil {
		t.Errorf("expected no acceleration yet")
	}

	// fake samples
	detector.samples = append(detector.samples, burnSample{tokens: 0, ts: time.Now().UnixMicro() - 1000})
	detector.samples = append(detector.samples, burnSample{tokens: 10, ts: time.Now().UnixMicro() - 500})
	detector.samples = append(detector.samples, burnSample{tokens: 5000, ts: time.Now().UnixMicro()})

	// Provide a massive token increase so that regardless of CPU execution time (dt2),
	// the velocity v2 is astronomically high, guaranteeing accel > 3.0.
	err = detector.CheckAcceleration(500000000)
	if err == nil {
		t.Errorf("expected acceleration detected")
	}
}

func TestJSONRepair(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{`{"a": 1`, `{"a": 1}`},
		{`[1, 2`, `[1, 2]`},
		{`{"a": "b"`, `{"a": "b"}`},
		{`{"a": `, `{}`},
		{`{"a": "b",`, `{"a": "b"}`},
		{`{"a": "b", "c":`, `{"a": "b"}`},
	}

	for _, c := range cases {
		out, err := JSONRepair([]byte(c.input))
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if string(out.Repaired) != c.expected {
			t.Errorf("expected %s for %s, got %s", c.expected, c.input, string(out.Repaired))
		}
	}
}

func TestTrackStreamCost(t *testing.T) {
	err := TrackStreamCost(context.Background(), 10, "test")
	if err != nil {
		t.Errorf("expected nil error")
	}

	err = TrackStreamCost(context.Background(), 300000, "test")
	if err != ErrResponseTooLarge {
		t.Errorf("expected ErrResponseTooLarge")
	}
}

func TestStreamError(t *testing.T) {
	err := &StreamError{"oops"}
	if err.Error() != "oops" {
		t.Errorf("bad err")
	}

	alert := &BurnAlert{Acceleration: 5.0}
	if alert.Error() != "burn rate acceleration alert" {
		t.Errorf("bad err")
	}
}
