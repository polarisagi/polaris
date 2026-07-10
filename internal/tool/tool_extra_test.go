package tool

import (
	"context"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
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

type mockFailingSideEffectChecker struct {
	calls int
}

func (m *mockFailingSideEffectChecker) SideEffectPreCheck(ctx context.Context, taskID, agentID string, claimedVersion int32) error {
	m.calls++
	if m.calls >= 2 {
		return apperr.New(apperr.CodeConflict, "mock TOCTOU error")
	}
	return nil
}

func TestExecuteTool_TOCTOU_PostCheck(t *testing.T) {
	sbx := sandbox.NewInProcessSandbox()
	router := sandbox.NewSandboxRouter(sbx, nil, nil, "linux", 0)
	envelope := sandbox.NewExecEnvelope(&mockPolicyGate{allow: true}, router, 0, "linux", nil)
	r := NewInMemoryToolRegistry(envelope)
	r.WithSideEffectChecker(&mockFailingSideEffectChecker{})

	_ = r.Register(minTool("toctou-tool"))
	sbx.Register("toctou-tool", func(_ context.Context, _ []byte) ([]byte, error) {
		return []byte("done"), nil
	})

	ctx := context.WithValue(ctxWithToken(), protocol.CtxTaskIDKey{}, "task-123")
	// Also set IdempotencyKey so we can check it's not cached
	ctx = context.WithValue(ctx, protocol.CtxIdempotencyKey{}, types.IdempotencyKey("idemp-key-1"))

	res, err := r.ExecuteTool(ctx, "toctou-tool", []byte("data"), types.TaintNone)
	if err == nil {
		t.Fatal("expected error from TOCTOU post-check")
	}
	if !apperr.IsCode(err, apperr.CodeConflict) {
		t.Fatalf("expected CodeConflict, got %v", err)
	}
	if res.Success {
		t.Fatal("expected Success=false on TOCTOU error")
	}

	// Verify not cached in idempotency cache
	cached, ok := r.idempotencyCache.get("idemp-key-1")
	if ok || cached != nil {
		t.Fatal("expected failed result not to be cached")
	}
}
