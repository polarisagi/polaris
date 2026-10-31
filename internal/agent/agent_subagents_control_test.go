package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type mockHandoffPosterWithEvents struct {
	mu          sync.Mutex
	tasks       map[string]*types.TaskSnapshot
	peekErrors  map[string]bool
	nilTaskIDs  map[string]bool
	failTaskIDs map[string]bool
	failCalls   []string
	postedTasks []*types.TaskEntry
	eventsMap   map[string][]types.AgentStreamEvent
	controlsMap map[string][]types.TaskControl
	postErrOnID string
}

func newMockHandoffPoster() *mockHandoffPosterWithEvents {
	return &mockHandoffPosterWithEvents{
		tasks:       make(map[string]*types.TaskSnapshot),
		peekErrors:  make(map[string]bool),
		nilTaskIDs:  make(map[string]bool),
		failTaskIDs: make(map[string]bool),
		eventsMap:   make(map[string][]types.AgentStreamEvent),
		controlsMap: make(map[string][]types.TaskControl),
	}
}

func (m *mockHandoffPosterWithEvents) PostTask(_ context.Context, task *types.TaskEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.postErrOnID != "" && strings.Contains(task.ID, m.postErrOnID) {
		return fmt.Errorf("blackboard post failed on %s", task.ID)
	}
	m.postedTasks = append(m.postedTasks, task)
	m.tasks[task.ID] = &types.TaskSnapshot{
		ID:        task.ID,
		Status:    types.TaskExecuting,
		Namespace: task.Namespace,
		Type:      task.Type,
		Intent:    task.Intent,
	}
	return nil
}

func (m *mockHandoffPosterWithEvents) PeekTask(_ context.Context, taskID string) (*types.TaskSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.peekErrors[taskID] {
		return nil, fmt.Errorf("temporary lookup failure: %s", taskID)
	}
	if m.nilTaskIDs[taskID] {
		return nil, nil
	}
	if snap, ok := m.tasks[taskID]; ok {
		return snap, nil
	}
	return nil, fmt.Errorf("task not found: %s", taskID)
}

func TestSpawnAgents_PeekTaskFailureConservativelyConsumesQuota(t *testing.T) {
	for _, mode := range []string{"error", "nil"} {
		t.Run(mode, func(t *testing.T) {
			a := newTestHandoffAgent(t)
			poster := newMockHandoffPoster()
			a.InjectHandoffPoster(poster)
			initial := make([]types.SpawnTaskSpec, 6)
			for i := range initial {
				initial[i] = types.SpawnTaskSpec{Prompt: fmt.Sprintf("initial %d", i), Role: "worker"}
			}
			ids, err := a.SpawnAgents(context.Background(), types.SpawnAgentsArgs{Tasks: initial, Wait: "none"})
			if err != nil || len(ids) != 6 {
				t.Fatalf("initial spawn: ids=%d err=%v", len(ids), err)
			}

			poster.mu.Lock()
			if mode == "error" {
				poster.peekErrors[ids[0]] = true
			} else {
				poster.nilTaskIDs[ids[0]] = true
			}
			poster.mu.Unlock()

			_, err = a.SpawnAgents(context.Background(), types.SpawnAgentsArgs{
				Tasks: []types.SpawnTaskSpec{{Prompt: "one more", Role: "worker"}}, Wait: "none",
			})
			if err == nil || !apperr.IsCode(err, apperr.CodeForbidden) {
				t.Fatalf("uncertain task status must conservatively consume quota, got %v", err)
			}

			// 一旦查库恢复并明确返回终态，该任务才可释放本地名额。
			poster.mu.Lock()
			delete(poster.peekErrors, ids[0])
			delete(poster.nilTaskIDs, ids[0])
			poster.tasks[ids[0]].Status = types.TaskDone
			poster.mu.Unlock()
			_, err = a.SpawnAgents(context.Background(), types.SpawnAgentsArgs{
				Tasks: []types.SpawnTaskSpec{{Prompt: "after confirmed terminal", Role: "worker"}}, Wait: "none",
			})
			if err != nil {
				t.Fatalf("confirmed terminal child should release quota: %v", err)
			}
		})
	}
}

func (m *mockHandoffPosterWithEvents) Subscribe(_ context.Context) (<-chan types.BlackboardEvent, error) {
	ch := make(chan types.BlackboardEvent)
	return ch, nil
}

func (m *mockHandoffPosterWithEvents) SubscribeTaskEvents(taskID string) (<-chan types.AgentStreamEvent, func()) {
	ch := make(chan types.AgentStreamEvent, 10)
	return ch, func() {}
}

func (m *mockHandoffPosterWithEvents) PublishTaskEvent(taskID string, event types.AgentStreamEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.eventsMap[taskID] = append(m.eventsMap[taskID], event)
}

