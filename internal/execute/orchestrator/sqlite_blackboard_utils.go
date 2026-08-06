package orchestrator

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// NewSQLiteBlackboard 创建 SQLiteBlackboard。
// db 须已完成 WAL 初始化（由 StorageFabric 传入）；*sql.DB 自动满足 protocol.BlackboardDB。
func NewSQLiteBlackboard(db protocol.BlackboardDB) *SQLiteBlackboard {
	return &SQLiteBlackboard{
		db:               db,
		cancels:          make(map[string]context.CancelFunc),
		taskRetentionTTL: 24 * time.Hour,
	}
}

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

// CancelTask 中止一个在途任务：先触发其 ctx 取消（让 Worker 侧 goroutine 真正
// 停下来），再把 DB 状态置为 failed（GD-14-001 跨 Agent Saga 协调）。
//
// 两步顺序不可交换：只改 DB 状态不会让正在跑的 goroutine 停下，它仍会继续产生
// 副作用；而只 cancel 不改状态，任务会一直挂在 running 直到 Reaper 30 分钟后
// 才判定僵尸——补偿协调等不了那么久。
//
// 任务已终态或 cancel 函数不存在（Worker 已自然退出）时不视为错误：
// 目标状态"这个任务不会再产生副作用"已经达成。
func (bb *SQLiteBlackboard) CancelTask(ctx context.Context, taskID string) error {
	bb.mu.Lock()
	cancel, hasCancel := bb.cancels[taskID]
	if hasCancel {
		delete(bb.cancels, taskID)
	}
	bb.mu.Unlock()

	if hasCancel && cancel != nil {
		cancel()
	}

	// 只更新仍处于非终态的行：已 done/failed 的任务不得被回写覆盖。
	res, err := bb.db.ExecContext(ctx, `
		UPDATE tasks
		SET status=?, error=?, version=version+1, updated_at=datetime('now')
		WHERE task_id=? AND status IN (?, ?, ?)`,
		statusFailed, "cancelled_by_saga_coordinator", taskID,
		statusPending, statusClaimed, statusRunning,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "blackboard.CancelTask", err)
	}
	if affected, _ := res.RowsAffected(); affected > 0 {
		bb.broadcast(types.BlackboardEvent{
			Type:   "task_failed",
			TaskID: taskID,
			Err:    apperr.New(apperr.CodeInternal, "cancelled_by_saga_coordinator"),
		})
	}
	return nil
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
