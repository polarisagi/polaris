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
	snap, ok := f.tasks[taskID]
	if !ok || snap == nil {
		return nil, nil
	}
	// 返回副本而非内部 map 值的指针：调用方（watcher/reconciler 的轮询
	// goroutine）会在释放锁之后读取返回值字段，若直接返回内部指针，
	// 测试里通过 poster.mu 保护的"写"和调用方无锁的"读"会形成数据竞争
	// （-race 可复现，2026-07-29 AwaitingHandoffReconciler 测试引入时发现）。
	cp := *snap
	return &cp, nil
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

// TestWatchHandoffCompletion_FiresResumeOnDone 回归测试 GD-1 修复：
// watcher 检测到子任务 Done 后，必须投递 TriggerAgentHandoffDone 唤醒 FSM，
// 而不是（修复前的）投递 TriggerSuspend 导致 Dispatch 返回
// "no transition from S_AWAIT_AGENT with trigger TriggerSuspend" 硬错误。
func TestWatchHandoffCompletion_FiresResumeOnDone(t *testing.T) {
	a := newTestHandoffAgent(t)
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot)}
	a.InjectHandoffPoster(poster)

	const childID = "handoff-child-1"
	poster.mu.Lock()
	poster.tasks[childID] = &types.TaskSnapshot{ID: childID, Status: types.TaskPending}
	poster.mu.Unlock()

	a.watchHandoffCompletion(childID)

	// 轮询间隔 1s，短暂等待后再翻转为 Done，确认 watcher 能在下一次
	// 轮询中捕获状态变化（而非只在启动瞬间读取一次）。
	time.Sleep(50 * time.Millisecond)
	poster.mu.Lock()
	poster.tasks[childID].Status = types.TaskDone
	poster.mu.Unlock()

	select {
	case trigger := <-a.intent:
		if trigger != types.TriggerAgentHandoffDone {
			t.Fatalf("expected TriggerAgentHandoffDone, got %v", trigger)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for watcher to fire TriggerAgentHandoffDone")
	}
}
