package swarm

import (
	"context"

	"github.com/polarisagi/polaris/pkg/types"
)

// SwarmFacade 多 Agent 协同模块对外统一接口。
//
// 问题背景：
//
//	当前 swarm 包的入口分散（startup.go、orchestrator/orchestrator.go、planner/planner.go）
//	上层代码（agent、gateway）直接持有 *Orchestrator struct，耦合具体实现。
//
// 解决方案：
//   - SwarmFacade 是 swarm 包对外的统一入口
//   - 上层模块（agent、gateway/server）依赖此接口，不直接持有 Orchestrator/Planner
//   - 内部 Blackboard 操作由 facade 封装，外部无感知
//
// @consumer: agent/agent.go, gateway/server/server.go, automation/
// @producer: swarm.SwarmCoordinator（由 cli.go/bootstrap 构造注入）
type SwarmFacade interface {
	// PostTask 向 Blackboard 投递任务（主 Agent 分解子任务时调用）。
	PostTask(ctx context.Context, entry *types.TaskEntry) error

	// PostBatch 批量投递任务（DAG 展开时使用）。
	PostBatch(ctx context.Context, entries []*types.TaskEntry) error

	// Subscribe 订阅 Blackboard 事件流（接收子任务完成/失败通知）。
	// 返回的 channel 在 ctx 取消时自动关闭。
	Subscribe(ctx context.Context) (<-chan types.BlackboardEvent, error)

	// ActiveCount 返回当前活跃（Claimed/Executing）的 Worker 数量。
	ActiveCount() int

	// SpawnPlanner 异步创建规划子 Agent，执行目标分解。
	// 分解结果通过 PostTask 写入 Blackboard，由 Subscribe 通知主 Agent。
	SpawnPlanner(ctx context.Context, goal, taskType, parentTaskID string)

	// Stop 停止所有 Worker，等待 ctx 超时或 Worker 全部退出。
	Stop(ctx context.Context) error
}
