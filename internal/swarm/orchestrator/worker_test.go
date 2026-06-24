package orchestrator

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

type mockAgentKernel struct {
	id     string
	state  types.AgentState
	result []byte
	ch     chan struct{}
}

func (m *mockAgentKernel) GetID() string { return m.id }
func (m *mockAgentKernel) Run(ctx context.Context) error {
	<-m.ch // block until triggered
	m.state = types.AgentStateComplete
	m.result = []byte(`{"status":"ok"}`)
	return nil
}
func (m *mockAgentKernel) SendIntent(trigger types.AgentTrigger) {
	close(m.ch) // trigger the run
}
func (m *mockAgentKernel) GetState() types.AgentState     { return m.state }
func (m *mockAgentKernel) SetTaskID(id string)            {}
func (m *mockAgentKernel) SetTaskIntent(intent []byte)    {}
func (m *mockAgentKernel) GetExecuteResult() []byte       { return m.result }
func (m *mockAgentKernel) GetTokenUsage() (int, int, int) { return 0, 0, 0 }

type mockBlackboard struct {
	mu     sync.Mutex
	tasks  map[string]*types.TaskEntry
	events chan types.BlackboardEvent
}

func (b *mockBlackboard) PostTask(ctx context.Context, task *types.TaskEntry) error {
	b.events <- types.BlackboardEvent{Type: "task_posted", TaskID: task.ID}
	return nil
}
func (b *mockBlackboard) PostBatch(ctx context.Context, tasks []*types.TaskEntry) error {
	return nil
}
func (b *mockBlackboard) ClaimTask(ctx context.Context, taskID, agentID string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.tasks[taskID]
	if !ok {
		return false, nil
	}
	entry.ClaimedBy = agentID
	entry.Status = types.TaskClaimed
	return true, nil
}
func (b *mockBlackboard) StartExecution(ctx context.Context, taskID, agentID string) error {
	return nil
}
func (b *mockBlackboard) CompleteTask(ctx context.Context, taskID, agentID string, result []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry := b.tasks[taskID]
	if entry != nil {
		entry.Status = types.TaskDone
	}
	return nil
}
func (b *mockBlackboard) FailTask(ctx context.Context, taskID, agentID string, errBytes []byte) error {
	return nil
}
func (b *mockBlackboard) RenewLease(ctx context.Context, taskID, agentID string) error {
	return nil
}
func (b *mockBlackboard) SuspendForHITL(ctx context.Context, taskID, agentID string, timeout int64) error {
	return nil
}
func (b *mockBlackboard) ResumeFromHITL(ctx context.Context, taskID, agentID string, approved bool) error {
	return nil
}
func (b *mockBlackboard) BeginCompensation(ctx context.Context, taskID, agentID string) error {
	return nil
}

func (b *mockBlackboard) AcquireBackgroundPermit(ctx context.Context, taskType string) error {
	return nil
}
func (b *mockBlackboard) EndCompensation(ctx context.Context, taskID, agentID string) error {
	return nil
}
func (m *mockBlackboard) SideEffectPreCheck(_ context.Context, _, _ string, _ int32) error {
	return nil
}

func (m *mockBlackboard) CountByStatus(ctx context.Context, status string) (int, error) {
	return 0, nil
}

func (m *mockBlackboard) MaxActivePriority(ctx context.Context) (int, error) {
	return 0, nil
}
func (b *mockBlackboard) PeekTask(ctx context.Context, taskID string) (*types.TaskSnapshot, error) {
	return nil, nil
}
func (b *mockBlackboard) Subscribe(ctx context.Context) (<-chan types.BlackboardEvent, error) {
	return b.events, nil
}
func (b *mockBlackboard) UpdateTaskTokens(_ context.Context, _ string, _, _, _ int, _ float64) error {
	return nil
}

func TestWorker_ListenLoop(t *testing.T) {
	bb := &mockBlackboard{
		tasks:  make(map[string]*types.TaskEntry),
		events: make(chan types.BlackboardEvent, 10),
	}

	entry := &types.TaskEntry{ID: "task-1"}
	entry.Status = types.TaskPending
	bb.tasks["task-1"] = entry

	kernel := &mockAgentKernel{
		id:    "agent-1",
		state: types.AgentStateIdle,
		ch:    make(chan struct{}),
	}

	worker := NewWorker("agent-1", bb, kernel)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = worker.ListenLoop(ctx)
	}()

	// 稍等一会确保订阅完成
	time.Sleep(10 * time.Millisecond)

	// 发布任务
	task := &types.TaskEntry{
		ID:       "task-1",
		Type:     "test_task",
		Priority: 1,
	}
	bb.PostTask(ctx, task)

	// 等待 Worker 抢占并执行完毕
	time.Sleep(50 * time.Millisecond)

	// 验证结果
	bb.mu.Lock()
	entry, ok := bb.tasks["task-1"]

	if !ok {
		bb.mu.Unlock()
		t.Fatalf("task not found in blackboard")
	}

	if entry.Status != types.TaskDone {
		t.Errorf("expected task to be Done, got %v", entry.Status)
	}

	if entry.ClaimedBy != "agent-1" {
		t.Errorf("expected task to be claimed by agent-1")
	}
	bb.mu.Unlock()
}

func TestWorker_ListenLoop_Push(t *testing.T) {
	bb := &mockBlackboard{
		tasks:  make(map[string]*types.TaskEntry),
		events: make(chan types.BlackboardEvent, 10),
	}

	entry := &types.TaskEntry{ID: "task-pushed"}
	entry.Status = types.TaskPending
	bb.tasks["task-pushed"] = entry

	kernel := &mockAgentKernel{
		id:    "agent-push",
		state: types.AgentStateIdle,
		ch:    make(chan struct{}),
	}

	worker := NewWorker("agent-push", bb, kernel)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = worker.ListenLoop(ctx)
	}()

	// 稍等一会确保订阅完成
	time.Sleep(10 * time.Millisecond)

	// Orchestrator 中心化推送任务
	worker.TaskPushChan <- "task-pushed"

	// 等待 Worker 抢占并执行完毕
	time.Sleep(50 * time.Millisecond)

	// 验证结果
	bb.mu.Lock()
	entry, ok := bb.tasks["task-pushed"]
	if !ok {
		bb.mu.Unlock()
		t.Fatalf("task not found in blackboard")
	}

	if entry.Status != types.TaskDone {
		t.Errorf("expected task to be Done, got %v", entry.Status)
	}

	if entry.ClaimedBy != "agent-push" {
		t.Errorf("expected task to be claimed by agent-push")
	}
	bb.mu.Unlock()
}
