// Package orchestrator — SQLiteBlackboard 实现 protocol.Blackboard（M8 多 Agent 协调）。
// 架构文档: docs/arch/M08-Multi-Agent-Orchestrator.md §1
//
// 设计约束:
//   - CAS 原子认领: UPDATE tasks SET status='claimed', claimed_by=? WHERE task_id=? AND status='pending'
//   - Reaper goroutine: 每秒扫描过期 Claimed → 回归 Pending（DefaultLeaseTTL=60s）
//   - KillSwitch FullStop: StopAll() → 所有 Executing → Suspended(oom_evicted)
//   - 订阅 fan-out: 每个 Subscribe 调用获得独立 chan，黑板事件广播
//
// 写路径说明:
//   - 直接持有 *sql.DB（MaxOpenConns=1），不经 MutationBus。
//   - CAS 操作（ClaimTask/CompleteTask/FailTask）需要同步确认 RowsAffected，
//     MutationBus 的异步批量模型无法满足此语义，故保留直接写。
//   - 串行化由 sql.DB MaxOpenConns=1 + WAL busy_timeout=5000ms 保证写串行化。
//   - 事务内所有查询必须走 tx.*，禁止在同一 goroutine 内混用 bb.db.*（连接池耗尽死锁）。
//
// 本文件保留 CAS 认领与生命周期推进的核心路径（PostTask~FailTask）；
// 租约续约/TOCTOU 校验/订阅广播/统计等辅助操作见 sqlite_blackboard_ops.go（R7 拆分）。

package orchestrator

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/observability/trace"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

const (
	DefaultLeaseTTL    = 60 * time.Second
	HeartbeatInterval  = 15 * time.Second
	ReaperScanInterval = 1 * time.Second
	MaxSpawnDepth      = 3 // inv_M8_06: 委托链深度 ≤3

	statusPending   = "pending"
	statusClaimed   = "claimed"
	statusRunning   = "running"
	statusDone      = "done"
	statusFailed    = "failed"
	statusSuspended = "suspended"
)

// SQLiteBlackboard 实现 protocol.Blackboard，以 SQLite 为持久化后端。
// 与现有内存 Blackboard 结构共存，此实现为持久化版本。
type SQLiteBlackboard struct {
	db          protocol.BlackboardDB
	registry    *AgentRegistry
	mu          sync.Mutex
	subscribers []chan types.BlackboardEvent
	subMu       sync.RWMutex
	cancels     map[string]context.CancelFunc // 记录每个执行中任务的取消函数
}

// SetRegistry 注入 AgentRegistry 用于校验 SpawnDepth
func (bb *SQLiteBlackboard) SetRegistry(r *AgentRegistry) {
	bb.mu.Lock()
	defer bb.mu.Unlock()
	bb.registry = r
}

var _ protocol.Blackboard = (*SQLiteBlackboard)(nil)

// writeTaskEvent 在给定事务内向 events 表写入任务状态转换事件（inv_M8_02）。
// 直接事务内写入而非经 MutationBus，原因与 CAS 操作相同：需同步确认执行结果。
// payload 为最小 JSON，满足 events 表 NOT NULL 约束，不破坏 hash-chain（M11 audit 可选覆盖）。
func (bb *SQLiteBlackboard) writeTaskEvent(
	ctx context.Context, tx *sql.Tx, actor, evType, taskID string,
) error {
	// id: "bb:<evType>:<taskID>:<UnixNano>" 在单写 SQLite（MaxOpenConns=1）中实际唯一
	id := fmt.Sprintf("bb:%s:%s:%d", evType, taskID, time.Now().UnixNano())
	payload, _ := json.Marshal(map[string]string{"task_id": taskID, "event": evType})
	_, err := tx.ExecContext(ctx, `
		INSERT INTO events (id, topic, actor, type, payload, created_at)
		VALUES (?, 'agent.task', ?, ?, ?, ?)`,
		id, actor, evType, payload, time.Now().UnixMilli(),
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteBlackboard.writeTaskEvent", err)
	}
	return nil
}

