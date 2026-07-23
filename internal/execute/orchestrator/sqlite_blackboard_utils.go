package orchestrator

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// NewSQLiteBlackboard 创建 SQLiteBlackboard。
// db 须已完成 WAL 初始化（由 StorageFabric 传入）；*sql.DB 自动满足 protocol.BlackboardDB。
func NewSQLiteBlackboard(db protocol.BlackboardDB) *SQLiteBlackboard {
	return &SQLiteBlackboard{
		db:      db,
		cancels: make(map[string]context.CancelFunc),
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
