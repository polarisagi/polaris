package protocol

import (
	"context"

	"github.com/polarisagi/polaris/pkg/types"
)

// Blackboard 是多 Agent 协调黑板。
// 所有 Agent 间通信走 schema event（禁止 P2P 自然语言），自然语言仅作 payload content。
// 常量: DefaultLeaseTTL=60s, HeartbeatInterval=15s(±5s jitter), ReaperScanInterval=1s。
// 优先级: 0=用户交互, 1=前台辅助, 2=后台优化, 3=Auto-Curriculum。
type Blackboard interface {
	PostTask(ctx context.Context, task *types.TaskEntry) error
	PostBatch(ctx context.Context, tasks []*types.TaskEntry) error
	ClaimTask(ctx context.Context, taskID, agentID string) (bool, error)
	StartExecution(ctx context.Context, taskID, agentID string) error
	CompleteTask(ctx context.Context, taskID, agentID string, result []byte) error
	FailTask(ctx context.Context, taskID, agentID string, errBytes []byte) error
	RenewLease(ctx context.Context, taskID, agentID string) error
	SuspendForHITL(ctx context.Context, taskID, agentID string, timeout int64) error
	ResumeFromHITL(ctx context.Context, taskID, agentID string, approved bool) error
	BeginCompensation(ctx context.Context, taskID, agentID string) error
	EndCompensation(ctx context.Context, taskID, agentID string) error
	SideEffectPreCheck(ctx context.Context, taskID, agentID string, claimedVersion int32) error
	PeekTask(ctx context.Context, taskID string) (*types.TaskSnapshot, error)
	Subscribe(ctx context.Context) (<-chan types.BlackboardEvent, error)
	// UpdateTaskTokens 记录本任务的 token 消耗（Gap-A, HE-Rule-1）。
	// 由 Worker.tryClaimAndExecute 在 AgentKernel.Run 返回后调用。
	// 幂等：多次调用以最后一次写入为准（覆盖，不累加）。
	UpdateTaskTokens(ctx context.Context, taskID string, tokensIn, tokensOut, cacheRead int, costUSD float64) error
	// CountByStatus 返回处于任一给定状态的任务数（活跃度信号，只读）。无参时返回 0。
	CountByStatus(statuses ...types.TaskStatus) int
	// MaxActivePriority 返回活跃任务（Claimed/Executing）的最高优先级（0=最高；无活跃任务返回 3=最低）。
	MaxActivePriority() int
}

// Scheduler 是任务调度器。
// CAS 抢占: UPDATE tasks SET state='running', worker_id=? WHERE id=? AND state='pending'
type Scheduler interface {
	Submit(ctx context.Context, task types.Task) (string, error)
	Get(ctx context.Context, id string) (*types.Task, error)
	Cancel(ctx context.Context, id string) error
	Subscribe(ctx context.Context, taskID string) (<-chan types.TaskEvent, error)
}