// NewSQLiteBlackboard 创建 SQLiteBlackboard。
// db 须已完成 WAL 初始化（由 StorageFabric 传入）；*sql.DB 自动满足 protocol.BlackboardDB。
func NewSQLiteBlackboard(db protocol.BlackboardDB) *SQLiteBlackboard {
	return &SQLiteBlackboard{
		db:      db,
		cancels: make(map[string]context.CancelFunc),
	}
}

// RegisterCancelFunc 注册任务级别的中断函数。
func (bb *SQLiteBlackboard) RegisterCancelFunc(taskID string, cancel context.CancelFunc) {
	bb.mu.Lock()
	defer bb.mu.Unlock()
	if bb.cancels == nil {
		bb.cancels = make(map[string]context.CancelFunc)
	}
	bb.cancels[taskID] = cancel
}

// removeCancelFunc 内部辅助方法，清理取消函数。
func (bb *SQLiteBlackboard) removeCancelFunc(taskID string) {
	if bb.cancels != nil {
		delete(bb.cancels, taskID)
	}
}

// resolveMaxDepth 查询注册的 agent MaxDepth
func (bb *SQLiteBlackboard) resolveMaxDepth(agentName string) int {
	bb.mu.Lock()
	registry := bb.registry
	bb.mu.Unlock()

	if registry != nil {
		registry.mu.RLock()
		entry, ok := registry.agents[agentName]
		registry.mu.RUnlock()
		if ok && entry.Card.MaxDepth > 0 {
			return entry.Card.MaxDepth
		}
	}
	return MaxSpawnDepth // 全局默认值 3
}

// PostTask 发布任务到黑板（INSERT OR IGNORE，幂等键保护）。
func (bb *SQLiteBlackboard) PostTask(ctx context.Context, task *types.TaskEntry) error {
	// 防止 Custom Agent 无限递归派生（inv_M8_06）
	maxDepth := bb.resolveMaxDepth(task.Type)
	if task.SpawnDepth > maxDepth {
		return apperr.New(apperr.CodeForbidden,
			fmt.Sprintf("blackboard.PostTask: SpawnDepth %d exceeds max %d for agent %q",
				task.SpawnDepth, maxDepth, task.Type))
	}
	tx, err := bb.db.BeginTx(ctx, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "blackboard.PostTask: begin tx", err)
	}
	defer func() { _ = tx.Rollback() }()

	// A16: 从 ctx 提取 trace，落盘延续
	span := trace.SpanFromContext(ctx)
	var traceID, spanID string
	if span != nil {
		traceID = span.TraceID
		spanID = span.SpanID
	}

	result, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO tasks(task_id, session_id, status, priority, version, namespace, intent, trace_id, span_id, created_at, updated_at)
		VALUES(?,?,?,?,0,?,?,?,?,datetime('now'),datetime('now'))`,
		task.ID, task.Type, statusPending, task.Priority, task.Namespace, task.Intent, traceID, spanID,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "blackboard.PostTask", err)
	}
	// INSERT OR IGNORE：仅首次插入（rows>0）写事件，避免重复幂等 post 产生噪音事件
	rows, raErr := result.RowsAffected()
	if raErr != nil {
		slog.Warn("blackboard: RowsAffected failed", "err", raErr)
	}
	if rows > 0 {
		if err := bb.writeTaskEvent(ctx, tx, "system:blackboard", "task_posted", task.ID); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "blackboard.PostTask: write event", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "blackboard.PostTask: commit", err)
	}
	bb.broadcast(types.BlackboardEvent{Type: "task_posted", TaskID: task.ID})
	return nil
}

// PostBatch 原子性地批量发布多个任务到黑板。
func (bb *SQLiteBlackboard) PostBatch(ctx context.Context, tasks []*types.TaskEntry) error {
	tx, err := bb.db.BeginTx(ctx, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "blackboard.PostBatch", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR IGNORE INTO tasks(task_id, session_id, status, priority, version, namespace, intent, trace_id, span_id, created_at, updated_at)
		VALUES(?,?,?,?,0,?,?,?,?,datetime('now'),datetime('now'))`)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "blackboard.PostBatch", err)
	}
	defer stmt.Close()

	span := trace.SpanFromContext(ctx)
	var traceID, spanID string
	if span != nil {
		traceID = span.TraceID
		spanID = span.SpanID
	}

	for _, task := range tasks {
		maxDepth := bb.resolveMaxDepth(task.Type)
		if task.SpawnDepth > maxDepth {
			return apperr.New(apperr.CodeForbidden,
				fmt.Sprintf("blackboard.PostBatch: SpawnDepth %d exceeds max %d for agent %q",
					task.SpawnDepth, maxDepth, task.Type))
		}
		result, err := stmt.ExecContext(ctx, task.ID, task.Type, statusPending, task.Priority, task.Namespace, task.Intent, traceID, spanID)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "blackboard.PostBatch", err)
		}
		rows, raErr := result.RowsAffected()
		if raErr != nil {
			slog.Warn("blackboard: RowsAffected failed", "err", raErr)
		}
		if rows > 0 {
			if err := bb.writeTaskEvent(ctx, tx, "system:blackboard", "task_posted", task.ID); err != nil {
				return apperr.Wrap(apperr.CodeInternal, "blackboard.PostBatch: write event", err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "blackboard.PostBatch", err)
	}

	for _, task := range tasks {
		bb.broadcast(types.BlackboardEvent{
			Type:   "task_posted",
			TaskID: task.ID,
		})
	}
	return nil
}

