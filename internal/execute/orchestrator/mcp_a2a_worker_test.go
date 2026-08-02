package orchestrator

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

// fakeA2ABlackboard 是 MCPA2AWorker 测试专用的最小 protocol.Blackboard 替身，
// 记录 CompleteTask/FailTask 的调用参数供断言（mockBlackboard 的 FailTask 是
// 无操作 stub，CompleteTask 不保留 result，均不满足本文件的断言需求）。
type fakeA2ABlackboard struct {
	mu       sync.Mutex
	tasks    map[string]*types.TaskEntry
	events   chan types.BlackboardEvent
	done     map[string][]byte
	failed   map[string][]byte
	claimErr error
}

func newFakeA2ABlackboard() *fakeA2ABlackboard {
	return &fakeA2ABlackboard{
		tasks:  make(map[string]*types.TaskEntry),
		events: make(chan types.BlackboardEvent, 10),
		done:   make(map[string][]byte),
		failed: make(map[string][]byte),
	}
}

func (b *fakeA2ABlackboard) PostTask(ctx context.Context, task *types.TaskEntry) error { return nil }
func (b *fakeA2ABlackboard) PostBatch(ctx context.Context, tasks []*types.TaskEntry) error {
	return nil
}
func (b *fakeA2ABlackboard) ClaimTask(ctx context.Context, taskID, agentID string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.claimErr != nil {
		return false, b.claimErr
	}
	entry, ok := b.tasks[taskID]
	if !ok || entry.Status != types.TaskPending {
		return false, nil
	}
	entry.ClaimedBy = agentID
	entry.Status = types.TaskClaimed
	return true, nil
}
func (b *fakeA2ABlackboard) StartExecution(ctx context.Context, taskID, agentID string) error {
	return nil
}
func (b *fakeA2ABlackboard) CompleteTask(ctx context.Context, taskID, agentID string, result []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.done[taskID] = result
	return nil
}
func (b *fakeA2ABlackboard) FailTask(ctx context.Context, taskID, agentID string, errBytes []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failed[taskID] = errBytes
	return nil
}
func (b *fakeA2ABlackboard) RenewLease(ctx context.Context, taskID, agentID string) error { return nil }
func (b *fakeA2ABlackboard) SuspendForHITL(ctx context.Context, taskID, agentID string, timeout int64) error {
	return nil
}
func (b *fakeA2ABlackboard) ResumeFromHITL(ctx context.Context, taskID, agentID string, approved bool) error {
	return nil
}
func (b *fakeA2ABlackboard) BeginCompensation(ctx context.Context, taskID, agentID string) error {
	return nil
}
func (b *fakeA2ABlackboard) EndCompensation(ctx context.Context, taskID, agentID string) error {
	return nil
}
func (b *fakeA2ABlackboard) SideEffectPreCheck(_ context.Context, _, _ string, _ int32) error {
	return nil
}
func (b *fakeA2ABlackboard) CountByStatus(statuses ...types.TaskStatus) int { return 0 }
func (b *fakeA2ABlackboard) MaxActivePriority() int                         { return 3 }
func (b *fakeA2ABlackboard) PeekTask(ctx context.Context, taskID string) (*types.TaskSnapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.tasks[taskID]
	if !ok {
		return nil, nil
	}
	return &types.TaskSnapshot{
		ID: entry.ID, Status: entry.Status, Namespace: entry.Namespace,
		Type: entry.Type, Intent: entry.Intent, SpawnDepth: entry.SpawnDepth,
	}, nil
}
func (b *fakeA2ABlackboard) Subscribe(ctx context.Context) (<-chan types.BlackboardEvent, error) {
	return b.events, nil
}
func (b *fakeA2ABlackboard) UpdateTaskTokens(_ context.Context, _ string, _, _, _ int, _ float64) error {
	return nil
}

func (b *fakeA2ABlackboard) doneResult(taskID string) ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	v, ok := b.done[taskID]
	return v, ok
}

func (b *fakeA2ABlackboard) failedMsg(taskID string) ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	v, ok := b.failed[taskID]
	return v, ok
}

