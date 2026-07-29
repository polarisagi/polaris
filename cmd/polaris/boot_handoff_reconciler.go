package main

import (
	"context"
	"log/slog"

	"github.com/polarisagi/polaris/internal/agent"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// recoverAwaitingHandoffs 在启动第一次对外服务前恢复处于 S_AWAIT_AGENT 的会话
// （GD-13-003）。与 recoverCrashedSessions 顺序执行，依赖 AgentPool.Acquire
// 会话级独占语义，二者共用同一条"boot 阶段串行恢复"窗口（见
// boot_crash_recovery.go 文件头注释：全局 protocol.ReplayMode 标志是进程级
// 而非会话级，此窗口内不存在其他并发会话与其读取冲突）。
//
// 启动时先同步执行一次 Reconcile（阻塞到 boot 流程，与 recoverCrashedSessions
// 对齐，保证已知的悬挂会话在对外服务开始前就已经着手恢复），随后把
// reconciler.Run 转入常驻后台 goroutine，以 scanInterval 周期持续扫描——
// 覆盖"AwaitingHandoffReconciler 自身也可能在等待过程中随进程再次重启"的
// 情形，以及"子任务在 boot 阶段那次 Reconcile 尚未完成、稍后才完成"的情形。
func recoverAwaitingHandoffs(ctx context.Context, sb *SubstrateBundle, ab *AgentBundle) {
	if sb == nil || sb.Store == nil || ab == nil || ab.AgentPool == nil || ab.Blackboard == nil {
		return
	}

	reconciler := agent.NewAwaitingHandoffReconciler(
		repo.NewSQLiteTaskCheckpointRepository(sb.Store.DB()),
		ab.AgentPool,
		ab.Blackboard,
	)

	if err := reconciler.Reconcile(ctx); err != nil {
		slog.Warn("polaris: awaiting-handoff reconcile (boot pass) failed", "err", err)
	}

	concurrent.SafeGo(ctx, "awaiting-handoff-reconciler", func(ctx context.Context) {
		reconciler.Run(ctx)
	})
}