// ClaimTask CAS 原子认领：仅 status=pending 且无 claimed_by 时成功。
// 返回 (true, nil) 表示认领成功；(false, nil) 表示被他人抢先。
func (bb *SQLiteBlackboard) ClaimTask(ctx context.Context, taskID, agentID string) (bool, error) {
	tx, err := bb.db.BeginTx(ctx, nil)
	if err != nil {
		return false, apperr.Wrap(apperr.CodeInternal, "blackboard.ClaimTask: begin tx", err)
	}
	defer func() { _ = tx.Rollback() }()

	expiresAt := time.Now().Add(DefaultLeaseTTL).UTC().Format(time.RFC3339)
	result, err := tx.ExecContext(ctx, `
		UPDATE tasks
		SET status=?, claimed_by=?, claimed_at=datetime('now'), expires_at=?, version=version+1, updated_at=datetime('now')
		WHERE task_id=? AND status=?`,
		statusClaimed, agentID, expiresAt, taskID, statusPending,
	)
	if err != nil {
		return false, apperr.Wrap(apperr.CodeInternal, "blackboard.ClaimTask", err)
	}
	rows, raErr := result.RowsAffected()
	if raErr != nil {
		slog.Warn("blackboard: RowsAffected failed", "err", raErr)
	}
	if rows == 0 {
		return false, nil // CAS miss：被抢先或任务不存在，Rollback 由 defer 执行
	}
	if err := bb.writeTaskEvent(ctx, tx, "agent:"+agentID, "task_claimed", taskID); err != nil {
		return false, apperr.Wrap(apperr.CodeInternal, "blackboard.ClaimTask: write event", err)
	}
	if err := tx.Commit(); err != nil {
		return false, apperr.Wrap(apperr.CodeInternal, "blackboard.ClaimTask: commit", err)
	}
	bb.broadcast(types.BlackboardEvent{Type: "task_claimed", TaskID: taskID, AgentID: agentID})
	return true, nil
}

