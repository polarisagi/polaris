package orchestrator

import (
	"context"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// HITL 挂起/恢复、Saga 补偿、ABA 版本校验、只读查询、错误类型、Agent 声明类型
// （R7 拆分自 blackboard.go）。核心任务生命周期方法（PostTask~RenewLease）见
// blackboard.go。
// ============================================================================

func (b *Blackboard) SuspendForHITL(ctx context.Context, taskID, agentID string, timeout int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.tasks[taskID]
	if !ok {
		return ErrTaskNotFound
	}
	if entry.ClaimedBy != agentID || entry.Status != types.TaskExecuting {
		return ErrStaleLease
	}
	entry.Status = types.TaskSuspended
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
	if entry.ClaimedBy != agentID || entry.Status != types.TaskSuspended {
		return ErrStaleLease
	}
	if approved {
		entry.Status = types.TaskExecuting
	} else {
		entry.Status = types.TaskFailed
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
	if entry.ClaimedBy != agentID || entry.Status != types.TaskExecuting {
		return ErrStaleLease
	}
	entry.Status = types.TaskCompensating
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
	if entry.ClaimedBy != agentID || entry.Status != types.TaskCompensating {
		return ErrStaleLease
	}
	entry.Status = types.TaskFailed
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

	if entry.Status != types.TaskExecuting {
		return ErrStaleLease
	}

	return nil
}

func (b *Blackboard) PeekTask(ctx context.Context, taskID string) (*types.TaskSnapshot, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	entry, ok := b.tasks[taskID]
	if !ok {
		return nil, nil
	}
	return &types.TaskSnapshot{
		ID:     entry.ID,
		Status: entry.Status,
		Result: entry.Result,
	}, nil
}

func (b *Blackboard) Subscribe(ctx context.Context) (<-chan types.BlackboardEvent, error) {
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
	MaxDepth      int // 0 表示使用全局 MaxSpawnDepth 默认值
}

// CountByStatus 返回处于任一给定状态的任务数（活跃度信号，只读）。
// 无参时返回 0。
func (b *Blackboard) CountByStatus(statuses ...types.TaskStatus) int {
	if len(statuses) == 0 {
		return 0
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	n := 0
	for _, t := range b.tasks {
		for _, s := range statuses {
			if t.Status == s {
				n++
				break
			}
		}
	}
	return n
}

// MaxActivePriority 返回活跃任务（Claimed/Executing）的最高优先级（0=最高）。
// 无活跃任务返回 3（最低优先级 → weight=0.1 → 认知压力低）。
func (b *Blackboard) MaxActivePriority() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	best := 3
	for _, t := range b.tasks {
		if t.Status == types.TaskClaimed || t.Status == types.TaskExecuting {
			if t.Priority < best {
				best = t.Priority
			}
		}
	}
	return best
}
