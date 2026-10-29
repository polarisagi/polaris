package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// fakeCheckpointRepo 是 protocol.TaskCheckpointRepository 的纯内存测试替身，
// 只有 ListByStatus 会被 AwaitingHandoffReconciler 用到。
type fakeCheckpointRepo struct {
	rows []types.TaskCheckpointRow
}

func (f *fakeCheckpointRepo) GetCheckpoint(context.Context, string, string, int) (*types.TaskCheckpointRow, error) {
	return nil, nil
}
func (f *fakeCheckpointRepo) GetLatestCheckpoint(context.Context, string, string) (*types.TaskCheckpointRow, error) {
	return nil, nil
}
func (f *fakeCheckpointRepo) UpsertCheckpoint(context.Context, types.TaskCheckpointRow) error {
	return nil
}
func (f *fakeCheckpointRepo) ListCheckpointsByTask(context.Context, string) ([]types.TaskCheckpointRow, error) {
	return nil, nil
}
func (f *fakeCheckpointRepo) ListByStatus(_ context.Context, status string) ([]types.TaskCheckpointRow, error) {
	var out []types.TaskCheckpointRow
	for _, r := range f.rows {
		if r.Status == status {
			out = append(out, r)
		}
	}
	return out, nil
}

// fakeSingleAgentPool 把固定的 *Agent 包装为 protocol.AgentPool：Acquire 总是
// 返回同一个实例，release 记录调用次数，供测试断言"确实归还了容量"。
type fakeSingleAgentPool struct {
	agent *Agent

	mu           sync.Mutex
	acquireCalls int
	releaseCalls int
}

func (p *fakeSingleAgentPool) Acquire(context.Context, string) (protocol.AgentController, func(), error) {
	p.mu.Lock()
	p.acquireCalls++
	p.mu.Unlock()
	return p.agent, func() {
		p.mu.Lock()
		p.releaseCalls++
		p.mu.Unlock()
	}, nil
}
func (p *fakeSingleAgentPool) AcquireHeadless(context.Context, types.Intent, ...types.HeadlessOption) (*types.AgentResult, error) {
	return nil, context.DeadlineExceeded
}

func (p *fakeSingleAgentPool) counts() (acquire, release int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.acquireCalls, p.releaseCalls
}

// TestReconcile_ChildTaskAlreadyDone 覆盖"进程重启时子任务已经跑完"分支：
// Reconcile 必须先用 ResumeAwaitingHandoff 把新 Acquire 出来的 Agent 就位到
// S_AWAIT_AGENT，再投递 TriggerAgentHandoffDone，而不是直接对停在 S_IDLE
// 的新实例发送触发（会命中 Dispatch 的 "no transition from S_IDLE" 硬错误）。
func TestReconcile_ChildTaskAlreadyDone(t *testing.T) {
	a := newTestHandoffAgent(t)
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot)}
	a.InjectHandoffPoster(poster)

	const childID = "handoff-child-done"
	poster.tasks[childID] = &types.TaskSnapshot{ID: childID, Status: types.TaskDone}

	repo := &fakeCheckpointRepo{rows: []types.TaskCheckpointRow{
		{TaskID: a.sCtx.SessionID, NodeID: childID, Status: "await_agent", Reason: "handoff_wait"},
	}}
	pool := &fakeSingleAgentPool{agent: a}

	r := NewAwaitingHandoffReconciler(repo, pool, poster)
	r.drainTimeout = 500 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}

	// Reconcile 对每一行 checkpoint 的实际处理都在独立 goroutine
	// （reconcileOne）里异步进行，这里等待该 goroutine 把 Trigger 写入
	// a.intent（drainTimeout 很短，goroutine 会很快因超时退出并 release）。
	select {
	case trigger := <-a.intent:
		if trigger != types.TriggerAgentHandoffDone {
			t.Fatalf("expected TriggerAgentHandoffDone, got %v", trigger)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for reconciler to send TriggerAgentHandoffDone")
	}

	if a.sm.Current() != types.AgentStateAwaitAgent {
		t.Fatalf("expected ResumeAwaitingHandoff to have hydrated FSM to S_AWAIT_AGENT, got %v", a.sm.Current())
	}
	if a.sCtx.HandoffTaskID != childID {
		t.Fatalf("expected HandoffTaskID to be backfilled to %q, got %q", childID, a.sCtx.HandoffTaskID)
	}

	// drainTimeout 到期后 reconcileOne 必须 release，归还 Pool 容量。
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, release := pool.counts(); release > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for release")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, release := pool.counts(); release != 1 {
		t.Fatalf("expected exactly 1 release call, got %d", release)
	}
}

