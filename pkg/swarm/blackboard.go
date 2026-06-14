package swarm

import (
	"context"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
)

// Blackboard 协调黑板的内存实现。
type Blackboard struct {
	mu    sync.RWMutex
	tasks map[string]*protocol.TaskEntry
	// taskVersions 每次状态变更时递增，供 SideEffectPreCheck 做 ABA 防护。
	// 语义与 SQLiteBlackboard 中 tasks.version 列对齐（ADR-0019 §TOCTOU）。
	taskVersions map[string]int32
	events       chan protocol.BlackboardEvent
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
		tasks:        make(map[string]*protocol.TaskEntry),
		taskVersions: make(map[string]int32),
		events:       make(chan protocol.BlackboardEvent, 1024),
		agents:       make(map[string]*AgentHandle),
	}
}

var _ protocol.Blackboard = (*Blackboard)(nil)

func (b *Blackboard) PostTask(ctx context.Context, entry *protocol.TaskEntry) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if entry.SpawnDepth > MaxSpawnDepth {
		return ErrSpawnDepthExceeded
	}

	b.checkBackpressureLocked()
	if b.backpressure {
		return ErrBackpressure
	}

	now := time.Now().Unix()
	entry.CreatedAt = now
	entry.UpdatedAt = now
	entry.Status = protocol.TaskPending

	b.tasks[entry.ID] = entry
	select {
	case b.events <- protocol.BlackboardEvent{
		Type:      "task_posted",
		TaskID:    entry.ID,
		Timestamp: now,
	}:
	default:
	}
	return nil
}

