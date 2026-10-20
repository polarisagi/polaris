package cronadmin

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

// 等待审批期间不消耗运行预算；不等待时预算耗尽即取消。
func TestSuperviseRunBudget_PausesWhileWaiting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var waiting atomic.Bool
	waiting.Store(true)
	var mu sync.Mutex
	var changes []bool
	go SuperviseBudget(ctx, cancel, 120*time.Millisecond, 10*time.Millisecond,
		func(context.Context) bool { return waiting.Load() },
		func(w bool) { mu.Lock(); changes = append(changes, w); mu.Unlock() })

	// 等待 3 倍预算的时间：仍不得被取消。
	select {
	case <-ctx.Done():
		t.Fatal("run budget must not tick while waiting for approval")
	case <-time.After(400 * time.Millisecond):
	}
	waiting.Store(false)
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("budget should be exhausted once approval is no longer pending")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(changes) < 2 || !changes[0] || changes[1] {
		t.Fatalf("expected suspended then running transitions, got %v", changes)
	}
}

type fakePending struct{ list []types.HITLPrompt }

func (f *fakePending) Prompt(context.Context, types.HITLPrompt) (*types.HITLResponse, error) {
	return nil, nil
}
func (f *fakePending) Pending(context.Context) ([]types.HITLPrompt, error) { return f.list, nil }

func TestSessionWaitingForApproval_MatchesSessionAndUnattendedOnly(t *testing.T) {
	gw := &fakePending{list: []types.HITLPrompt{
		{ID: "a", SessionID: "other", Unattended: true},
		{ID: "b", SessionID: "mine", Unattended: false},
	}}
	w := sessionWaitingForApproval(gw, "mine")
	if w(context.Background()) {
		t.Fatal("must only match unattended approvals of this session")
	}
	gw.list = append(gw.list, types.HITLPrompt{ID: "c", SessionID: "mine", Unattended: true})
	if !w(context.Background()) {
		t.Fatal("unattended approval of this session should count as waiting")
	}
}
