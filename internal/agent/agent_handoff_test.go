package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

// fakeHandoffPoster 是 HandoffPoster 的纯内存测试替身。autoCompleteAfter>0 时，
// 在 PostTask 后延迟指定时长自动将任务标记为 TaskDone，模拟目标 Worker 认领并
// 完成委派任务；autoCompleteAfter==0 时任务永不完成，用于验证超时路径。
type fakeHandoffPoster struct {
	mu                sync.Mutex
	tasks             map[string]*types.TaskSnapshot
	autoCompleteAfter time.Duration
	autoFail          bool
	lastPosted        *types.TaskEntry
}

func (f *fakeHandoffPoster) PostTask(_ context.Context, task *types.TaskEntry) error {
	f.mu.Lock()
	f.lastPosted = task
	f.tasks[task.ID] = &types.TaskSnapshot{ID: task.ID, Status: types.TaskPending, Namespace: task.Namespace, Type: task.Type}
	f.mu.Unlock()

	if f.autoCompleteAfter > 0 {
		go func() {
			time.Sleep(f.autoCompleteAfter)
			f.mu.Lock()
			defer f.mu.Unlock()
			snap := f.tasks[task.ID]
			if snap == nil {
				return
			}
			if f.autoFail {
				snap.Status = types.TaskFailed
			} else {
				snap.Status = types.TaskDone
				snap.Result = []byte("delegated-result")
			}
		}()
	}
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
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot), autoCompleteAfter: 50 * time.Millisecond}
	a.InjectHandoffPoster(poster)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := a.executeTransferToAgent(ctx, "librarian", "please summarize X", types.TaintMedium)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if !res.Success {
		t.Fatalf("expected Success=true, got false, output=%s", res.Output)
	}
	if string(res.Output) != "delegated-result" {
		t.Errorf("expected delegated-result, got %s", res.Output)
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
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot), autoCompleteAfter: 10 * time.Millisecond}
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

func TestExecuteTransferToAgent_DelegateFailed(t *testing.T) {
	a := newTestHandoffAgent(t)
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot), autoCompleteAfter: 10 * time.Millisecond, autoFail: true}
	a.InjectHandoffPoster(poster)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := a.executeTransferToAgent(ctx, "librarian", "x", types.TaintLow)
	if err != nil {
		t.Fatalf("expected nil error (delegate failure surfaces via ToolResult, not error), got %v", err)
	}
	if res.Success {
		t.Error("expected Success=false when delegate task fails")
	}
}

func TestExecuteTransferToAgent_TimesOutWithoutDeadlock(t *testing.T) {
	a := newTestHandoffAgent(t)
	// autoCompleteAfter=0：任务永不完成，验证 ctx 超时后能在有限时间内返回。
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot)}
	a.InjectHandoffPoster(poster)

	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	var res *types.ToolResult
	var err error
	go func() {
		res, err = a.executeTransferToAgent(ctx, "librarian", "x", types.TaintLow)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("executeTransferToAgent did not return after ctx deadline")
	}
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if res.Success {
		t.Error("expected Success=false on timeout")
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
