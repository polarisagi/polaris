package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type mockOffloader struct {
	offloaded map[string][]byte
}

func (m *mockOffloader) Offload(_ context.Context, taskID string, data []byte) (string, error) {
	if m.offloaded == nil {
		m.offloaded = make(map[string][]byte)
	}
	id := "mock-offload-123"
	m.offloaded[id] = data
	return id, nil
}

// TestSpawnAgents_ConcurrentThreeAgents 验证一次并发派生 3 个子智能体（ADR-0115 WP4）。
func TestSpawnAgents_ConcurrentThreeAgents(t *testing.T) {
	a := newTestHandoffAgent(t)
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot)}
	a.InjectHandoffPoster(poster)

	tasks := []types.SpawnTaskSpec{
		{Prompt: "subtask 1", Role: "researcher"},
		{Prompt: "subtask 2", Role: "coder"},
		{Prompt: "subtask 3", Role: "reviewer"},
	}

	childIDs, err := a.SpawnAgents(context.Background(), types.SpawnAgentsArgs{
		Tasks: tasks,
		Wait:  "all",
	})
	if err != nil {
		t.Fatalf("SpawnAgents failed: %v", err)
	}
	if len(childIDs) != 3 {
		t.Fatalf("expected 3 child IDs, got %d", len(childIDs))
	}

	poster.mu.Lock()
	defer poster.mu.Unlock()
	for _, id := range childIDs {
		snap, ok := poster.tasks[id]
		if !ok || snap == nil {
			t.Errorf("task %s was not posted to blackboard", id)
		}
	}

	// 验证已设置为集合并投递 TriggerAwaitAgent
	if len(a.sCtx.HandoffTaskIDs) != 3 {
		t.Errorf("expected 3 HandoffTaskIDs, got %v", a.sCtx.HandoffTaskIDs)
	}
}

// TestSpawnAgents_WaitNone_StatusAndResult 验证 wait="none" 模式与状态/产物查询。
func TestSpawnAgents_WaitNone_StatusAndResult(t *testing.T) {
	a := newTestHandoffAgent(t)
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot)}
	a.InjectHandoffPoster(poster)

	tasks := []types.SpawnTaskSpec{
		{Prompt: "async task 1", Role: "worker"},
		{Prompt: "async task 2", Role: "worker"},
	}

	childIDs, err := a.SpawnAgents(context.Background(), types.SpawnAgentsArgs{
		Tasks: tasks,
		Wait:  "none",
	})
	if err != nil {
		t.Fatalf("SpawnAgents failed: %v", err)
	}

	// wait=none 时不挂起，不设置 HandoffTaskIDs
	if len(a.sCtx.HandoffTaskIDs) != 0 {
		t.Errorf("wait=none should not set HandoffTaskIDs, got %v", a.sCtx.HandoffTaskIDs)
	}

	// 模拟子任务推进
	poster.mu.Lock()
	poster.tasks[childIDs[0]].Status = types.TaskExecuting
	poster.tasks[childIDs[1]].Status = types.TaskDone
	poster.tasks[childIDs[1]].Result = []byte("result 2")
	poster.mu.Unlock()

	// 查询状态
	statusList, err := a.AgentStatus(context.Background(), childIDs)
	if err != nil {
		t.Fatalf("AgentStatus failed: %v", err)
	}
	if len(statusList) != 2 {
		t.Fatalf("expected 2 status entries, got %d", len(statusList))
	}
	if statusList[0].Status != "running" {
		t.Errorf("task 0 expected running, got %s", statusList[0].Status)
	}
	if statusList[1].Status != "done" {
		t.Errorf("task 1 expected done, got %s", statusList[1].Status)
	}

	// 查询未完成任务的产物
	res0, err := a.AgentResult(context.Background(), childIDs[0])
	if err != nil {
		t.Fatalf("AgentResult for running task returned error: %v", err)
	}
	if !strings.Contains(res0, "尚未完成") {
		t.Errorf("expected not done notice, got: %s", res0)
	}

	// 查询已完成任务的产物
	res1, err := a.AgentResult(context.Background(), childIDs[1])
	if err != nil {
		t.Fatalf("AgentResult for done task failed: %v", err)
	}
	if res1 != "result 2" {
		t.Errorf("expected 'result 2', got: %s", res1)
	}
}

