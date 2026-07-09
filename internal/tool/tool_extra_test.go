package tool

import (
	"context"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/types"
)

type mockSideEffectChecker struct{}

func (m mockSideEffectChecker) SideEffectPreCheck(ctx context.Context, taskID, agentID string, claimedVersion int32) error {
	return nil
}

func TestToolExtra_WithSideEffectChecker(t *testing.T) {
	reg := NewInMemoryToolRegistry(sandbox.NewExecEnvelope(nil, nil, 0, "", nil))
	reg.WithSideEffectChecker(mockSideEffectChecker{})
	if reg.blackboard == nil {
		t.Fatalf("expected blackboard to be set")
	}
}

func TestToolExtra_isReversible(t *testing.T) {
	t1 := types.Tool{Capability: types.CapWriteNetwork}
	if isReversible(t1) {
		t.Fatalf("network tool should not be reversible")
	}

	t2 := types.Tool{
		Capability:  types.CapWriteLocal,
		SideEffects: []types.SideEffect{types.SideStateMutate},
	}
	if isReversible(t2) {
		t.Fatalf("state mutate tool should not be reversible")
	}

	t3 := types.Tool{
		Capability:  types.CapWriteLocal,
		SideEffects: []types.SideEffect{types.SideFileWrite},
	}
	if !isReversible(t3) {
		t.Fatalf("fs write tool should be reversible")
	}
}

func TestToolExtra_RateLimiter(t *testing.T) {
	rl := newRateLimiter(1)
	if !rl.Allow() {
		t.Fatalf("expected first allow to succeed")
	}
	if rl.Allow() {
		t.Fatalf("expected second allow to fail")
	}
	time.Sleep(1100 * time.Millisecond)
	if !rl.Allow() {
		t.Fatalf("expected allow after refill to succeed")
	}
}
