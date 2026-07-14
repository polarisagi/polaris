package orchestrator

// mockBlackboard 是 protocol.Blackboard 的轻量内存测试替身，供本包内不便直接
// 起 SQLite（newMockSQLiteDB）的单元测试使用（default_worker_test.go/
// csv_fanout_test.go）。
//
// 2026-07-14：随中心化 Orchestrator/Worker 一并删除的生产用内存 Blackboard
// （blackboard.go/blackboard_lifecycle.go）与本类型是两回事——那是曾经真实
// 提供 protocol.Blackboard 生产实现、但已被 SQLiteBlackboard 完全取代的死代码
// （HE-6 State-in-DB 反模式，零生产调用点，见 ADR-0050）；本类型是纯测试替身，
// 从未也不应作为生产实现使用。

import (
	"context"
	"sync"

	"github.com/polarisagi/polaris/pkg/types"
)

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
