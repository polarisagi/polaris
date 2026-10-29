package llm

import (
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/config"
)

func TestCircuitBreaker_HalfOpenConcurrency(t *testing.T) {
	cb := newCircuitBreaker(config.M1RouterThresholds{
		CircuitBreakerFailureCount:    2,
		CircuitBreakerCooldownSeconds: 1, // 1 second for faster test
	})

	// 1. Initial State -> Closed
	if !cb.Allow() {
		t.Fatalf("expected Allow() to be true initially")
	}

	// 2. Fail twice to open
	cb.RecordFailure()
	cb.RecordFailure()

	if cb.Allow() {
		t.Fatalf("expected Allow() to be false when Open")
	}

	// 3. Wait for cooldown to expire
	time.Sleep(1500 * time.Millisecond)

	// 4. Test Single Probe Semantics concurrently
	const concurrency = 100
	var wg sync.WaitGroup
	var allowedCount int
	var mu sync.Mutex

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if cb.Allow() {
				mu.Lock()
				allowedCount++
				mu.Unlock()
			}
		}()
	}

	wg.Wait()

	if allowedCount != 1 {
		t.Fatalf("expected exactly 1 probe to be allowed, got %d", allowedCount)
	}

	// 5. Test another failure while HalfOpen -> returns to Open and refreshes cooldown
	cb.RecordFailure()

	if cb.Allow() {
		t.Fatalf("expected Allow() to be false after probe failure")
	}

	// Ensure openUntil was refreshed
	time.Sleep(100 * time.Millisecond)
	if cb.Allow() {
		t.Fatalf("expected Allow() to be false, still in cooldown")
	}
}