func (b *Blackboard) PostBatch(ctx context.Context, entries []*protocol.TaskEntry) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, task := range entries {
		if task.SpawnDepth > MaxSpawnDepth {
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
		entry.Status = protocol.TaskPending
		b.tasks[entry.ID] = entry
		select {
		case b.events <- protocol.BlackboardEvent{
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
	if entry.Status != protocol.TaskClaimed {
		return ErrStaleLease
	}

	entry.Status = protocol.TaskExecuting
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
	entry.Status = protocol.TaskDone
	entry.UpdatedAt = time.Now().Unix()
	b.taskVersions[taskID]++

	// 非阻塞写：channel 满时丢弃事件，与 PostTask/ClaimTask 等方法保持一致，
	// 避免高并发下 channel 满导致持有 mu.Lock 的 goroutine 永久阻塞（P0-1）。
	select {
	case b.events <- protocol.BlackboardEvent{
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
	entry.Status = protocol.TaskFailed
	entry.UpdatedAt = time.Now().Unix()
	b.taskVersions[taskID]++

	// 非阻塞写：同 CompleteTask（P0-1）
	select {
	case b.events <- protocol.BlackboardEvent{
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
	if entry.Status != protocol.TaskPending {
		return false, nil
	}

	entry.ClaimedBy = agentID
	entry.ClaimedAt = now
	entry.ExpiresAt = now + 60
	entry.Status = protocol.TaskClaimed
	b.taskVersions[taskID]++

	// 非阻塞写：同 CompleteTask/FailTask（P0-1）
	select {
	case b.events <- protocol.BlackboardEvent{
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

func (b *Blackboard) SuspendForHITL(ctx context.Context, taskID, agentID string, timeout int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.tasks[taskID]
	if !ok {
		return ErrTaskNotFound
	}
	if entry.ClaimedBy != agentID || entry.Status != protocol.TaskExecuting {
		return ErrStaleLease
	}
	entry.Status = protocol.TaskSuspended
	entry.ExpiresAt = timeout
	entry.UpdatedAt = time.Now().Unix()
	return nil
}

func (b *Blackboard) ResumeFromHITL(ctx context.Context, taskID, agentID string, approved bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.tasks[taskID]
	if !ok {
		return ErrTaskNotFound
	}
	if entry.ClaimedBy != agentID || entry.Status != protocol.TaskSuspended {
		return ErrStaleLease
	}
	if approved {
		entry.Status = protocol.TaskExecuting
	} else {
		entry.Status = protocol.TaskFailed
	}
	entry.ExpiresAt = time.Now().Unix() + 60
	entry.UpdatedAt = time.Now().Unix()
	return nil
}

func (b *Blackboard) BeginCompensation(ctx context.Context, taskID, agentID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.tasks[taskID]
	if !ok {
		return ErrTaskNotFound
	}
	if entry.ClaimedBy != agentID || entry.Status != protocol.TaskExecuting {
		return ErrStaleLease
	}
	entry.Status = protocol.TaskCompensating
	entry.ExpiresAt = time.Now().Unix() + 300
	entry.UpdatedAt = time.Now().Unix()
	return nil
}

func (b *Blackboard) EndCompensation(ctx context.Context, taskID, agentID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.tasks[taskID]
	if !ok {
		return ErrTaskNotFound
	}
	if entry.ClaimedBy != agentID || entry.Status != protocol.TaskCompensating {
		return ErrStaleLease
	}
	entry.Status = protocol.TaskFailed
	entry.UpdatedAt = time.Now().Unix()
	return nil
}

func (b *Blackboard) SideEffectPreCheck(ctx context.Context, taskID, agentID string, claimedVersion int32) error {
	b.mu.RLock()
	defer b.mu.RUnlock()

	entry, ok := b.tasks[taskID]
	if !ok {
		return ErrTaskNotFound
	}

	if entry.ClaimedBy != agentID {
		return ErrStaleLease
	}

	// ABA 防护：校验 claimedVersion 与当前版本是否一致。
	// 若任务在 Claim 后被 Reaper 回收再被其他 Agent 重新 Claim（version 已递增），
	// 此处会拦截，与 SQLiteBlackboard 版本行为对齐（P0-2）。
	if cur := b.taskVersions[taskID]; cur != claimedVersion {
		return ErrStaleLease
	}

	if time.Now().Unix() > entry.ExpiresAt {
		return ErrStaleLease
	}

	if entry.Status != protocol.TaskExecuting {
		return ErrStaleLease
	}

	return nil
}

func (b *Blackboard) PeekTask(ctx context.Context, taskID string) (*protocol.TaskSnapshot, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	entry, ok := b.tasks[taskID]
	if !ok {
		return nil, nil
	}
	return &protocol.TaskSnapshot{
		ID:     entry.ID,
		Status: entry.Status,
		Result: entry.Result,
	}, nil
}

func (b *Blackboard) Subscribe(ctx context.Context) (<-chan protocol.BlackboardEvent, error) {
	// 简单的订阅，直接返回全局 events chan（注意：这与 sqlite_blackboard.go 的独立订阅通道不同，由于它是内存的且最初只有一个 channels，为了兼容旧代码暂留）
	return b.events, nil
}

// UpdateTaskTokens 更新任务的 token 消耗（Gap-A, HE-Rule-1）。
// 内存版：直接更新 TaskEntry 字段，无 DB 写入。
func (b *Blackboard) UpdateTaskTokens(_ context.Context, taskID string, tokensIn, tokensOut, cacheRead int, costUSD float64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.tasks[taskID]
	if !ok {
		return nil // 任务已过期或被 Reaper 清理，静默成功
	}
	entry.TokensInput = tokensIn
	entry.TokensOutput = tokensOut
	entry.TokensCacheRead = cacheRead
	entry.CostUSD = costUSD
	entry.UpdatedAt = time.Now().Unix()
	return nil
}

var (
	ErrTaskNotFound       = &BlackboardError{"task not found"}
	ErrStaleLease         = &BlackboardError{"stale lease"}
	ErrBackpressure       = &BlackboardError{"backpressure active"}
	ErrSpawnDepthExceeded = &BlackboardError{"spawn depth exceeded"}
)

type BlackboardError struct{ msg string }

func (e *BlackboardError) Error() string { return e.msg }

// AgentHandle Agent 句柄。
type AgentHandle struct {
	Card         AgentCard
	Handle       any // 本地 chan 或远程 A2A gRPC
	RegisteredAt int64
	Status       string // active | inactive | unreachable
}

// AgentCard Agent 能力声明（A2A v0.3 兼容）。
type AgentCard struct {
	Name          string
	Version       string
	Description   string
	Skills        []string
	Tools         []string
	Models        []string
	MaxConcurrent int
	TrustLevel    int
	SandboxTier   int
	Endpoint      string
}
