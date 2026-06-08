package patterns

import (
	"context"
	"fmt"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/pkg/swarm"
)

// SwarmCoordinator 去中心化 handoff 协调器。
// 架构文档: docs/arch/M08-Multi-Agent-Orchestrator.md §3
// 行为: 初始认领后，若持有者自判不适，则修改 Note 后退回 Pending（Handoff），
// 由其他 Agent 基于 Note 重新评估是否接手。
type SwarmCoordinator struct {
	bb              *swarm.SQLiteBlackboard
	maxHandoffDepth int
}

func NewSwarmCoordinator(bb *swarm.SQLiteBlackboard) *SwarmCoordinator {
	return &SwarmCoordinator{
		bb:              bb,
		maxHandoffDepth: 3,
	}
}

// Handoff 供当前执行任务的 Agent 调用，将任务退回给黑板，并附带切换意见。
func (sc *SwarmCoordinator) Handoff(ctx context.Context, taskID, agentID string, handoffNote string, depth int) error {
	if depth >= sc.maxHandoffDepth {
		return perrors.New(perrors.CodeInternal, fmt.Sprintf("handoff limit exceeded (max: %d), task %s requires supervisor intervention", sc.maxHandoffDepth, taskID))
	}

	// 利用 FailTask 或者直接构造一条特定语义的 SQL 操作。这里为了演示，我们将状态改回 Pending
	// 注意：在真实的 SQLiteBlackboard 中需要一个原生的 Handoff API，
	// 此处模拟使用 FailTask 并利用 Payload 携带 HandoffNote
	payload := []byte(fmt.Sprintf("[HANDOFF_NOTE]: %s", handoffNote))
	if err := sc.bb.FailTask(ctx, taskID, agentID, payload); err != nil {
		return perrors.Wrap(perrors.CodeInternal, "failed to handoff task", err)
	}

	// 实际架构中，FailTask 后，M8 监控线程可以根据 Payload 的特征
	// 重新将其转换为 Pending 并赋予更高的优先级，或者直接广播 EventTaskHandoff

	return nil
}
