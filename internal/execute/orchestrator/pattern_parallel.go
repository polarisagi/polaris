package orchestrator

import (
	"context"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// ParallelExecutor 实现了并发编排模式。
// 架构文档: docs/arch/M08-Multi-Agent-Orchestrator.md §3
// 行为: 将多个无依赖的子任务同时投递到黑板，并等待它们全部完成。
// 内部收敛为 StateGraphExecutor 的 thin wrapper (GD-13-005)。
type ParallelExecutor struct {
	sge *StateGraphExecutor
}

func NewParallelExecutor(bb *SQLiteBlackboard) *ParallelExecutor {
	sge := NewStateGraphExecutor(bb)
	sge.PreserveNodeID = true
	return &ParallelExecutor{
		sge: sge,
	}
}

// Execute 批量投递任务并等待它们完成。
func (pe *ParallelExecutor) Execute(ctx context.Context, parentTaskID string, subTasks []types.TaskEntry) error {
	if len(subTasks) == 0 {
		return nil
	}

	spec := protocol.WorkflowGraphSpec{}
	for i := range subTasks {
		t := &subTasks[i]
		spec.Nodes = append(spec.Nodes, protocol.WorkflowNodeSpec{
			ID:             t.ID,
			CapabilityType: t.Type,
			IntentTemplate: string(t.Intent),
			IsEntry:        true,
		})
	}
	return pe.sge.Execute(ctx, parentTaskID, spec)
}
