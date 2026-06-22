package llm

import (
	"context"
	"testing"
	"time"
)

func TestCircuitBreaker(t *testing.T) {
	cb := NewCircuitBreaker(3, time.Millisecond*100, 2)

	if !cb.Allow() {
		t.Errorf("expected initially open")
	}

	for i := 0; i < 4; i++ {
		cb.RecordResult(false)
	}

	if cb.Allow() {
		t.Errorf("expected circuit broken")
	}

	time.Sleep(time.Millisecond * 150)
	if !cb.TryHalfOpen() {
		t.Errorf("expected half open to allow")
	}

	cb.RecordResult(true)
	if cb.state != 0 {
		t.Errorf("expected closed after success")
	}
}

type mockFallbackProvider struct {
	name      string
	available bool
}

func (m *mockFallbackProvider) Name() string      { return m.name }
func (m *mockFallbackProvider) IsAvailable() bool { return m.available }

func TestFallbackExecutor(t *testing.T) {
	mockProvider1 := &mockFallbackProvider{name: "p1", available: false}
	mockProvider2 := &mockFallbackProvider{name: "p2", available: true}

	cb := NewCircuitBreaker(3, time.Second, 1)
	scorer := &HealthScorer{}
	exec := NewFallbackExecutor(context.Background(), []Provider{mockProvider1, mockProvider2}, cb, scorer)

	err := exec.Execute()
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	if SelectFallback(FailRateLimit) != FallbackSecondary {
		t.Errorf("expected FallbackSecondary")
	}
	if SelectFallback(FailServerError) != FallbackSecondary {
		t.Errorf("expected FallbackSecondary")
	}
	if SelectFallback(FailTimeout) != FallbackTertiary {
		t.Errorf("expected FallbackTertiary")
	}
	if SelectFallback(FailContentFilter) != FallbackEscalate {
		t.Errorf("expected FallbackEscalate")
	}
	if SelectFallback(FailTokenLimit) != FallbackTertiary {
		t.Errorf("expected FallbackTertiary")
	}
	if SelectFallback("unknown") != FallbackGraceful {
		t.Errorf("expected FallbackGraceful")
	}

	if TierFromInt(0) != FallbackPrimary {
		t.Errorf("expected FallbackPrimary")
	}
	if TierFromInt(1) != FallbackSecondary {
		t.Errorf("expected FallbackSecondary")
	}
	if TierFromInt(2) != FallbackTertiary {
		t.Errorf("expected FallbackTertiary")
	}
	if TierFromInt(3) != FallbackGraceful {
		t.Errorf("expected FallbackGraceful")
	}
	if TierFromInt(99) != FallbackEscalate {
		t.Errorf("expected FallbackEscalate")
	}
}

func TestHealthScorer(t *testing.T) {
	scorer := &HealthScorer{
		availabilityWeight: 0.4,
		latencyWeight:      0.3,
		costWeight:         0.2,
		qualityWeight:      0.1,
	}
	stats := &ProviderStats{
		SuccessRate:  0.9,
		P95Latency:   200,
		CostAccuracy: 1.0,
		QualityScore: 0.8,
	}
	score := scorer.Score(stats)
	if score <= 0 {
		t.Errorf("expected positive score")
	}
}