func (m *mockHandoffPosterWithEvents) SendTaskControl(taskID string, ctrl types.TaskControl) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.controlsMap == nil {
		m.controlsMap = make(map[string][]types.TaskControl)
	}
	m.controlsMap[taskID] = append(m.controlsMap[taskID], ctrl)
	return true
}

func (m *mockHandoffPosterWithEvents) FailTask(_ context.Context, taskID, _ string, reason []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failCalls = append(m.failCalls, taskID)
	if snap, ok := m.tasks[taskID]; ok {
		snap.Status = types.TaskFailed
		snap.Result = reason
	}
	return nil
}

type mockSubagentController struct {
	mu         sync.Mutex
	steerCalls map[string][]string
	stopCalls  []string
}

func (m *mockSubagentController) TrySteer(sessionID, text string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.steerCalls == nil {
		m.steerCalls = make(map[string][]string)
	}
	m.steerCalls[sessionID] = append(m.steerCalls[sessionID], text)
	return true
}

func (m *mockSubagentController) StopSession(sessionID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopCalls = append(m.stopCalls, sessionID)
	return true
}

// 1. 验证 spawn_agents 并发上限按父智能体当前运行中的子智能体总数计算（R2-14）。
func TestR2_14_SpawnAgents_ConcurrencyLimitRunningTotal(t *testing.T) {
	a := newTestHandoffAgent(t)
	poster := newMockHandoffPoster()
	a.InjectHandoffPoster(poster)

	// standard 档位默认 SubagentMaxConcurrent=6
	// 第一次调用派生 4 个子任务，wait="none"
	tasks1 := make([]types.SpawnTaskSpec, 4)
	for i := range tasks1 {
		tasks1[i] = types.SpawnTaskSpec{Prompt: fmt.Sprintf("task %d", i), Role: "worker"}
	}
	ids1, err := a.SpawnAgents(context.Background(), types.SpawnAgentsArgs{
		Tasks: tasks1,
		Wait:  "none",
	})
	if err != nil {
		t.Fatalf("first SpawnAgents failed: %v", err)
	}
	if len(ids1) != 4 {
		t.Fatalf("expected 4 child IDs, got %d", len(ids1))
	}

	// 第二次调用尝试派生 3 个子任务：4 (running) + 3 = 7 > 6，必须被拦截
	tasks2 := make([]types.SpawnTaskSpec, 3)
	for i := range tasks2 {
		tasks2[i] = types.SpawnTaskSpec{Prompt: fmt.Sprintf("task %d", i+4), Role: "worker"}
	}
	_, err2 := a.SpawnAgents(context.Background(), types.SpawnAgentsArgs{
		Tasks: tasks2,
		Wait:  "none",
	})
	if err2 == nil || !apperr.IsCode(err2, apperr.CodeForbidden) {
		t.Fatalf("expected CodeForbidden for cumulative concurrency limit (4+3 > 6), got: %v", err2)
	}

	// 模拟已完成 2 个子任务
	poster.mu.Lock()
	poster.tasks[ids1[0]].Status = types.TaskDone
	poster.tasks[ids1[1]].Status = types.TaskDone
	poster.mu.Unlock()

	// 此时运行中剩余 2 个，再派生 3 个：2 (running) + 3 = 5 <= 6，应成功
	ids3, err3 := a.SpawnAgents(context.Background(), types.SpawnAgentsArgs{
		Tasks: tasks2,
		Wait:  "none",
	})
	if err3 != nil {
		t.Fatalf("expected success after subtasks completed, got: %v", err3)
	}
	if len(ids3) != 3 {
		t.Fatalf("expected 3 child IDs, got %d", len(ids3))
	}
}

// 2. 验证循环中途 PostTask 失败时，前面已投递的子任务被级联取消并在错误中列出（R2-14）。
func TestR2_14_SpawnAgents_PostTaskFailure_CascadeCancelsSiblings(t *testing.T) {
	a := newTestHandoffAgent(t)
	poster := newMockHandoffPoster()
	poster.postErrOnID = "-1-" // 第二个任务报错
	a.InjectHandoffPoster(poster)

	tasks := []types.SpawnTaskSpec{
		{Prompt: "task 0", Role: "worker"},
		{Prompt: "task 1", Role: "worker"},
		{Prompt: "task 2", Role: "worker"},
	}

	_, err := a.SpawnAgents(context.Background(), types.SpawnAgentsArgs{
		Tasks: tasks,
		Wait:  "none",
	})
	if err == nil {
		t.Fatal("expected error on PostTask failure")
	}

	poster.mu.Lock()
	defer poster.mu.Unlock()

	// 验证第一个任务已被级联取消
	if len(poster.failCalls) == 0 {
		t.Fatal("expected sibling task to be cascade cancelled via FailTask")
	}
	cancelledID := poster.failCalls[0]
	if !strings.Contains(cancelledID, "-0-") {
		t.Fatalf("expected task 0 to be cancelled, got cancelled id: %s", cancelledID)
	}
	if poster.tasks[cancelledID].Status != types.TaskFailed {
		t.Fatalf("expected task 0 status to be failed, got: %v", poster.tasks[cancelledID].Status)
	}

	// 验证返回的错误消息中列出了被取消的子任务
	if !strings.Contains(err.Error(), "sibling tasks cancelled") || !strings.Contains(err.Error(), cancelledID) {
		t.Fatalf("expected error message to list cancelled sibling tasks, got: %v", err)
	}
}

