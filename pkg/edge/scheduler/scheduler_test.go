package scheduler

import (
	"context"
	"testing"
)

func TestResourceGovernor_AdmitPriority(t *testing.T) {
	rg := NewResourceGovernor(10)
	// Override probes for deterministic test
	rg.memProbeFn = func() int64 { return 2048 }
	rg.cpuProbeFn = func() float64 { return 30.0 }

	// priority=0 always admit
	if !rg.Admit(0) {
		t.Errorf("priority=0 should always admit")
	}
	rg.Release()

	// priority=1 admit under normal pressure
	if !rg.Admit(1) {
		t.Errorf("priority=1 should admit under normal load")
	}
	rg.Release()

	// priority=5 admit under normal pressure
	if !rg.Admit(5) {
		t.Errorf("priority=5 should admit under normal load")
	}
	rg.Release()
}

func TestResourceGovernor_MemoryPressure(t *testing.T) {
	rg := NewResourceGovernor(10)
	rg.memProbeFn = func() int64 { return 256 } // below 512MB
	rg.cpuProbeFn = func() float64 { return 30.0 }

	// priority=0 always admit even under memory pressure
	if !rg.Admit(0) {
		t.Errorf("priority=0 should always admit")
	}
	rg.Release()

	// priority=3 rejected under memory pressure
	if rg.Admit(3) {
		t.Errorf("priority=3 should be rejected under memory pressure (<512MB free)")
	}
}

func TestResourceGovernor_CPUThreshold(t *testing.T) {
	rg := NewResourceGovernor(10)
	rg.memProbeFn = func() int64 { return 2048 }
	rg.cpuProbeFn = func() float64 { return 80.0 }

	// priority=0 always admit
	if !rg.Admit(0) {
		t.Errorf("priority=0 should always admit")
	}
	rg.Release()

	// priority=3 rejected under high CPU
	if rg.Admit(3) {
		t.Errorf("priority=3 should be rejected under high CPU (>70%%)")
	}
}

func TestResourceGovernor_ConcurrentLimit(t *testing.T) {
	rg := NewResourceGovernor(3)
	rg.memProbeFn = func() int64 { return 2048 }
	rg.cpuProbeFn = func() float64 { return 30.0 }

	// Fill all 3 slots
	for i := 0; i < 3; i++ {
		if !rg.Admit(1) {
			t.Fatalf("slot %d: should admit", i)
		}
	}

	// 4th task rejected at capacity
	if rg.Admit(1) {
		t.Errorf("4th task should be rejected (capacity=3)")
	}

	// priority=0 always admitted even at capacity
	if !rg.Admit(0) {
		t.Errorf("priority=0 should always admit even at capacity")
	}
	rg.Release()

	// Release and retry
	rg.Release()
	rg.Release()
	rg.Release()

	if !rg.Admit(1) {
		t.Errorf("after release, should admit")
	}
	rg.Release()
}

func TestResourceGovernor_WaitForCapacity(t *testing.T) {
	rg := NewResourceGovernor(1)
	rg.memProbeFn = func() int64 { return 2048 }
	rg.cpuProbeFn = func() float64 { return 30.0 }

	// Fill the only slot
	if !rg.Admit(1) {
		t.Fatalf("should admit first task")
	}

	// Release in background
	go func() {
		rg.Release()
	}()

	ctx := context.Background()
	if err := rg.WaitForCapacity(ctx); err != nil {
		t.Errorf("WaitForCapacity should succeed, got %v", err)
	}
}
