package orchestrator

import (
	"context"
	"fmt"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// MapReduceExecutor 分片归并执行器。
// 架构文档: docs/arch/M08-Multi-Agent-Orchestrator.md §3
// Map: 将父任务按 Scope 拆分后投递至黑板
// Reduce: 收集 Result，去重 Artifacts hash，聚合结果写回。
// 内部收敛为 StateGraphExecutor 的 thin wrapper (GD-13-005)。
type MapReduceExecutor struct {
	bb  *SQLiteBlackboard
	sge *StateGraphExecutor
}

// NewMapReduceExecutor 创建 MapReduceExecutor，totalTimeout 当前由底层统一管理。
func NewMapReduceExecutor(bb *SQLiteBlackboard, totalTimeout time.Duration) *MapReduceExecutor {
	sge := NewStateGraphExecutor(bb)
	sge.PreserveNodeID = true
	return &MapReduceExecutor{
		bb:  bb,
		sge: sge,
	}
}

// Execute 接收已经拆分好的子任务，并行执行后进行 Reduce 收集。
func (mre *MapReduceExecutor) Execute(ctx context.Context, parentTaskID string, subTasks []types.TaskEntry) ([]byte, error) {
	if len(subTasks) == 0 {
		return nil, nil
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

	// 委托执行 Map 任务
	if err := mre.sge.Execute(ctx, parentTaskID, spec); err != nil {
		return nil, err
	}

	// 收集并 Reduce 结果
	var aggregated []byte
	chkRepo := repo.NewSQLiteTaskCheckpointRepository(mre.bb.DB())
	seenHashes := make(map[string]bool)

	for i, t := range subTasks {
		cp, err := chkRepo.GetCheckpoint(ctx, parentTaskID, t.ID, 1) // 假设只访问一次
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "failed to get map result", err)
		}
		if cp == nil || cp.Status != "done" {
			return nil, apperr.New(apperr.CodeInternal, "map task did not complete successfully")
		}

		// 这里复用原本基于 OutputJSON 的 hash 逻辑进行去重。
		// 由于去重在原始代码是按 Payload 整体，现在提取 OutputJSON 模拟。
		hashStr := cp.OutputJSON // 简单去重
		if !seenHashes[hashStr] {
			seenHashes[hashStr] = true
			aggregated = append(aggregated, []byte(fmt.Sprintf("\n--- Result %d ---\n", i))...)
			aggregated = append(aggregated, []byte(cp.OutputJSON)...)
		}
	}

	return aggregated, nil
}

// SetPipelineOrchestrator 注入补偿任务监控器，透传给内部 StateGraphExecutor
// （GD-14-001）。本模式的补偿能力完全来自底座 StateGraphExecutor，
// 不注入时补偿仍会投递，但补偿任务自身失败无人跟进。
func (mr *MapReduceExecutor) SetPipelineOrchestrator(po *PipelineOrchestrator) {
	mr.sge.SetPipelineOrchestrator(po)
}
