package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

// fakeHandoffPoster 是 HandoffPoster 的纯内存测试替身。
type fakeHandoffPoster struct {
	mu         sync.Mutex
	tasks      map[string]*types.TaskSnapshot
	lastPosted *types.TaskEntry
}

func (f *fakeHandoffPoster) PostTask(_ context.Context, task *types.TaskEntry) error {
	f.mu.Lock()
	f.lastPosted = task
	f.tasks[task.ID] = &types.TaskSnapshot{ID: task.ID, Status: types.TaskPending, Namespace: task.Namespace, Type: task.Type}
	f.mu.Unlock()
	return nil
}

func (f *fakeHandoffPoster) PeekTask(_ context.Context, taskID string) (*types.TaskSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tasks[taskID], nil
}

func newTestHandoffAgent(t *testing.T) *Agent {
	t.Helper()
	a := NewAgent("handoff-test-agent", nil, nil)
	a.sCtx.SessionID = "session-1"
	return a
}

func TestExecuteTransferToAgent_Success(t *testing.T) {
	a := newTestHandoffAgent(t)
	a.sCtx.NamespaceID = "ns-shared"
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot)}
	a.InjectHandoffPoster(poster)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := a.executeTransferToAgent(ctx, "librarian", "please summarize X", types.TaintMedium)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if !res.Suspended {
		t.Fatalf("expected Suspended=true, got false")
	}
	if string(res.Output) != "Agent suspended waiting for handoff task completion." {
		t.Errorf("expected suspended message, got %s", res.Output)
	}
	if poster.lastPosted == nil {
		t.Fatal("expected a task to have been posted")
	}
	if poster.lastPosted.Namespace != "ns-shared" {
		t.Errorf("expected task namespace to reuse current NamespaceID, got %q", poster.lastPosted.Namespace)
	}
	if poster.lastPosted.Type != "agent_handoff:librarian" {
		t.Errorf("expected task type 'agent_handoff:librarian', got %q", poster.lastPosted.Type)
	}
}

func TestExecuteTransferToAgent_NamespaceFallsBackToSessionID(t *testing.T) {
	a := newTestHandoffAgent(t)
	// NamespaceID 留空，验证退化为 SessionID。
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot)}
	a.InjectHandoffPoster(poster)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := a.executeTransferToAgent(ctx, "librarian", "x", types.TaintLow); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if poster.lastPosted.Namespace != a.sCtx.SessionID {
		t.Errorf("expected namespace to fall back to SessionID %q, got %q", a.sCtx.SessionID, poster.lastPosted.Namespace)
	}
}

func TestExecuteTransferToAgent_MissingRole(t *testing.T) {
	a := newTestHandoffAgent(t)
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot)}
	a.InjectHandoffPoster(poster)

	_, err := a.executeTransferToAgent(context.Background(), "", "x", types.TaintLow)
	if err == nil {
		t.Fatal("expected error when target_agent_role is empty")
	}
}

func TestExecuteTransferToAgent_NilPosterFailsClosed(t *testing.T) {
	a := newTestHandoffAgent(t)
	// 未注入 HandoffPoster：必须 fail-closed 返回错误，而非 panic。
	_, err := a.executeTransferToAgent(context.Background(), "librarian", "x", types.TaintLow)
	if err == nil {
		t.Fatal("expected fail-closed error when handoffPoster is nil")
	}
}
