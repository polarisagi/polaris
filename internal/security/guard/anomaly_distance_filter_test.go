package guard

import (
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

func TestAnomalyDistanceFilter(t *testing.T) {
	filter := NewAnomalyDistanceFilter(3.0)

	// Test bypass when no samples
	taint, err := filter.Check("taskA", []float64{1.0, 2.0})
	if taint != types.TaintMedium || err != nil {
		t.Fatalf("expected TaintMedium and nil error on cold start, got %v, %v", taint, err)
	}

	// Train filter with variance
	for i := 0; i < 35; i++ {
		filter.Record("taskA", []float64{10.0 + float64(i%2), 20.0 + float64(i%2)})
	}

	// Test normal
	taint, err = filter.Check("taskA", []float64{10.0, 20.0})
	if taint != types.TaintNone || err != nil {
		t.Fatalf("expected TaintNone and nil error for normal sample, got %v, %v", taint, err)
	}

	// Test anomaly
	taint, err = filter.Check("taskA", []float64{100.0, 200.0})
	if taint != types.TaintHigh || err != ErrAnomalyDetected {
		t.Fatalf("expected TaintHigh and ErrAnomalyDetected for anomalous sample, got %v, %v", taint, err)
	}

	// Test dims 0
	taint, err = filter.Check("taskB", []float64{})
	if taint != types.TaintMedium || err != nil {
		t.Fatalf("expected TaintMedium for unseen task and 0 dims, got %v, %v", taint, err)
	}
}