// fakeMCPToolCaller 是 MCPToolCaller 的测试替身。
type fakeMCPToolCaller struct {
	mu           sync.Mutex
	servers      map[string]string // name -> serverID
	callResult   string
	callErr      error
	lastServerID string
	lastTool     string
	lastArgs     map[string]any
	callCount    int
}

func (c *fakeMCPToolCaller) CallTool(ctx context.Context, serverID, toolName string, args map[string]any) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.callCount++
	c.lastServerID = serverID
	c.lastTool = toolName
	c.lastArgs = args
	if c.callErr != nil {
		return "", c.callErr
	}
	return c.callResult, nil
}

func (c *fakeMCPToolCaller) ResolveServerIDByName(name string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id, ok := c.servers[name]
	return id, ok
}

func startAndPost(t *testing.T, bb *fakeA2ABlackboard, worker *MCPA2AWorker, entry *types.TaskEntry) {
	t.Helper()
	bb.mu.Lock()
	bb.tasks[entry.ID] = entry
	bb.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = worker.RunLoop(ctx) }()

	time.Sleep(10 * time.Millisecond)
	bb.events <- types.BlackboardEvent{Type: "task_posted", TaskID: entry.ID}
	time.Sleep(50 * time.Millisecond)
}

// TestMCPA2AWorker_DelegatesToTargetServer 验证核心路径：认领
// "agent_handoff:mcp:<server>/<agent>" 任务后正确解析 server/agent，调用
// a2a_delegate 工具，成功后 CompleteTask。
func TestMCPA2AWorker_DelegatesToTargetServer(t *testing.T) {
	bb := newFakeA2ABlackboard()
	caller := &fakeMCPToolCaller{
		servers:    map[string]string{"linear": "srv-1"},
		callResult: `{"status":"accepted"}`,
	}
	worker := NewMCPA2AWorker(bb, caller, time.Second)

	startAndPost(t, bb, worker, &types.TaskEntry{
		ID: "task-a2a-1", Type: "agent_handoff:mcp:linear/researcher",
		Status: types.TaskPending, Intent: []byte("summarize X"), Namespace: "ns-1",
	})

	result, ok := bb.doneResult("task-a2a-1")
	if !ok {
		t.Fatalf("expected task completed, failed msgs: %v", bb.failed)
	}
	if string(result) != `{"status":"accepted"}` {
		t.Errorf("expected CallTool result written as-is, got %s", result)
	}
	if caller.lastServerID != "srv-1" || caller.lastTool != mcpA2ADelegateToolName {
		t.Errorf("expected CallTool(srv-1, %q, ...), got (%s, %s)", mcpA2ADelegateToolName, caller.lastServerID, caller.lastTool)
	}
	if caller.lastArgs["target_agent"] != "researcher" {
		t.Errorf("expected target_agent=researcher, got %v", caller.lastArgs["target_agent"])
	}
}

// TestMCPA2AWorker_IgnoresNonMCPHandoffTasks 验证前缀匹配严格性：普通
// "agent_handoff:librarian"（非 mcp: 前缀）不被本 Worker 认领。
func TestMCPA2AWorker_IgnoresNonMCPHandoffTasks(t *testing.T) {
	bb := newFakeA2ABlackboard()
	caller := &fakeMCPToolCaller{servers: map[string]string{"linear": "srv-1"}}
	worker := NewMCPA2AWorker(bb, caller, time.Second)

	startAndPost(t, bb, worker, &types.TaskEntry{
		ID: "task-local-1", Type: "agent_handoff:librarian",
		Status: types.TaskPending, Intent: []byte("x"),
	})

	if _, ok := bb.doneResult("task-local-1"); ok {
		t.Fatal("expected non-mcp: handoff task to be left untouched (not completed)")
	}
	if _, ok := bb.failedMsg("task-local-1"); ok {
		t.Fatal("expected non-mcp: handoff task to be left untouched (not failed)")
	}
	if caller.callCount != 0 {
		t.Fatalf("expected CallTool never invoked, got %d calls", caller.callCount)
	}
}

