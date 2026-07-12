package orchestrator

import (
	"context"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// Blackboard 协调黑板的内存实现。
type Blackboard struct {
	mu    sync.RWMutex
	tasks map[string]*types.TaskEntry
	// taskVersions 每次状态变更时递增，供 SideEffectPreCheck 做 ABA 防护。
	// 语义与 SQLiteBlackboard 中 tasks.version 列对齐（ADR-0019 §TOCTOU）。
	taskVersions map[string]int32
	events       chan types.BlackboardEvent
	agents       map[string]*AgentHandle
	backpressure bool
	epoch        int64
}

// GetEpoch 返回当前的 Orchestrator Epoch.
func (b *Blackboard) GetEpoch() int64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.epoch
}

// SetEpoch 设置当前的 Orchestrator Epoch.
func (b *Blackboard) SetEpoch(e int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.epoch = e
}

func (b *Blackboard) checkBackpressureLocked() {
	capEvent := float64(cap(b.events))
	lenEvent := float64(len(b.events))
	if capEvent == 0 {
		return
	}
	if lenEvent > capEvent*0.8 {
		b.backpressure = true
	} else if lenEvent < capEvent*0.5 {
		b.backpressure = false
	}
}

// NewBlackboard creates a new Blackboard.
func NewBlackboard() *Blackboard {
	return &Blackboard{
		tasks:        make(map[string]*types.TaskEntry),
		taskVersions: make(map[string]int32),
		events:       make(chan types.BlackboardEvent, 1024),
		agents:       make(map[string]*AgentHandle),
	}
}

var _ protocol.Blackboard = (*Blackboard)(nil)

func (b *Blackboard) PostTask(ctx context.Context, entry *types.TaskEntry) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	maxDepth := MaxSpawnDepth
	if handle, ok := b.agents[entry.Type]; ok && handle.Card.MaxDepth > 0 {
		maxDepth = handle.Card.MaxDepth
	}
	if entry.SpawnDepth > maxDepth {
		return ErrSpawnDepthExceeded
	}

	b.checkBackpressureLocked()
	if b.backpressure {
		return ErrBackpressure
	}

	now := time.Now().Unix()
	entry.CreatedAt = now
	entry.UpdatedAt = now
	entry.Status = types.TaskPending

	b.tasks[entry.ID] = entry
	select {
	case b.events <- types.BlackboardEvent{
		Type:      "task_posted",
		TaskID:    entry.ID,
		Timestamp: now,
	}:
	default:
	}
	return nil
}

func (b *Blackboard) PostBatch(ctx context.Context, entries []*types.TaskEntry) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, task := range entries {
		maxDepth := MaxSpawnDepth
		if handle, ok := b.agents[task.Type]; ok && handle.Card.MaxDepth > 0 {
			maxDepth = handle.Card.MaxDepth
		}
		if task.SpawnDepth > maxDepth {
			return ErrSpawnDepthExceeded
		}
	}

	b.checkBackpressureLocked()
	if b.backpressure {
		return ErrBackpressure
	}

	now := time.Now().Unix()
	for _, entry := range entries {
		entry.CreatedAt = now
		entry.UpdatedAt = now
		entry.Status = types.TaskPending
		b.tasks[entry.ID] = entry
		select {
		case b.events <- types.BlackboardEvent{
			Type:      "task_posted",
			TaskID:    entry.ID,
			Timestamp: now,
		}:
		default:
		}
	}
	return nil
}

func (b *Blackboard) StartExecution(ctx context.Context, taskID string, agentID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.tasks[taskID]
	if !ok {
		return ErrTaskNotFound
	}
	if entry.ClaimedBy != agentID {
		return ErrStaleLease
	}
	if entry.Status != types.TaskClaimed {
		return ErrStaleLease
	}

	entry.Status = types.TaskExecuting
	entry.UpdatedAt = time.Now().Unix()
	// 状态变更时递增版本，防止 SideEffectPreCheck ABA 误判
	b.taskVersions[taskID]++
	return nil
}

func (b *Blackboard) CompleteTask(ctx context.Context, taskID string, agentID string, result []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.tasks[taskID]
	if !ok {
		return ErrTaskNotFound
	}
	if entry.ClaimedBy != agentID {
		return ErrStaleLease
	}

	entry.Result = result
	entry.Status = types.TaskDone
	entry.UpdatedAt = time.Now().Unix()
	b.taskVersions[taskID]++

	// 非阻塞写：channel 满时丢弃事件，与 PostTask/ClaimTask 等方法保持一致，
	// 避免高并发下 channel 满导致持有 mu.Lock 的 goroutine 永久阻塞（P0-1）。
	select {
	case b.events <- types.BlackboardEvent{
		Type:      "task_completed",
		TaskID:    taskID,
		AgentID:   agentID,
		Payload:   result,
		Timestamp: entry.UpdatedAt,
	}:
	default:
	}
	return nil
}

func (b *Blackboard) FailTask(ctx context.Context, taskID string, agentID string, errBytes []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.tasks[taskID]
	if !ok {
		return ErrTaskNotFound
	}
	if entry.ClaimedBy != agentID {
		return ErrStaleLease
	}

	entry.Result = errBytes
	entry.Status = types.TaskFailed
	entry.UpdatedAt = time.Now().Unix()
	b.taskVersions[taskID]++

	// 非阻塞写：同 CompleteTask（P0-1）
	select {
	case b.events <- types.BlackboardEvent{
		Type:      "task_failed",
		TaskID:    taskID,
		AgentID:   agentID,
		Payload:   errBytes,
		Timestamp: entry.UpdatedAt,
	}:
	default:
	}
	return nil
}

func (b *Blackboard) ClaimTask(ctx context.Context, taskID, agentID string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	entry, ok := b.tasks[taskID]
	if !ok {
		return false, nil
	}

	if entry.ClaimedBy != "" {
		return false, nil
	}

	now := time.Now().Unix()
	if entry.Status != types.TaskPending {
		return false, nil
	}

	entry.ClaimedBy = agentID
	entry.ClaimedAt = now
	entry.ExpiresAt = now + 60
	entry.Status = types.TaskClaimed
	b.taskVersions[taskID]++

	// 非阻塞写：同 CompleteTask/FailTask（P0-1）
	select {
	case b.events <- types.BlackboardEvent{
		Type:      "task_claimed",
		TaskID:    taskID,
		AgentID:   agentID,
		Timestamp: now,
	}:
	default:
	}

	return true, nil
}

func (b *Blackboard) RenewLease(ctx context.Context, taskID, agentID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	entry, ok := b.tasks[taskID]
	if !ok {
		return nil
	}

	if entry.ClaimedBy != agentID {
		return nil
	}

	entry.ExpiresAt = time.Now().Unix() + 60
	return nil
}

// HITL 挂起/恢复、Saga 补偿、ABA 版本校验、只读查询、错误类型、Agent 声明类型
// 见 blackboard_lifecycle.go（R7 拆分）。
