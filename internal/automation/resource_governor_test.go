package automation

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
)

func TestResourceGovernor_AdmitPriority(t *testing.T) {
	rg := NewResourceGovernor(10, config.ResourceGovernorConfig{})
	// Override probes for deterministic test
	rg.memProbeFn = func() int64 { return 2048 }
	rg.cpuProbeFn = func() float64 { return 30.0 }

	// priority=0 always admit
	if !(func() bool { ok, _ := rg.Admit(0); return ok })() {
		t.Errorf("priority=0 should always admit")
	}
	rg.Release()

	// priority=1 admit under normal pressure
	if !(func() bool { ok, _ := rg.Admit(1); return ok })() {
		t.Errorf("priority=1 should admit under normal load")
	}
	rg.Release()

	// priority=5 admit under normal pressure
	if !(func() bool { ok, _ := rg.Admit(5); return ok })() {
		t.Errorf("priority=5 should admit under normal load")
	}
	rg.Release()
}

func TestResourceGovernor_MemoryPressure(t *testing.T) {
	rg := NewResourceGovernor(10, config.ResourceGovernorConfig{})
	rg.memProbeFn = func() int64 { return 256 } // below 512MB
	rg.cpuProbeFn = func() float64 { return 30.0 }

	// priority=0 always admit even under memory pressure
	if !(func() bool { ok, _ := rg.Admit(0); return ok })() {
		t.Errorf("priority=0 should always admit")
	}
	rg.Release()

	// priority=3 rejected under memory pressure
	if (func() bool { ok, _ := rg.Admit(3); return ok })() {
		t.Errorf("priority=3 should be rejected under memory pressure (<512MB free)")
	}
}

func TestResourceGovernor_CPUThreshold(t *testing.T) {
	rg := NewResourceGovernor(10, config.ResourceGovernorConfig{})
	rg.memProbeFn = func() int64 { return 2048 }
	rg.cpuProbeFn = func() float64 { return 80.0 }

	// priority=0 always admit
	if !(func() bool { ok, _ := rg.Admit(0); return ok })() {
		t.Errorf("priority=0 should always admit")
	}
	rg.Release()

	// priority=3 rejected under high CPU
	if (func() bool { ok, _ := rg.Admit(3); return ok })() {
		t.Errorf("priority=3 should be rejected under high CPU (>70%%)")
	}
}

func TestResourceGovernor_ConcurrentLimit(t *testing.T) {
	rg := NewResourceGovernor(3, config.ResourceGovernorConfig{})
	rg.memProbeFn = func() int64 { return 2048 }
	rg.cpuProbeFn = func() float64 { return 30.0 }

	// Fill all 3 slots
	for i := 0; i < 3; i++ {
		if !(func() bool { ok, _ := rg.Admit(1); return ok })() {
			t.Fatalf("slot %d: should admit", i)
		}
	}

	// 4th task rejected at capacity
	if (func() bool { ok, _ := rg.Admit(1); return ok })() {
		t.Errorf("4th task should be rejected (capacity=3)")
	}

	// priority=0 always admitted even at capacity
	if !(func() bool { ok, _ := rg.Admit(0); return ok })() {
		t.Errorf("priority=0 should always admit even at capacity")
	}
	rg.Release()

	// Release and retry
	rg.Release()
	rg.Release()
	rg.Release()

	if !(func() bool { ok, _ := rg.Admit(1); return ok })() {
		t.Errorf("after release, should admit")
	}
	rg.Release()
}

func TestResourceGovernor_WaitForCapacity(t *testing.T) {
	rg := NewResourceGovernor(1, config.ResourceGovernorConfig{})
	rg.memProbeFn = func() int64 { return 2048 }
	rg.cpuProbeFn = func() float64 { return 30.0 }

	// Fill the only slot
	if !(func() bool { ok, _ := rg.Admit(1); return ok })() {
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
