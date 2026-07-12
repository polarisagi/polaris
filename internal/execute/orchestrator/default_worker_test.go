package orchestrator

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// mockAgentPool 记录 AcquireHeadless 收到的 query，并按配置返回成功/失败，
// 用于验证 DefaultTaskWorker 是否正确把 TaskEntry.Intent 转交给 headless 推理，
// 以及正确处理成功/失败两条路径的 Blackboard 写回。
type mockAgentPool struct {
	mu          sync.Mutex
	queries     []string
	replyOutput string
	failWith    error
}

func (p *mockAgentPool) Acquire(ctx context.Context, sessionID string) (protocol.AgentController, func(), error) {
	return nil, func() {}, nil
}

func (p *mockAgentPool) AcquireHeadless(ctx context.Context, intent types.Intent, opts ...types.HeadlessOption) (*types.AgentResult, error) {
	p.mu.Lock()
	p.queries = append(p.queries, intent.Query)
	p.mu.Unlock()
	if p.failWith != nil {
		return nil, p.failWith
	}
	return &types.AgentResult{Output: p.replyOutput}, nil
}

func (p *mockAgentPool) queriesForTest() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.queries))
	copy(out, p.queries)
	return out
}

// TestDefaultTaskWorker_ClaimsAndExecutesOrphanedTask 验证核心修复行为：
// 之前无人认领的 "agent_query" 类型任务，现在会被 DefaultTaskWorker 认领，
// 其 Intent 原样作为 headless query 执行，成功后写回 CompleteTask。
func TestDefaultTaskWorker_ClaimsAndExecutesOrphanedTask(t *testing.T) {
	bb := &mockBlackboard{
		tasks:  make(map[string]*types.TaskEntry),
		events: make(chan types.BlackboardEvent, 10),
	}
	bb.tasks["task-aq-1"] = &types.TaskEntry{
		ID:     "task-aq-1",
		Type:   "agent_query",
		Status: types.TaskPending,
		Intent: []byte("what is the weather"),
	}

	pool := &mockAgentPool{replyOutput: "sunny"}
	worker := NewDefaultTaskWorker(bb, pool, "workflow_step")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = worker.RunLoop(ctx) }()

	time.Sleep(10 * time.Millisecond)
	bb.events <- types.BlackboardEvent{Type: "task_posted", TaskID: "task-aq-1"}
	time.Sleep(50 * time.Millisecond)

	bb.mu.Lock()
	entry := bb.tasks["task-aq-1"]
	bb.mu.Unlock()

	if entry.Status != types.TaskDone {
		t.Fatalf("expected task to be claimed and completed, got status %v", entry.Status)
	}
	if entry.ClaimedBy != defaultTaskWorkerAgentID {
		t.Fatalf("expected task claimed by %q, got %q", defaultTaskWorkerAgentID, entry.ClaimedBy)
	}
	queries := pool.queriesForTest()
	if len(queries) != 1 || queries[0] != "what is the weather" {
		t.Fatalf("expected AcquireHeadless called once with the task's Intent, got %v", queries)
	}
}

// TestDefaultTaskWorker_SkipsExcludedType 验证 excludeTypes 生效：属于专用
// Worker（如 workflow_step_worker.go）能力类型的任务不会被本 Worker 抢占，
// 避免把结构化 JSON intent 当纯文本传给 LLM。
func TestDefaultTaskWorker_SkipsExcludedType(t *testing.T) {
	bb := &mockBlackboard{
		tasks:  make(map[string]*types.TaskEntry),
		events: make(chan types.BlackboardEvent, 10),
	}
	bb.tasks["task-wf-1"] = &types.TaskEntry{
		ID:     "task-wf-1",
		Type:   "workflow_step",
		Status: types.TaskPending,
		Intent: []byte(`{"state_graph_node_id":"n1"}`),
	}

	pool := &mockAgentPool{replyOutput: "should not be called"}
	worker := NewDefaultTaskWorker(bb, pool, "workflow_step")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = worker.RunLoop(ctx) }()

	time.Sleep(10 * time.Millisecond)
	bb.events <- types.BlackboardEvent{Type: "task_posted", TaskID: "task-wf-1"}
	time.Sleep(50 * time.Millisecond)

	bb.mu.Lock()
	entry := bb.tasks["task-wf-1"]
	bb.mu.Unlock()

	if entry.Status != types.TaskPending {
		t.Fatalf("expected excluded-type task to remain untouched (Pending), got %v", entry.Status)
	}
	if len(pool.queriesForTest()) != 0 {
		t.Fatalf("expected AcquireHeadless never called for excluded type, got %v", pool.queriesForTest())
	}
}

// TestDefaultTaskWorker_HeadlessFailureFailsTask 验证 AcquireHeadless 报错时
// 走 FailTask 而非静默吞掉，避免任务永久卡在 claimed 状态。
func TestDefaultTaskWorker_HeadlessFailureFailsTask(t *testing.T) {
	bb := &mockFailTrackingBlackboard{
		mockBlackboard: mockBlackboard{
			tasks:  make(map[string]*types.TaskEntry),
			events: make(chan types.BlackboardEvent, 10),
		},
	}
	bb.tasks["task-fail-1"] = &types.TaskEntry{
		ID:     "task-fail-1",
		Type:   "provider_recovered",
		Status: types.TaskPending,
		Intent: []byte("resume this"),
	}

	pool := &mockAgentPool{failWith: context.DeadlineExceeded}
	worker := NewDefaultTaskWorker(bb, pool, "workflow_step")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = worker.RunLoop(ctx) }()

	time.Sleep(10 * time.Millisecond)
	bb.events <- types.BlackboardEvent{Type: "task_posted", TaskID: "task-fail-1"}
	time.Sleep(50 * time.Millisecond)

	if !bb.failCalledForTest() {
		t.Fatal("expected FailTask to be called after AcquireHeadless returns an error")
	}
}

// mockFailTrackingBlackboard 包一层 mockBlackboard，记录 FailTask 是否被调用
// （基类的 FailTask 是无操作 stub，无法从外部观测调用与否）。
type mockFailTrackingBlackboard struct {
	mockBlackboard
	mu         sync.Mutex
	failCalled bool
}

func (b *mockFailTrackingBlackboard) FailTask(ctx context.Context, taskID, agentID string, errBytes []byte) error {
	b.mu.Lock()
	b.failCalled = true
	b.mu.Unlock()
	return nil
}

func (b *mockFailTrackingBlackboard) failCalledForTest() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.failCalled
}