// TestSpawnAgents_ResultTruncateAndOffload 验证超过 4000 字符的结果截断与 read_tool_ref 卸载。
func TestSpawnAgents_ResultTruncateAndOffload(t *testing.T) {
	a := newTestHandoffAgent(t)
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot)}
	a.InjectHandoffPoster(poster)

	offloader := &mockOffloader{}
	a.toolOffloader = offloader

	hugeResult := strings.Repeat("a", 5000)
	taskID := "task-huge"
	poster.mu.Lock()
	poster.tasks[taskID] = &types.TaskSnapshot{
		ID:     taskID,
		Status: types.TaskDone,
		Result: []byte(hugeResult),
	}
	poster.mu.Unlock()

	res, err := a.AgentResult(context.Background(), taskID)
	if err != nil {
		t.Fatalf("AgentResult failed: %v", err)
	}

	if !strings.Contains(res, "read_tool_ref") {
		t.Errorf("expected read_tool_ref in truncated output, got: %s", res[:200])
	}
	if len(res) > 4100 {
		t.Errorf("expected truncated output <= 4100 chars, got %d", len(res))
	}
}

// TestSpawnAgents_MessageAndStop 验证向子智能体插话与中止原语。
func TestSpawnAgents_MessageAndStop(t *testing.T) {
	a := newTestHandoffAgent(t)
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot)}
	a.InjectHandoffPoster(poster)

	taskID := "subagent-test-1"
	poster.mu.Lock()
	poster.tasks[taskID] = &types.TaskSnapshot{ID: taskID, Status: types.TaskPending}
	poster.mu.Unlock()

	// 插话
	msgRes, err := a.AgentMessage(context.Background(), taskID, "优先处理错误分析")
	if err != nil {
		t.Fatalf("AgentMessage failed: %v", err)
	}
	if !strings.Contains(msgRes, "已成功") {
		t.Errorf("unexpected message result: %s", msgRes)
	}

	// 中止
	stopRes, err := a.AgentStop(context.Background(), taskID)
	if err != nil {
		t.Fatalf("AgentStop failed: %v", err)
	}
	if !strings.Contains(stopRes, "已成功中止") {
		t.Errorf("unexpected stop result: %s", stopRes)
	}

	poster.mu.Lock()
	snap := poster.tasks[taskID]
	poster.mu.Unlock()
	if snap.Status != types.TaskFailed {
		t.Errorf("expected task status failed, got %v", snap.Status)
	}
}

// TestSpawnAgents_ConcurrencyAndDepthLimits 验证并发上限与深度上限拦截。
func TestSpawnAgents_ConcurrencyAndDepthLimits(t *testing.T) {
	a := newTestHandoffAgent(t)
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot)}
	a.InjectHandoffPoster(poster)

	// standard 档位默认并发上限为 6（参见 profile.go）
	tasks := make([]types.SpawnTaskSpec, 10)
	for i := range tasks {
		tasks[i] = types.SpawnTaskSpec{Prompt: "prompt"}
	}

	_, err := a.SpawnAgents(context.Background(), types.SpawnAgentsArgs{
		Tasks: tasks,
		Wait:  "all",
	})
	if err == nil || !apperr.IsCode(err, apperr.CodeForbidden) {
		t.Fatalf("expected CodeForbidden for concurrency limit, got: %v", err)
	}

	// 深度超限：standard 深度上限为 3，若当前深度已为 3，再派生深度 4 必须被拦截
	a.sCtx.SpawnDepth = 3
	_, err = a.SpawnAgents(context.Background(), types.SpawnAgentsArgs{
		Tasks: []types.SpawnTaskSpec{{Prompt: "deep"}},
		Wait:  "all",
	})
	if err == nil || !apperr.IsCode(err, apperr.CodeForbidden) {
		t.Fatalf("expected CodeForbidden for delegation depth limit, got: %v", err)
	}
}

