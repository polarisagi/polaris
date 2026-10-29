package sleeptime_test

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/learning/sleeptime"
)

type mockEvaluator struct {
	baseIn  float64
	baseOut float64
	candIn  float64
	candOut float64
	err     error
}

func (m *mockEvaluator) Evaluate(ctx context.Context, overlayFiles map[string][]byte) (float64, float64, error) {
	if m.err != nil {
		return 0, 0, m.err
	}
	if overlayFiles == nil {
		return m.baseIn, m.baseOut, nil
	}
	return m.candIn, m.candOut, nil
}

func TestVerifyProposals_DualSplitDecisions(t *testing.T) {
	ctx := context.Background()
	changes := []sleeptime.ProposedChange{
		{Path: "prompt_addenda/test.md", Content: "content", Reason: "reason"},
	}

	tests := []struct {
		name         string
		baseIn       float64
		baseOut      float64
		candIn       float64
		candOut      float64
		wantAccepted bool
	}{
		{
			name:         "Both held-in and held-out improve -> ACCEPT",
			baseIn:       0.70,
			baseOut:      0.75,
			candIn:       0.80,
			candOut:      0.82,
			wantAccepted: true,
		},
		{
			name:         "Held-in improves, held-out equal -> ACCEPT",
			baseIn:       0.70,
			baseOut:      0.75,
			candIn:       0.78,
			candOut:      0.75,
			wantAccepted: true,
		},
		{
			name:         "Held-out improves, held-in equal -> ACCEPT",
			baseIn:       0.70,
			baseOut:      0.75,
			candIn:       0.70,
			candOut:      0.80,
			wantAccepted: true,
		},
		{
			name:         "Held-in improves but held-out regresses -> REJECT",
			baseIn:       0.70,
			baseOut:      0.75,
			candIn:       0.85,
			candOut:      0.70, // regression
			wantAccepted: false,
		},
		{
			name:         "Held-out improves but held-in regresses -> REJECT",
			baseIn:       0.70,
			baseOut:      0.75,
			candIn:       0.65, // regression
			candOut:      0.85,
			wantAccepted: false,
		},
		{
			name:         "Both scores identical (no improvement) -> REJECT",
			baseIn:       0.75,
			baseOut:      0.80,
			candIn:       0.75,
			candOut:      0.80,
			wantAccepted: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eval := &mockEvaluator{
				baseIn:  tt.baseIn,
				baseOut: tt.baseOut,
				candIn:  tt.candIn,
				candOut: tt.candOut,
			}
			accepted, bIn, bOut, cIn, cOut, reason, err := sleeptime.VerifyProposals(ctx, eval, changes)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if accepted != tt.wantAccepted {
				t.Errorf("accepted = %v, want %v (reason: %s, base: %.2f/%.2f, cand: %.2f/%.2f)",
					accepted, tt.wantAccepted, reason, bIn, bOut, cIn, cOut)
			}
		})
	}
}
