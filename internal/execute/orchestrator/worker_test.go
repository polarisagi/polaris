package orchestrator

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

// mockAgentKernel 的 state/result/namespace 由 Worker 所在的后台 goroutine
// 写入（tryClaimAndExecute → Run/SetMemoryNamespace），测试主 goroutine 通过
// time.Sleep 等待后直接读取——两者之间没有 happens-before 关系，`go test -race`
// 会正确报告数据竞争（先前用裸字段读写，仅靠 sleep "凑巧不出错"）。
// 用 mu 统一保护，语义与 mockBlackboard 已有的锁模式保持一致。
type mockAgentKernel struct {
	mu        sync.Mutex
	id        string
	state     types.AgentState
	result    []byte
	ch        chan struct{}
	namespace string // 记录最后一次 SetMemoryNamespace 调用的值（GD-14-001 测试断言用）
}

func (m *mockAgentKernel) GetID() string { return m.id }
func (m *mockAgentKernel) Run(ctx context.Context) error {
	<-m.ch // block until triggered
	m.mu.Lock()
	m.state = types.AgentStateComplete
	m.result = []byte(`{"status":"ok"}`)
	m.mu.Unlock()
	return nil
}
func (m *mockAgentKernel) SendIntent(trigger types.AgentTrigger) {
	close(m.ch) // trigger the run
}
func (m *mockAgentKernel) GetState() types.AgentState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}
func (m *mockAgentKernel) SetTaskID(id string)         {}
func (m *mockAgentKernel) SetTaskIntent(intent []byte) {}
func (m *mockAgentKernel) SetMemoryNamespace(ns string) {
	m.mu.Lock()
	m.namespace = ns
	m.mu.Unlock()
}
func (m *mockAgentKernel) GetExecuteResult() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.result
}
func (m *mockAgentKernel) GetTokenUsage() (int, int, int) { return 0, 0, 0 }

// namespaceForTest 供测试断言读取 namespace，避免直接访问裸字段触发竞争。
func (m *mockAgentKernel) namespaceForTest() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.namespace
}

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

func (b *mockBlackboard) EndCompensation(ctx context.Context, taskID, agentID string) error {
	return nil
}
func (m *mockBlackboard) SideEffectPreCheck(_ context.Context, _, _ string, _ int32) error {
	return nil
}

func (m *mockBlackboard) CountByStatus(statuses ...types.TaskStatus) int {
	return 0
}

func (m *mockBlackboard) MaxActivePriority() int {
	return 3
}
func (b *mockBlackboard) PeekTask(ctx context.Context, taskID string) (*types.TaskSnapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.tasks[taskID]
	if !ok {
		return nil, nil
	}
	return &types.TaskSnapshot{ID: entry.ID, Status: entry.Status, Namespace: entry.Namespace, Type: entry.Type, Intent: entry.Intent}, nil
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

// TestWorker_TryClaimAndExecute_PropagatesNamespace 验证 GD-14-001 Worker 端
// 布线：认领任务后，Worker 读取 TaskEntry.Namespace（经 PeekTask 透传）并注入
// AgentKernel.SetMemoryNamespace，使协同任务下的 Worker Agent 能感知共享命名空间。
func TestWorker_TryClaimAndExecute_PropagatesNamespace(t *testing.T) {
	bb := &mockBlackboard{
		tasks:  make(map[string]*types.TaskEntry),
		events: make(chan types.BlackboardEvent, 10),
	}

	entry := &types.TaskEntry{ID: "task-ns-1", Namespace: "swarm-ns-shared"}
	entry.Status = types.TaskPending
	bb.tasks["task-ns-1"] = entry

	kernel := &mockAgentKernel{
		id:    "agent-ns",
		state: types.AgentStateIdle,
		ch:    make(chan struct{}),
	}

	worker := NewWorker("agent-ns", bb, kernel)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = worker.ListenLoop(ctx)
	}()

	time.Sleep(10 * time.Millisecond)
	worker.TaskPushChan <- "task-ns-1"
	time.Sleep(50 * time.Millisecond)

	if got := kernel.namespaceForTest(); got != "swarm-ns-shared" {
		t.Errorf("expected kernel.SetMemoryNamespace(%q), got %q", "swarm-ns-shared", got)
	}
}

// TestWorker_TryClaimAndExecute_NoNamespace 验证未设置 Namespace 的任务
// （单 Agent 场景，绝大多数任务）不会误注入命名空间——默认行为不变。
func TestWorker_TryClaimAndExecute_NoNamespace(t *testing.T) {
	bb := &mockBlackboard{
		tasks:  make(map[string]*types.TaskEntry),
		events: make(chan types.BlackboardEvent, 10),
	}

	entry := &types.TaskEntry{ID: "task-no-ns"}
	entry.Status = types.TaskPending
	bb.tasks["task-no-ns"] = entry

	kernel := &mockAgentKernel{
		id:    "agent-no-ns",
		state: types.AgentStateIdle,
		ch:    make(chan struct{}),
	}

	worker := NewWorker("agent-no-ns", bb, kernel)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = worker.ListenLoop(ctx)
	}()

	time.Sleep(10 * time.Millisecond)
	worker.TaskPushChan <- "task-no-ns"
	time.Sleep(50 * time.Millisecond)

	if got := kernel.namespaceForTest(); got != "" {
		t.Errorf("expected empty namespace for task without Namespace set, got %q", got)
	}
}
