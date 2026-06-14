// Package swarm — SQLiteBlackboard 实现 protocol.Blackboard（M8 多 Agent 协调）。
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

package swarm

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
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
	db          *sql.DB
	mu          sync.Mutex
	subscribers []chan protocol.BlackboardEvent
	subMu       sync.RWMutex
	cancels     map[string]context.CancelFunc // 记录每个执行中任务的取消函数
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
	return err
}

// NewSQLiteBlackboard 创建 SQLiteBlackboard。
// db 须已完成 WAL 初始化（由 StorageFabric 传入）。
func NewSQLiteBlackboard(db *sql.DB) *SQLiteBlackboard {
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

// PostTask 发布任务到黑板（INSERT OR IGNORE，幂等键保护）。
func (bb *SQLiteBlackboard) PostTask(ctx context.Context, task *protocol.TaskEntry) error {
	// 防止 Custom Agent 无限递归派生（inv_M8_06）
	if task.SpawnDepth > MaxSpawnDepth {
		return perrors.New(perrors.CodeForbidden,
			fmt.Sprintf("blackboard.PostTask: SpawnDepth %d exceeds max %d (inv_M8_06)",
				task.SpawnDepth, MaxSpawnDepth))
	}
	tx, err := bb.db.BeginTx(ctx, nil)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "blackboard.PostTask: begin tx", err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO tasks(task_id, session_id, status, priority, version, created_at, updated_at)
		VALUES(?,?,?,?,0,datetime('now'),datetime('now'))`,
		task.ID, task.Type, statusPending, task.Priority,
	)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "blackboard.PostTask", err)
	}
	// INSERT OR IGNORE：仅首次插入（rows>0）写事件，避免重复幂等 post 产生噪音事件
	if rows, _ := result.RowsAffected(); rows > 0 {
		if err := bb.writeTaskEvent(ctx, tx, "system:blackboard", "task_posted", task.ID); err != nil {
			return perrors.Wrap(perrors.CodeInternal, "blackboard.PostTask: write event", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return perrors.Wrap(perrors.CodeInternal, "blackboard.PostTask: commit", err)
	}
	bb.broadcast(protocol.BlackboardEvent{Type: "task_posted", TaskID: task.ID})
	return nil
}

// PostBatch 原子性地批量发布多个任务到黑板。
func (bb *SQLiteBlackboard) PostBatch(ctx context.Context, tasks []*protocol.TaskEntry) error {
	tx, err := bb.db.BeginTx(ctx, nil)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "blackboard.PostBatch", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR IGNORE INTO tasks(task_id, session_id, status, priority, version, created_at, updated_at)
		VALUES(?,?,?,?,0,datetime('now'),datetime('now'))`)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "blackboard.PostBatch", err)
	}
	defer stmt.Close()

	for _, task := range tasks {
		if task.SpawnDepth > MaxSpawnDepth {
			return perrors.New(perrors.CodeForbidden,
				fmt.Sprintf("blackboard.PostBatch: SpawnDepth %d exceeds max %d (inv_M8_06)",
					task.SpawnDepth, MaxSpawnDepth))
		}
		result, err := stmt.ExecContext(ctx, task.ID, task.Type, statusPending, task.Priority)
		if err != nil {
			return perrors.Wrap(perrors.CodeInternal, "blackboard.PostBatch", err)
		}
		if rows, _ := result.RowsAffected(); rows > 0 {
			if err := bb.writeTaskEvent(ctx, tx, "system:blackboard", "task_posted", task.ID); err != nil {
				return perrors.Wrap(perrors.CodeInternal, "blackboard.PostBatch: write event", err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return perrors.Wrap(perrors.CodeInternal, "blackboard.PostBatch", err)
	}

	for _, task := range tasks {
		bb.broadcast(protocol.BlackboardEvent{
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
		return false, perrors.Wrap(perrors.CodeInternal, "blackboard.ClaimTask: begin tx", err)
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
		return false, perrors.Wrap(perrors.CodeInternal, "blackboard.ClaimTask", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return false, nil // CAS miss：被抢先或任务不存在，Rollback 由 defer 执行
	}
	if err := bb.writeTaskEvent(ctx, tx, "agent:"+agentID, "task_claimed", taskID); err != nil {
		return false, perrors.Wrap(perrors.CodeInternal, "blackboard.ClaimTask: write event", err)
	}
	if err := tx.Commit(); err != nil {
		return false, perrors.Wrap(perrors.CodeInternal, "blackboard.ClaimTask: commit", err)
	}
	bb.broadcast(protocol.BlackboardEvent{Type: "task_claimed", TaskID: taskID, AgentID: agentID})
	return true, nil
}

// StartExecution 将任务从 claimed 推进到 running 状态，表示 Agent 已开始实际执行。
// 需持有认领权（claimed_by == agentID）；幂等：already-running 不报错。
func (bb *SQLiteBlackboard) StartExecution(ctx context.Context, taskID, agentID string) error {
	tx, err := bb.db.BeginTx(ctx, nil)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "blackboard.StartExecution: begin tx", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE tasks
		SET status=?, version=version+1, updated_at=datetime('now')
		WHERE task_id=? AND claimed_by=? AND status=?`,
		statusRunning, taskID, agentID, statusClaimed,
	)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "blackboard.StartExecution", err)
	}
	rows, _ := res.RowsAffected()
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
		return perrors.Wrap(perrors.CodeInternal, "blackboard.StartExecution: write event", err)
	}
	if err := tx.Commit(); err != nil {
		return perrors.Wrap(perrors.CodeInternal, "blackboard.StartExecution: commit", err)
	}
	bb.broadcast(protocol.BlackboardEvent{
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
		return perrors.Wrap(perrors.CodeInternal, "blackboard.CompleteTask: begin tx", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE tasks
		SET status=?, version=version+1, updated_at=datetime('now')
		WHERE task_id=? AND claimed_by=? AND status IN (?,?)`,
		statusDone, taskID, agentID, statusClaimed, statusRunning,
	)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "blackboard.CompleteTask", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrTaskNotOwned
	}
	if err := bb.writeTaskEvent(ctx, tx, "agent:"+agentID, "task_completed", taskID); err != nil {
		return perrors.Wrap(perrors.CodeInternal, "blackboard.CompleteTask: write event", err)
	}
	if err := tx.Commit(); err != nil {
		return perrors.Wrap(perrors.CodeInternal, "blackboard.CompleteTask: commit", err)
	}
	bb.broadcast(protocol.BlackboardEvent{
		Type:    "task_completed",
		TaskID:  taskID,
		AgentID: agentID,
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
		return perrors.Wrap(perrors.CodeInternal, "blackboard.FailTask: begin tx", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE tasks
		SET status=?, version=version+1, updated_at=datetime('now')
		WHERE task_id=? AND claimed_by=? AND status IN (?,?)`,
		statusFailed, taskID, agentID, statusClaimed, statusRunning,
	)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "blackboard.FailTask", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrTaskNotOwned
	}
	if err := bb.writeTaskEvent(ctx, tx, "agent:"+agentID, "task_failed", taskID); err != nil {
		return perrors.Wrap(perrors.CodeInternal, "blackboard.FailTask: write event", err)
	}
	if err := tx.Commit(); err != nil {
		return perrors.Wrap(perrors.CodeInternal, "blackboard.FailTask: commit", err)
	}
	bb.broadcast(protocol.BlackboardEvent{
		Type:    "task_failed",
		TaskID:  taskID,
		AgentID: agentID,
		Payload: errBytes,
	})
	return nil
}

// RenewLease 续约（重置 expires_at = now + DefaultLeaseTTL）。
func (bb *SQLiteBlackboard) RenewLease(ctx context.Context, taskID, agentID string) error {
	expiresAt := time.Now().Add(DefaultLeaseTTL).UTC().Format(time.RFC3339)
	res, err := bb.db.ExecContext(ctx, `
		UPDATE tasks
		SET expires_at=?, updated_at=datetime('now'), version=version+1
		WHERE task_id=? AND claimed_by=? AND status=?`,
		expiresAt, taskID, agentID, statusClaimed,
	)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "blackboard.RenewLease", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrStaleBlackboardLease
	}
	return nil
}

// SuspendForHITL 将 Executing 的任务挂起（HITL超时戳覆盖ExpiresAt）。
func (bb *SQLiteBlackboard) SuspendForHITL(ctx context.Context, taskID, agentID string, timeout int64) error {
	tx, err := bb.db.BeginTx(ctx, nil)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "blackboard.SuspendForHITL: begin tx", err)
	}
	defer func() { _ = tx.Rollback() }()

	expiresAt := time.Unix(timeout, 0).UTC().Format(time.RFC3339)
	res, err := tx.ExecContext(ctx, `
		UPDATE tasks
		SET status=?, expires_at=?, updated_at=datetime('now'), version=version+1
		WHERE task_id=? AND claimed_by=? AND status=?`,
		statusSuspended, expiresAt, taskID, agentID, statusRunning,
	)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "blackboard.SuspendForHITL", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrTaskNotOwned
	}
	if err := bb.writeTaskEvent(ctx, tx, "agent:"+agentID, "task_suspended_hitl", taskID); err != nil {
		return perrors.Wrap(perrors.CodeInternal, "blackboard.SuspendForHITL: write event", err)
	}
	if err := tx.Commit(); err != nil {
		return perrors.Wrap(perrors.CodeInternal, "blackboard.SuspendForHITL: commit", err)
	}
	bb.broadcast(protocol.BlackboardEvent{Type: "task_suspended_hitl", TaskID: taskID, AgentID: agentID})
	return nil
}

// ResumeFromHITL 恢复被挂起的任务（!approved → Failed）。
func (bb *SQLiteBlackboard) ResumeFromHITL(ctx context.Context, taskID, agentID string, approved bool) error {
	tx, err := bb.db.BeginTx(ctx, nil)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "blackboard.ResumeFromHITL: begin tx", err)
	}
	defer func() { _ = tx.Rollback() }()

	newStatus := statusRunning
	if !approved {
		newStatus = statusFailed
	}

	expiresAt := time.Now().Add(DefaultLeaseTTL).UTC().Format(time.RFC3339)
	res, err := tx.ExecContext(ctx, `
		UPDATE tasks
		SET status=?, expires_at=?, updated_at=datetime('now'), version=version+1
		WHERE task_id=? AND claimed_by=? AND status=?`,
		newStatus, expiresAt, taskID, agentID, statusSuspended,
	)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "blackboard.ResumeFromHITL", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrTaskNotOwned
	}
	evType := "task_resumed_hitl_approved"
	if !approved {
		evType = "task_resumed_hitl_rejected"
	}
	if err := bb.writeTaskEvent(ctx, tx, "agent:"+agentID, evType, taskID); err != nil {
		return perrors.Wrap(perrors.CodeInternal, "blackboard.ResumeFromHITL: write event", err)
	}
	if err := tx.Commit(); err != nil {
		return perrors.Wrap(perrors.CodeInternal, "blackboard.ResumeFromHITL: commit", err)
	}
	bb.broadcast(protocol.BlackboardEvent{Type: evType, TaskID: taskID, AgentID: agentID})
	return nil
}

// BeginCompensation 开始补偿链（状态改为 compensating，提供 300s 时间预算）。
func (bb *SQLiteBlackboard) BeginCompensation(ctx context.Context, taskID, agentID string) error {
	expiresAt := time.Now().Add(300 * time.Second).UTC().Format(time.RFC3339)

	res, err := bb.db.ExecContext(ctx, `
		UPDATE tasks
		SET status='compensating', expires_at=?, updated_at=datetime('now'), version=version+1
		WHERE task_id=? AND claimed_by=? AND status=?`,
		expiresAt, taskID, agentID, statusRunning,
	)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "blackboard.BeginCompensation", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrTaskNotOwned
	}
	return nil
}

// EndCompensation 补偿完成（状态改为 failed，进入正常回收）。
func (bb *SQLiteBlackboard) EndCompensation(ctx context.Context, taskID, agentID string) error {
	res, err := bb.db.ExecContext(ctx, `
		UPDATE tasks
		SET status=?, updated_at=datetime('now'), version=version+1
		WHERE task_id=? AND claimed_by=? AND status='compensating'`,
		statusFailed, taskID, agentID,
	)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "blackboard.EndCompensation", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrTaskNotOwned
	}
	return nil
}

// SideEffectPreCheck TOCTOU 校验。
func (bb *SQLiteBlackboard) SideEffectPreCheck(ctx context.Context, taskID, agentID string, claimedVersion int32) error {
	var status string
	var claimedBy sql.NullString
	var expiresAtStr string
	var version int32

	err := bb.db.QueryRowContext(ctx, `
		SELECT status, claimed_by, expires_at, version FROM tasks WHERE task_id=?`,
		taskID,
	).Scan(&status, &claimedBy, &expiresAtStr, &version)

	if err != nil {
		if err == sql.ErrNoRows {
			return ErrTaskNotOwned // using existing error
		}
		return perrors.Wrap(perrors.CodeInternal, "blackboard.SideEffectPreCheck", err)
	}

	if !claimedBy.Valid || claimedBy.String != agentID {
		return ErrStaleBlackboardLease
	}

	if version != claimedVersion {
		return ErrStaleBlackboardLease
	}

	if status != statusRunning {
		return ErrStaleBlackboardLease
	}

	expiresAt, _ := time.Parse(time.RFC3339, expiresAtStr)
	if time.Now().UTC().After(expiresAt) {
		return ErrStaleBlackboardLease
	}

	return nil
}

// PeekTask 只读快照提取。
func (bb *SQLiteBlackboard) PeekTask(ctx context.Context, taskID string) (*protocol.TaskSnapshot, error) {
	var statusStr string
	err := bb.db.QueryRowContext(ctx, "SELECT status FROM tasks WHERE task_id=?", taskID).Scan(&statusStr)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, perrors.Wrap(perrors.CodeInternal, "blackboard.PeekTask", err)
	}

	var status protocol.TaskStatus
	switch statusStr {
	case statusPending:
		status = protocol.TaskPending
	case statusClaimed:
		status = protocol.TaskClaimed
	case statusRunning:
		status = protocol.TaskExecuting
	case statusDone:
		status = protocol.TaskDone
	case statusFailed:
		status = protocol.TaskFailed
	case statusSuspended:
		status = protocol.TaskSuspended
	case "compensating":
		status = protocol.TaskCompensating
	}

	return &protocol.TaskSnapshot{
		ID:     taskID,
		Status: status,
	}, nil
}

// Subscribe 返回事件订阅通道（chan cap=64，背压丢弃策略）。
// 调用方须在 context 取消后不再读取通道。
func (bb *SQLiteBlackboard) Subscribe(ctx context.Context) (<-chan protocol.BlackboardEvent, error) {
	ch := make(chan protocol.BlackboardEvent, 64)
	bb.subMu.Lock()
	bb.subscribers = append(bb.subscribers, ch)
	bb.subMu.Unlock()

	// ctx 取消时自动注销
	go func() {
		<-ctx.Done()
		bb.subMu.Lock()
		defer bb.subMu.Unlock()
		for i, s := range bb.subscribers {
			if s == ch {
				bb.subscribers = append(bb.subscribers[:i], bb.subscribers[i+1:]...)
				close(ch)
				break
			}
		}
	}()
	return ch, nil
}

// StartReaper 启动 Reaper goroutine，周期扫描过期认领任务 → 回归 Pending。
// 由 StorageFabric.Open() 在启动时调用。
func (bb *SQLiteBlackboard) StartReaper(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(ReaperScanInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				bb.reap(ctx)
			}
		}
	}()
}

// reap 扫描 expires_at 已过期的 claimed 任务。
// 1. 并发调用所有过期任务的 cancel() 触发协程中止。
// 2. 等待 5s 宽限期（供 M7 工具感知 ctx.Done() 并完成清理）。
// 3. 宽限期结束后强制更新 DB：Status=Pending, Version++。
func (bb *SQLiteBlackboard) reap(ctx context.Context) {
	rows, err := bb.db.QueryContext(ctx, `
		SELECT task_id, claimed_by FROM tasks
		WHERE status IN (?,?) AND expires_at < datetime('now')`,
		statusClaimed, statusRunning,
	)
	if err != nil {
		return
	}

	type row struct{ taskID, agentID string }
	var expired []row

	for rows.Next() {
		var r row
		if rows.Scan(&r.taskID, &r.agentID) == nil {
			expired = append(expired, r)
		}
	}
	rows.Close()

	if len(expired) == 0 {
		return
	}

	bb.mu.Lock()
	for _, r := range expired {
		if cancel, ok := bb.cancels[r.taskID]; ok && cancel != nil {
			cancel()
			delete(bb.cancels, r.taskID)
		}
	}
	bb.mu.Unlock()

	// 宽限期：给 M7 工具的 ctx.Done() 感知路径留出 5s 时间窗口
	select {
	case <-time.After(5 * time.Second):
	case <-ctx.Done():
		return
	}

	// 宽限期结束，强制回写 DB
	for _, r := range expired {
		_, _ = bb.db.ExecContext(ctx, `
			UPDATE tasks
			SET status = CASE WHEN toxicity + 1 >= 3 THEN ? ELSE ? END,
			    claimed_by=NULL, claimed_at=NULL, expires_at=NULL,
			    provider_suspended_count=provider_suspended_count+1,
			    toxicity=toxicity+1,
			    version=version+1, updated_at=datetime('now')
			WHERE task_id=? AND status IN (?,?)`,
			statusFailed, statusPending, r.taskID, statusClaimed, statusRunning,
		)
		bb.broadcast(protocol.BlackboardEvent{
			Type:    "task_lease_expired",
			TaskID:  r.taskID,
			AgentID: r.agentID,
		})
	}
}

// StopAll KillSwitch FullStop 响应：所有 Executing 任务进入 Suspended(oom_evicted)。
func (bb *SQLiteBlackboard) StopAll(ctx context.Context, reason string) error {
	_, err := bb.db.ExecContext(ctx, `
		UPDATE tasks
		SET status=?, suspend_reason=?, version=version+1, updated_at=datetime('now')
		WHERE status IN (?, ?)`,
		statusSuspended, reason, statusClaimed, statusRunning,
	)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "blackboard.StopAll", err)
	}
	bb.broadcast(protocol.BlackboardEvent{Type: "killswitch_stopall", Payload: []byte(reason)})
	return nil
}

// UpdateTaskTokens 写入任务的 token 消耗（Gap-A, HE-Rule-1）。
// SQL 覆盖写入（幂等），任务不存在时静默成功（已被 Reaper 清理）。
func (bb *SQLiteBlackboard) UpdateTaskTokens(ctx context.Context, taskID string, tokensIn, tokensOut, cacheRead int, costUSD float64) error {
	_, err := bb.db.ExecContext(ctx, `
		UPDATE tasks
		SET tokens_input=?, tokens_output=?, tokens_cache_read=?, cost_usd=?, updated_at=datetime('now')
		WHERE task_id=?`,
		tokensIn, tokensOut, cacheRead, costUSD, taskID,
	)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "blackboard.UpdateTaskTokens", err)
	}
	return nil
}

// broadcast 广播事件到所有订阅通道（非阻塞，背压丢弃）。
func (bb *SQLiteBlackboard) broadcast(ev protocol.BlackboardEvent) {
	bb.subMu.RLock()
	defer bb.subMu.RUnlock()
	for _, ch := range bb.subscribers {
		select {
		case ch <- ev:
		default:
			// 背压丢弃：消费者太慢时丢弃最新事件（保护 blackboard 不被阻塞）
		}
	}
}

// ─── 错误类型 ────────────────────────────────────────────────────────────────

var (
	ErrTaskNotOwned         = perrors.New(perrors.CodeInternal, "blackboard: task not owned by this agent or in wrong state")
	ErrStaleBlackboardLease = perrors.New(perrors.CodeInternal, "blackboard: lease expired or task not claimed by this agent")
)

// ResumeFromSuspended 将 suspended 任务重置为 pending 以便重新调度（幂等）。
func (bb *SQLiteBlackboard) ResumeFromSuspended(ctx context.Context, taskID string) error {
	res, err := bb.db.ExecContext(ctx, `
		UPDATE tasks SET status=?, claimed_by=NULL, claimed_at=NULL,
			expires_at=NULL, version=version+1, updated_at=datetime('now')
		WHERE task_id=? AND status=?`,
		statusPending, taskID, statusSuspended)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "ResumeFromSuspended", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return perrors.New(perrors.CodeNotFound, "ResumeFromSuspended: no suspended task "+taskID)
	}
	return nil
}
