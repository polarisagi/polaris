package main

import (
	"context"

	"github.com/polarisagi/polaris/internal/agent"
	"github.com/polarisagi/polaris/internal/store/repo"
)

// recoverAwaitingHandoffs 在启动第一次对外服务前恢复处于 S_AWAIT_AGENT 的会话。
// 与 recoverCrashedSessions 阔序执行，依赖 AgentPool.Acquire 会话级独占语义。
//
//nolint:unused
func recoverAwaitingHandoffs(ctx context.Context, sb *SubstrateBundle, ab *AgentBundle) {
	// 接线 AwaitingHandoffReconciler，需要 ab 提供 HandoffPoster (ab.Blackboard)
	_ = agent.NewAwaitingHandoffReconciler(
		repo.NewSQLiteTaskCheckpointRepository(sb.Store.DB()),
		ab.AgentPool,
		ab.Blackboard,
	)
	// 暂时不执行 Reconcile，等待完整接线
}