// TestMCPA2AWorker_UnknownServerFailsTask 验证目标 server 未连接时 FailTask，
// 不 panic、不发起网络调用。
func TestMCPA2AWorker_UnknownServerFailsTask(t *testing.T) {
	bb := newFakeA2ABlackboard()
	caller := &fakeMCPToolCaller{servers: map[string]string{}}
	worker := NewMCPA2AWorker(bb, caller, time.Second)

	startAndPost(t, bb, worker, &types.TaskEntry{
		ID: "task-a2a-2", Type: "agent_handoff:mcp:unknown/researcher",
		Status: types.TaskPending, Intent: []byte("x"),
	})

	if _, ok := bb.failedMsg("task-a2a-2"); !ok {
		t.Fatal("expected task to be failed when target server is not connected")
	}
	if caller.callCount != 0 {
		t.Fatalf("expected CallTool never invoked for unknown server, got %d calls", caller.callCount)
	}
}

// TestMCPA2AWorker_ExceedsSpawnDepthFailsWithoutDispatch 验证 ADR-0084 决策9
// 的执行前二次深度校验：SpawnDepth 超限时直接 FailTask，不发起外部调用。
func TestMCPA2AWorker_ExceedsSpawnDepthFailsWithoutDispatch(t *testing.T) {
	bb := newFakeA2ABlackboard()
	caller := &fakeMCPToolCaller{servers: map[string]string{"linear": "srv-1"}}
	worker := NewMCPA2AWorker(bb, caller, time.Second)

	startAndPost(t, bb, worker, &types.TaskEntry{
		ID: "task-a2a-3", Type: "agent_handoff:mcp:linear/researcher",
		Status: types.TaskPending, Intent: []byte("x"), SpawnDepth: MaxSpawnDepth + 1,
	})

	if _, ok := bb.failedMsg("task-a2a-3"); !ok {
		t.Fatal("expected task to be failed when SpawnDepth exceeds MaxSpawnDepth")
	}
	if caller.callCount != 0 {
		t.Fatalf("expected CallTool never invoked when depth exceeded, got %d calls", caller.callCount)
	}
}

// TestMCPA2AWorker_CallToolErrorFailsTask 验证 CallTool 失败时 FailTask 而非
// 静默吞掉。
func TestMCPA2AWorker_CallToolErrorFailsTask(t *testing.T) {
	bb := newFakeA2ABlackboard()
	caller := &fakeMCPToolCaller{
		servers: map[string]string{"linear": "srv-1"},
		callErr: context.DeadlineExceeded,
	}
	worker := NewMCPA2AWorker(bb, caller, time.Second)

	startAndPost(t, bb, worker, &types.TaskEntry{
		ID: "task-a2a-4", Type: "agent_handoff:mcp:linear/researcher",
		Status: types.TaskPending, Intent: []byte("x"),
	})

	if _, ok := bb.failedMsg("task-a2a-4"); !ok {
		t.Fatal("expected task to be failed when CallTool returns an error")
	}
}

// TestMCPA2AWorker_MissingAgentSegmentDefaultsToDefault 验证 "mcp:<server>"
// （无 "/<agent>" 段）时退化为 agent="default"，而非报格式错误。
func TestMCPA2AWorker_MissingAgentSegmentDefaultsToDefault(t *testing.T) {
	bb := newFakeA2ABlackboard()
	caller := &fakeMCPToolCaller{
		servers:    map[string]string{"linear": "srv-1"},
		callResult: "ok",
	}
	worker := NewMCPA2AWorker(bb, caller, time.Second)

	startAndPost(t, bb, worker, &types.TaskEntry{
		ID: "task-a2a-5", Type: "agent_handoff:mcp:linear/",
		Status: types.TaskPending, Intent: []byte("x"),
	})

	if _, ok := bb.doneResult("task-a2a-5"); !ok {
		t.Fatalf("expected task completed with default agent, failed: %v", bb.failed)
	}
	if caller.lastArgs["target_agent"] != "default" {
		t.Errorf("expected target_agent=default when agent segment is empty, got %v", caller.lastArgs["target_agent"])
	}
}
