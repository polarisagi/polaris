package orchestrator

import (
	"context"
	"fmt"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// SwarmCoordinator 去中心化 handoff 协调器。
// 架构文档: docs/arch/M08-Multi-Agent-Orchestrator.md §3
// 行为: 初始认领后，若持有者自判不适，则修改 Note 后退回 Pending（Handoff），
// 由其他 Agent 基于 Note 重新评估是否接手。
type SwarmCoordinator struct {
	bb              *SQLiteBlackboard
	maxHandoffDepth int
}

func NewSwarmCoordinator(bb *SQLiteBlackboard) *SwarmCoordinator {
	return &SwarmCoordinator{
		bb:              bb,
		maxHandoffDepth: 3,
	}
}

// Handoff 供当前执行任务的 Agent 调用，将任务退回给黑板，并附带切换意见。
// depth 为当前 handoff 深度，超过 maxHandoffDepth 则强制报错要求 Supervisor 介入。
func (sc *SwarmCoordinator) Handoff(ctx context.Context, taskID, agentID string, handoffNote string, depth int) error {
	if depth >= sc.maxHandoffDepth {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf(
			"handoff limit exceeded (max: %d), task %s requires supervisor intervention",
			sc.maxHandoffDepth, taskID,
		))
	}

	// 利用 FailTask 将任务标记为失败，Payload 携带 HandoffNote；
	// M8 监控线程识别 [HANDOFF_NOTE] 前缀后可将其重新转换为 Pending 并提升优先级。
	payload := []byte(fmt.Sprintf("[HANDOFF_NOTE]: %s", handoffNote))
	if err := sc.bb.FailTask(ctx, taskID, agentID, payload); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "failed to handoff task", err)
	}

	return nil
}
