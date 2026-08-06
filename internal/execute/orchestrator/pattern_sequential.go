package orchestrator

import (
	"context"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// SequentialExecutor 实现了串行编排模式。
// 架构文档: docs/arch/M08-Multi-Agent-Orchestrator.md §3
// 行为: Task A 的输出将作为 Task B 的输入，依次串联执行。
// 内部收敛为 StateGraphExecutor 的 thin wrapper (GD-13-005)。
type SequentialExecutor struct {
	sge *StateGraphExecutor
}

// NewSequentialExecutor 创建 SequentialExecutor，perTaskTimeout 当前由底层统一管理。
func NewSequentialExecutor(bb *SQLiteBlackboard, perTaskTimeout time.Duration) *SequentialExecutor {
	sge := NewStateGraphExecutor(bb)
	sge.PreserveNodeID = true
	return &SequentialExecutor{
		sge: sge,
	}
}

// Execute 依次投递任务，并等待上一个任务完成再投递下一个。
func (se *SequentialExecutor) Execute(ctx context.Context, parentTaskID string, subTasks []types.TaskEntry) error {
	if len(subTasks) == 0 {
		return nil
	}
	spec := protocol.WorkflowGraphSpec{}
	for i := range subTasks {
		t := &subTasks[i]
		node := protocol.WorkflowNodeSpec{
			ID:             t.ID,
			CapabilityType: t.Type,
			IntentTemplate: string(t.Intent),
			IsEntry:        i == 0,
		}
		spec.Nodes = append(spec.Nodes, node)

		if i > 0 {
			spec.Edges = append(spec.Edges, protocol.WorkflowEdgeSpec{
				From: subTasks[i-1].ID,
				To:   t.ID,
			})
		}
	}
	return se.sge.Execute(ctx, parentTaskID, spec)
}

// SetPipelineOrchestrator 注入补偿任务监控器，透传给内部 StateGraphExecutor
// （GD-14-001）。本模式的补偿能力完全来自底座 StateGraphExecutor，
// 不注入时补偿仍会投递，但补偿任务自身失败无人跟进。
func (se *SequentialExecutor) SetPipelineOrchestrator(po *PipelineOrchestrator) {
	se.sge.SetPipelineOrchestrator(po)
}
