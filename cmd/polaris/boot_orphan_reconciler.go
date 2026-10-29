package main

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// interruptedByRestart 孤儿运行行的统一失败原因，前端与排障均据此识别"重启中断"而非业务失败。
const interruptedByRestart = "interrupted by restart"

// reconcileOrphanRuns 启动期一次性对账（ADR-0104 决策一，HE-6）：把上一进程遗留的 running 行落到终态。
//
// 为什么无需 owner/epoch 列：调用点在单实例锁（acquireRuntime）之后、任何调度器启动之前，
// 此刻库中所有 running 行的持有者进程必已不存在。对账不做任何重跑：工作流步骤是完整 Agent
// 运行，自动重跑可能重复不可逆副作用，续跑由 POST /v1/workflows/runs/{id}/resume 显式触发。
//
// automations/workflows 的 last_run_status 改为 error 但不增加 failure_count、不动 circuit_open：
// 重启不是业务失败，不能推动熔断；而调度查询 `last_run_status != 'running'` 依赖此处解除阻断。
// 七条 UPDATE 同一事务，避免对账中途崩溃留下"run 已中断而父行仍 running"的半态。
func reconcileOrphanRuns(ctx context.Context, db *sql.DB, now time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "orphan reconciler: begin tx", err)
	}
	defer func() { _ = tx.Rollback() }() // Commit 成功后为 no-op

	finishedAt := now.UTC().Format(time.RFC3339)
	nowMs := now.UnixMilli()
	steps := []struct {
		name  string
		query string
		args  []any
	}{
		{"automation_runs",
			`UPDATE automation_runs SET status='interrupted', error_msg=?, finished_at=? WHERE status='running'`,
			[]any{interruptedByRestart, finishedAt}},
		{"automations",
			`UPDATE automations SET last_run_status='error', last_run_error=? WHERE last_run_status='running'`,
			[]any{interruptedByRestart}},
		{"workflow_runs",
			`UPDATE workflow_runs SET status='interrupted', error_msg=?, finished_at=? WHERE status='running'`,
			[]any{interruptedByRestart, finishedAt}},
		{"workflows",
			`UPDATE workflows SET last_run_status='error', last_run_error=? WHERE last_run_status='running'`,
			[]any{interruptedByRestart}},
		{"subagent_runs",
			`UPDATE subagent_runs SET status='interrupted', error=?, finished_at=? WHERE status='running'`,
			[]any{interruptedByRestart, nowMs}},
		{"planner_sessions",
			`UPDATE planner_sessions SET status='failed', completed_at=? WHERE status='running'`,
			[]any{nowMs}},
		// HITL 待审的 waiter channel 随进程消失，遗留 pending 再也无法被裁决，
		// 不置终态就是幽灵待审（Respond 只会得到 "not pending"）。
		{"hitl_requests",
			`UPDATE hitl_requests SET status='orphaned', decided_at=?, reason=? WHERE status='pending'`,
			[]any{nowMs, interruptedByRestart}},
	}
	counts := make([]any, 0, len(steps)*2)
	for _, st := range steps {
		res, err := tx.ExecContext(ctx, st.query, st.args...)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "orphan reconciler: "+st.name, err)
		}
		n, _ := res.RowsAffected()
		counts = append(counts, st.name, n)
	}
	if err := tx.Commit(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "orphan reconciler: commit", err)
	}
	slog.Info("polaris: orphan runs reconciled", counts...)
	return nil
}