// StartExecution 将任务从 claimed 推进到 running 状态，表示 Agent 已开始实际执行。
// 需持有认领权（claimed_by == agentID）；幂等：already-running 不报错。
func (bb *SQLiteBlackboard) StartExecution(ctx context.Context, taskID, agentID string) error {
	tx, err := bb.db.BeginTx(ctx, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "blackboard.StartExecution: begin tx", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE tasks
		SET status=?, version=version+1, updated_at=datetime('now')
		WHERE task_id=? AND claimed_by=? AND status=?`,
		statusRunning, taskID, agentID, statusClaimed,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "blackboard.StartExecution", err)
	}
	rows, raErr := res.RowsAffected()
	if raErr != nil {
		slog.Warn("blackboard: RowsAffected failed", "err", raErr)
	}
	if rows == 0 {
		// 可能已是 running（幂等）或未认领（错误）
		var status string
		_ = tx.QueryRowContext(ctx, "SELECT status FROM tasks WHERE task_id=?", taskID).Scan(&status)
		if status != statusRunning {
			return ErrTaskNotOwned
		}
		// already-running 幂等路径：不写事件，直接 Rollback 返回 nil
		return nil
	}
	if err := bb.writeTaskEvent(ctx, tx, "agent:"+agentID, "task_running", taskID); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "blackboard.StartExecution: write event", err)
	}
	if err := tx.Commit(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "blackboard.StartExecution: commit", err)
	}
	bb.broadcast(types.BlackboardEvent{
		Type:    "task_running",
		TaskID:  taskID,
		AgentID: agentID,
	})
	return nil
}

// CompleteTask 将任务标记为完成（须持有认领权）。
func (bb *SQLiteBlackboard) CompleteTask(ctx context.Context, taskID, agentID string, result []byte) error {
	bb.mu.Lock()
	bb.removeCancelFunc(taskID)
	bb.mu.Unlock()

	tx, err := bb.db.BeginTx(ctx, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "blackboard.CompleteTask: begin tx", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE tasks
		SET status=?, version=version+1, updated_at=datetime('now')
		WHERE task_id=? AND claimed_by=? AND status IN (?,?)`,
		statusDone, taskID, agentID, statusClaimed, statusRunning,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "blackboard.CompleteTask", err)
	}
	rows, raErr := res.RowsAffected()
	if raErr != nil {
		slog.Warn("blackboard: RowsAffected failed", "err", raErr)
	}
	if rows == 0 {
		return ErrTaskNotOwned
	}
	if err := bb.writeTaskEvent(ctx, tx, "agent:"+agentID, "task_completed", taskID); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "blackboard.CompleteTask: write event", err)
	}
	if err := tx.Commit(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "blackboard.CompleteTask: commit", err)
	}
	bb.broadcast(types.BlackboardEvent{
		Type:    "task_completed",
		TaskID:  taskID,
		AgentID: agentID,
		// Payload 携带任务结果字节，对齐 FailTask 的 Payload:errBytes 既有模式
		// （此前遗漏，导致 PatternDAGExecutor/StateGraphExecutor 等消费方读到的
		// ev.Payload 恒为空——GD-8-001 StateGraph 条件边求值发现，见 M08 §3-quinquies）。
		// DB 侧不落盘完整 result（tasks 表无独立 result 列，与 FailTask 的 errBytes
		// 处理方式一致），仅通过广播事件传递给订阅方。
		Payload: result,
	})
	return nil
}

// FailTask 将任务标记为失败。
func (bb *SQLiteBlackboard) FailTask(ctx context.Context, taskID, agentID string, errBytes []byte) error {
	bb.mu.Lock()
	bb.removeCancelFunc(taskID)
	bb.mu.Unlock()

	tx, err := bb.db.BeginTx(ctx, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "blackboard.FailTask: begin tx", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE tasks
		SET status=?, version=version+1, updated_at=datetime('now')
		WHERE task_id=? AND claimed_by=? AND status IN (?,?)`,
		statusFailed, taskID, agentID, statusClaimed, statusRunning,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "blackboard.FailTask", err)
	}
	rows, raErr := res.RowsAffected()
	if raErr != nil {
		slog.Warn("blackboard: RowsAffected failed", "err", raErr)
	}
	if rows == 0 {
		return ErrTaskNotOwned
	}
	if err := bb.writeTaskEvent(ctx, tx, "agent:"+agentID, "task_failed", taskID); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "blackboard.FailTask: write event", err)
	}
	if err := tx.Commit(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "blackboard.FailTask: commit", err)
	}
	bb.broadcast(types.BlackboardEvent{
		Type:    "task_failed",
		TaskID:  taskID,
		AgentID: agentID,
		Payload: errBytes,
	})
	return nil
}
