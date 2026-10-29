package llm

import (
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/config"
)

func TestWindowCircuitBreaker_ErrorRate(t *testing.T) {
	cfg := config.M1RouterThresholds{
		WindowBreakerWindowSecs:  1, // fast window for tests
		WindowBreakerMinSamples:  20,
		WindowBreakerThreshold:   0.5,
		WindowBreakerCooldownSec: 1, // short cooldown
	}

	wcb := newWindowCircuitBreaker(cfg)

	// Send 9 successes and 11 failures
	for i := 0; i < 9; i++ {
		wcb.RecordSuccess()
	}

	for i := 0; i < 11; i++ {
		wcb.RecordFailure()
	}

	// Now it should be tripped
	if wcb.Allow() {
		t.Fatal("expected breaker to be open after 11/20 failures")
	}

	// Wait for cooldown to pass and window to reset
	time.Sleep(1100 * time.Millisecond)

	if !wcb.Allow() {
		t.Fatal("expected breaker to be closed after cooldown and window reset")
	}
}
