package embedonnx

import (
	"context"
	"testing"
	"time"
)

func TestDecideTierTableDriven(t *testing.T) {
	tests := []struct {
		name       string
		memTotalMB uint64
		memAvailMB uint64
		gemmaP95Ms float64
		bgeP95Ms   float64
		contended  bool
		wantTier   string
		wantModel  string
		wantRetry  bool
	}{
		{
			name:       "MemoryLowerBound_TotalUnder1.5GB",
			memTotalMB: 1024,
			memAvailMB: 800,
			gemmaP95Ms: 20.0,
			bgeP95Ms:   10.0,
			contended:  false,
			wantTier:   TierLight,
			wantModel:  ModelVersionBGE,
			wantRetry:  false,
		},
		{
			name:       "MemoryLowerBound_AvailUnder600MB",
			memTotalMB: 4096,
			memAvailMB: 500,
			gemmaP95Ms: 30.0,
			bgeP95Ms:   15.0,
			contended:  false,
			wantTier:   TierLight,
			wantModel:  ModelVersionBGE,
			wantRetry:  false,
		},
		{
			name:       "Row1_BalancedFast_NonContended",
			memTotalMB: 8192,
			memAvailMB: 4096,
			gemmaP95Ms: 45.0,
			bgeP95Ms:   15.0,
			contended:  false,
			wantTier:   TierBalanced,
			wantModel:  ModelVersionGemma,
			wantRetry:  false,
		},
		{
			name:       "Row1_BalancedFast_ContendedIgnored",
			memTotalMB: 8192,
			memAvailMB: 4096,
			gemmaP95Ms: 60.0,
			bgeP95Ms:   20.0,
			contended:  true,
			wantTier:   TierBalanced,
			wantModel:  ModelVersionGemma,
			wantRetry:  false,
		},
		{
			name:       "Row1_Boundary_80msExact",
			memTotalMB: 8192,
			memAvailMB: 4096,
			gemmaP95Ms: 80.0,
			bgeP95Ms:   25.0,
			contended:  false,
			wantTier:   TierBalanced,
			wantModel:  ModelVersionGemma,
			wantRetry:  false,
		},
		{
			name:       "Row2_LightweightFast_NonContended",
			memTotalMB: 8192,
			memAvailMB: 4096,
			gemmaP95Ms: 120.0,
			bgeP95Ms:   22.0,
			contended:  false,
			wantTier:   TierLight,
			wantModel:  ModelVersionBGE,
			wantRetry:  false,
		},
		{
			name:       "Row2_Boundary_30msExact",
			memTotalMB: 8192,
			memAvailMB: 4096,
			gemmaP95Ms: 80.001,
			bgeP95Ms:   30.0,
			contended:  false,
			wantTier:   TierLight,
			wantModel:  ModelVersionBGE,
			wantRetry:  false,
		},
		{
			name:       "Row3_ContendedIndeterminate",
			memTotalMB: 8192,
			memAvailMB: 4096,
			gemmaP95Ms: 150.0,
			bgeP95Ms:   40.0,
			contended:  true,
			wantTier:   TierLight,
			wantModel:  ModelVersionBGE,
			wantRetry:  true,
		},
		{
			name:       "Row4_BothExceeded_FallbackFTS",
			memTotalMB: 8192,
			memAvailMB: 4096,
			gemmaP95Ms: 110.0,
			bgeP95Ms:   35.0,
			contended:  false,
			wantTier:   TierNone,
			wantModel:  "",
			wantRetry:  false,
		},
		{
			name:       "Row4_Boundary_30.001ms_FallbackFTS",
			memTotalMB: 8192,
			memAvailMB: 4096,
			gemmaP95Ms: 85.0,
			bgeP95Ms:   30.001,
			contended:  false,
			wantTier:   TierNone,
			wantModel:  "",
			wantRetry:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := DecideTier(tt.memTotalMB, tt.memAvailMB, tt.gemmaP95Ms, tt.bgeP95Ms, tt.contended)
			if res.Tier != tt.wantTier {
				t.Errorf("Tier mismatch: got %q, want %q", res.Tier, tt.wantTier)
			}
			if res.ModelVersion != tt.wantModel {
				t.Errorf("ModelVersion mismatch: got %q, want %q", res.ModelVersion, tt.wantModel)
			}
			if res.RetryOnStart != tt.wantRetry {
				t.Errorf("RetryOnStart mismatch: got %v, want %v", res.RetryOnStart, tt.wantRetry)
			}
			if res.Contended != tt.contended {
				t.Errorf("Contended mismatch: got %v, want %v", res.Contended, tt.contended)
			}
		})
	}
}

func TestMeasureP95(t *testing.T) {
	ctx := context.Background()
	p95, err := MeasureP95(ctx, func(_ context.Context, _ string) error {
		time.Sleep(100 * time.Microsecond)
		return nil
	})
	if err != nil {
		t.Fatalf("MeasureP95 failed: %v", err)
	}
	if p95 <= 0 {
		t.Fatalf("expected positive p95, got %f", p95)
	}
}

func TestWaitForCPUIdle(t *testing.T) {
	// 模拟连续 3 次 < 50%
	readings := []float64{60.0, 40.0, 30.0, 20.0}
	idx := 0
	getCPU := func() float64 {
		if idx < len(readings) {
			v := readings[idx]
			idx++
			return v
		}
		return 10.0
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ok := WaitForCPUIdle(ctx, getCPU, 8*time.Second)
	if !ok {
		t.Fatalf("expected WaitForCPUIdle to succeed")
	}
}
