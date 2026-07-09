package sandbox

import (
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

func TestCalculateWasmQuota(t *testing.T) {
	q1 := CalculateWasmQuota(0, types.TaintLow)
	if q1.MemoryPages != 2048 || q1.Fuel != 10000000 || q1.MaxMounts != 1 {
		t.Fatalf("tier 0 low taint failed")
	}

	q2 := CalculateWasmQuota(1, types.TaintLow)
	if q2.MemoryPages != 8192 || q2.Fuel != 50000000 || q2.MaxMounts != 5 {
		t.Fatalf("tier 1 low taint failed")
	}

	q3 := CalculateWasmQuota(0, types.TaintHigh)
	if q3.MemoryPages != 1024 || q3.Fuel != 5000000 || q3.MaxMounts != 0 {
		t.Fatalf("tier 0 high taint failed")
	}
}