// TestReconcile_ChildTaskStillPending 覆盖"子任务仍在运行"分支：必须重新
// 挂载 watchHandoffCompletion，而不是原地放弃（原实现的问题：仅 log 一句
// "re-attaching watcher" 却从未真正调用，等价于永久失联）。
func TestReconcile_ChildTaskStillPending(t *testing.T) {
	a := newTestHandoffAgent(t)
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot)}
	a.InjectHandoffPoster(poster)

	const childID = "handoff-child-pending"
	poster.tasks[childID] = &types.TaskSnapshot{ID: childID, Status: types.TaskPending}

	repo := &fakeCheckpointRepo{rows: []types.TaskCheckpointRow{
		{TaskID: a.sCtx.SessionID, NodeID: childID, Status: "await_agent", Reason: "handoff_wait"},
	}}
	pool := &fakeSingleAgentPool{agent: a}

	r := NewAwaitingHandoffReconciler(repo, pool, poster)
	r.drainTimeout = 2 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}

	// watcher 挂载后短暂等待其完成 Subscribe，再推送匹配事件（阶段04 A-01：
	// watchHandoffCompletion 改为事件驱动，不再靠轮询捕获状态翻转）。
	time.Sleep(100 * time.Millisecond)
	poster.mu.Lock()
	poster.tasks[childID].Status = types.TaskDone
	ch := poster.subscribeCh
	poster.mu.Unlock()
	if ch == nil {
		t.Fatal("expected re-attached watcher to have called Subscribe by now")
	}
	ch <- types.BlackboardEvent{Type: "task_completed", TaskID: childID}

	select {
	case trigger := <-a.intent:
		if trigger != types.TriggerAgentHandoffDone {
			t.Fatalf("expected TriggerAgentHandoffDone, got %v", trigger)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("timed out waiting for re-attached watcher to fire TriggerAgentHandoffDone")
	}
}

// TestReconcile_DedupSkipsInFlightSession 验证同一 sessionID 在前一轮尚未
// 结束前，后续扫描不会重复 Acquire（避免重复挂 watcher 造成的 churn）。
func TestReconcile_DedupSkipsInFlightSession(t *testing.T) {
	a := newTestHandoffAgent(t)
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot)}
	a.InjectHandoffPoster(poster)

	const childID = "handoff-child-dedup"
	poster.tasks[childID] = &types.TaskSnapshot{ID: childID, Status: types.TaskPending}

	repo := &fakeCheckpointRepo{rows: []types.TaskCheckpointRow{
		{TaskID: a.sCtx.SessionID, NodeID: childID, Status: "await_agent", Reason: "handoff_wait"},
	}}
	pool := &fakeSingleAgentPool{agent: a}

	r := NewAwaitingHandoffReconciler(repo, pool, poster)
	r.drainTimeout = 3 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("first Reconcile returned error: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // 确保第一轮的 goroutine 已经跑到 LoadOrStore

	// 第一轮 goroutine 尚未结束（子任务仍 pending，drainTimeout=3s），
	// 立即发起第二轮扫描应被去重跳过，不应产生第二次 Acquire。
	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("second Reconcile returned error: %v", err)
	}

	time.Sleep(200 * time.Millisecond)
	if acquire, _ := pool.counts(); acquire != 1 {
		t.Fatalf("expected exactly 1 acquire call (dedup should skip the second scan), got %d", acquire)
	}
}
