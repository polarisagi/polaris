package supervisor

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func TestSupervisor_OneForOne_Restart(t *testing.T) {
	sup := NewSupervisor(3, 5*time.Second) // max 3 restarts within 5s
	// modify backoff for faster testing
	sup.baseBackoff = 10 * time.Millisecond
	sup.maxBackoff = 50 * time.Millisecond

	var panicCounter int32
	var successCounter int32

	sup.AddWorker("worker-panic", func(ctx context.Context) error {
		count := atomic.AddInt32(&panicCounter, 1)
		if count <= 2 {
			panic("simulated panic")
		}
		atomic.AddInt32(&successCounter, 1)
		return nil
	})

	sup.Start()

	// Since the worker finishes eventually, Wait() will complete when worker finishes
	// successfully and returns nil error.
	time.Sleep(100 * time.Millisecond) // Give it time to panic twice and recover
	sup.Stop()

	if atomic.LoadInt32(&panicCounter) != 3 {
		t.Errorf("expected worker to be called 3 times, got %d", atomic.LoadInt32(&panicCounter))
	}
	if atomic.LoadInt32(&successCounter) != 1 {
		t.Errorf("expected worker to succeed once, got %d", atomic.LoadInt32(&successCounter))
	}
}

func TestSupervisor_MaxRestartsExceeded(t *testing.T) {
	sup := NewSupervisor(2, 5*time.Second)
	sup.baseBackoff = 10 * time.Millisecond

	var executionCount int32

	sup.AddWorker("worker-fail", func(ctx context.Context) error {
		atomic.AddInt32(&executionCount, 1)
		return apperr.New(apperr.CodeInternal, "simulated fatal error")
	})

	sup.Start()

	// Wait should unblock after max restarts (initial run + 2 restarts = 3 executions)
	sup.Wait()

	if atomic.LoadInt32(&executionCount) != 3 {
		t.Errorf("expected exactly 3 executions, got %d", atomic.LoadInt32(&executionCount))
	}
}

func TestSupervisor_GracefulStop(t *testing.T) {
	sup := NewSupervisor(3, 5*time.Second)

	var executionCount int32

	sup.AddWorker("worker-sleep", func(ctx context.Context) error {
		atomic.AddInt32(&executionCount, 1)
		// Wait for cancellation
		<-ctx.Done()
		return ctx.Err()
	})

	sup.Start()

	// Let it start
	time.Sleep(10 * time.Millisecond)

	sup.Stop()

	if atomic.LoadInt32(&executionCount) != 1 {
		t.Errorf("expected exactly 1 execution, got %d", atomic.LoadInt32(&executionCount))
	}
}