// TestSpawnAgents_ModelInheritance 验证未指定模型时继承父智能体模型。
func TestSpawnAgents_ModelInheritance(t *testing.T) {
	a := newTestHandoffAgent(t)
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot)}
	a.InjectHandoffPoster(poster)

	// 模拟手选或当前生效模型
	a.sCtx.UserLockedModel = "anthropic/claude-3-7-sonnet"

	childIDs, err := a.SpawnAgents(context.Background(), types.SpawnAgentsArgs{
		Tasks: []types.SpawnTaskSpec{{Prompt: "inherit test"}},
		Wait:  "none",
	})
	if err != nil {
		t.Fatalf("SpawnAgents failed: %v", err)
	}

	poster.mu.Lock()
	defer poster.mu.Unlock()
	snap := poster.tasks[childIDs[0]]
	var spec types.SpawnTaskSpec
	if uErr := json.Unmarshal(snap.Intent, &spec); uErr != nil {
		t.Fatalf("unmarshal intent failed: %v", uErr)
	}
	if spec.Model != "anthropic/claude-3-7-sonnet" {
		t.Errorf("expected inherited model 'anthropic/claude-3-7-sonnet', got %q", spec.Model)
	}
}

// TestSubagents_CompletionWake 验证全部子智能体完成后触发恢复。
func TestSubagents_CompletionWake(t *testing.T) {
	a := newTestHandoffAgent(t)
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot)}
	a.InjectHandoffPoster(poster)

	childIDs := []string{"child-1", "child-2"}
	for _, id := range childIDs {
		poster.tasks[id] = &types.TaskSnapshot{ID: id, Status: types.TaskExecuting}
	}

	a.sCtx.HandoffTaskIDs = childIDs
	a.watchSubagentsCompletion(childIDs)

	// 尚未完成，peekAllSubagentsDone 应为 false
	if a.peekAllSubagentsDone(context.Background(), childIDs) {
		t.Fatal("peekAllSubagentsDone should be false while running")
	}

	// 模拟完成
	poster.mu.Lock()
	for _, id := range childIDs {
		poster.tasks[id].Status = types.TaskDone
	}
	poster.mu.Unlock()

	// 验证已全部完成
	if !a.peekAllSubagentsDone(context.Background(), childIDs) {
		t.Fatal("peekAllSubagentsDone should be true when all done")
	}

	// 触发黑板事件唤醒
	ch, _ := poster.Subscribe(context.Background())
	_ = ch
	time.Sleep(50 * time.Millisecond)
}

// TestSpawnAgents_LocalOnlyMode_RejectsRemoteModel 验证 local_only_mode 下派生子智能体时若指定云端模型则硬拦截（V3）。
func TestSpawnAgents_LocalOnlyMode_RejectsRemoteModel(t *testing.T) {
	a := newTestHandoffAgent(t)
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot)}
	a.InjectHandoffPoster(poster)

	origCfg := config.Get()
	defer config.Update(origCfg)

	var newCfg config.Config
	if origCfg != nil {
		newCfg = *origCfg
	}
	newCfg.Security.LocalOnlyMode = true
	config.Update(&newCfg)

	// 1. 指定云端模型：被拦截
	_, err := a.SpawnAgents(context.Background(), types.SpawnAgentsArgs{
		Tasks: []types.SpawnTaskSpec{
			{Prompt: "test remote", Model: "openai/gpt-4o"},
		},
		Wait: "none",
	})
	if err == nil {
		t.Fatal("V3 缺陷：local_only_mode 下 spawn_agents 必须拒绝云端模型")
	}
	if !strings.Contains(err.Error(), "local_only_mode") {
		t.Fatalf("期望包含 local_only_mode 错误，实际: %v", err)
	}

	// 2. 指定本地模型：正常放行
	childIDs, err := a.SpawnAgents(context.Background(), types.SpawnAgentsArgs{
		Tasks: []types.SpawnTaskSpec{
			{Prompt: "test local", Model: "ollama/llama3"},
		},
		Wait: "none",
	})
	if err != nil {
		t.Fatalf("unexpected error with local model: %v", err)
	}
	if len(childIDs) != 1 {
		t.Fatalf("expected 1 child ID, got %d", len(childIDs))
	}
}