// 3. 验证 AgentMessage 通过 Blackboard 投递 steer 消息至子智能体输入（R2-14）。
func TestR2_14_AgentMessage_SteersSubagent(t *testing.T) {
	a := newTestHandoffAgent(t)
	poster := newMockHandoffPoster()
	ctrl := &mockSubagentController{}
	a.InjectHandoffPoster(poster)
	a.InjectSubagentController(ctrl)

	childID := "subagent-test-steer-target"
	poster.mu.Lock()
	poster.tasks[childID] = &types.TaskSnapshot{ID: childID, Status: types.TaskExecuting}
	poster.mu.Unlock()

	res, err := a.AgentMessage(context.Background(), childID, "注意分析边界条件")
	if err != nil {
		t.Fatalf("AgentMessage failed: %v", err)
	}
	if !strings.Contains(res, "已成功") {
		t.Fatalf("unexpected AgentMessage result: %s", res)
	}

	// 验证 Blackboard 控制通道上发送了 steer 控制指令（R3-04）
	poster.mu.Lock()
	controls := poster.controlsMap[childID]
	poster.mu.Unlock()

	var gotSteerCtrl bool
	for _, ctrlMsg := range controls {
		if ctrlMsg.Kind == types.TaskControlSteer && strings.Contains(ctrlMsg.Text, "边界条件") {
			gotSteerCtrl = true
			break
		}
	}
	if !gotSteerCtrl {
		t.Fatalf("Blackboard 控制通道上未找到类型为 steer 的指令: %+v", controls)
	}

	// 验证 subagent controller（Pool）收到了 TrySteer
	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()
	steers := ctrl.steerCalls["sub-"+childID]
	if len(steers) == 0 {
		steers = ctrl.steerCalls[childID]
	}
	if len(steers) != 1 || steers[0] != "注意分析边界条件" {
		t.Fatalf("subagent controller 未接收到正确的插话内容: %+v", ctrl.steerCalls)
	}
}

// 4. 验证 AgentStop 取消子 Agent 执行并从 Pool 释放（R2-14）。
func TestR2_14_AgentStop_StopsSubagent(t *testing.T) {
	a := newTestHandoffAgent(t)
	poster := newMockHandoffPoster()
	ctrl := &mockSubagentController{}
	a.InjectHandoffPoster(poster)
	a.InjectSubagentController(ctrl)

	childID := "subagent-test-stop-target"
	poster.mu.Lock()
	poster.tasks[childID] = &types.TaskSnapshot{ID: childID, Status: types.TaskExecuting}
	poster.mu.Unlock()

	res, err := a.AgentStop(context.Background(), childID)
	if err != nil {
		t.Fatalf("AgentStop failed: %v", err)
	}
	if !strings.Contains(res, "已成功中止") {
		t.Fatalf("unexpected AgentStop result: %s", res)
	}

	// 验证 Blackboard 任务被标记为 failed
	poster.mu.Lock()
	snap := poster.tasks[childID]
	poster.mu.Unlock()

	if snap.Status != types.TaskFailed {
		t.Fatalf("expected task status failed, got: %v", snap.Status)
	}

	// 验证向 Blackboard 独立控制通道发送了 cancel 控制指令（R3-04）
	var gotCancelCtrl bool
	for _, ctrlMsg := range poster.controlsMap[childID] {
		if ctrlMsg.Kind == types.TaskControlCancel {
			gotCancelCtrl = true
			break
		}
	}
	if !gotCancelCtrl {
		t.Fatalf("Blackboard 控制通道上未找到 cancel 指令: %+v", poster.controlsMap[childID])
	}

	// 验证 subagent controller（Pool）被调用 StopSession 释放
	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()
	var stopped bool
	for _, id := range ctrl.stopCalls {
		if id == "sub-"+childID || id == childID {
			stopped = true
			break
		}
	}
	if !stopped {
		t.Fatalf("subagent controller 未收到 StopSession: %+v", ctrl.stopCalls)
	}
}
