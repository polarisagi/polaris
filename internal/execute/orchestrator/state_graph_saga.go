package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// 跨 Agent Saga 协调（GD-14-001）
//
// StateGraphExecutor 是 Parallel / MapReduce / CSV-Fanout 等并发编排模式的
// 统一底座（各 Executor 都是它的 thin wrapper）。它的 WorkflowNodeSpec 一直
// 声明着 Compensation 字段、校验阶段也一直在检查它（MaxVisits>1 与
// Compensation 互斥），但**从未在任何失败路径上执行过补偿**——节点失败时只是
// 返回错误，已经成功的兄弟节点留下的副作用无人回滚。
//
// 这正是 GD-14-001 描述的缺口，且并发扇出场景比顺序流水线更严重：
//   - 顺序模式（PipelineOrchestrator）已有完整的逆序补偿 + 监控 + ESCALATE；
//   - PatternDAGExecutor 也有 runCompensation；
//   - 唯独并发底座 StateGraphExecutor 没有——而"部分成功部分失败"恰恰是
//     并发扇出的常态，不是边界情况。
//
// 本文件补齐两件事：
//  1. 逆序补偿：按完成的**逆序**为已声明 Compensation 的节点投递补偿任务；
//  2. 在途兄弟取消：并发场景下 A 失败时 B 往往还在跑，若不取消，B 会在补偿
//     跑完之后才产出新的副作用——补偿反而先于副作用发生，回滚失去意义。
// ============================================================================

// compensationEntry 记录一个已完成、且声明了补偿动作的节点。
type compensationEntry struct {
	node   protocol.WorkflowNodeSpec
	taskID string
}

// recordCompensable 在节点成功完成时登记其补偿动作（逆序：后完成的先补偿）。
func (r *stateGraphRun) recordCompensable(nodeID, taskID string) {
	node, ok := r.nodeMap[nodeID]
	if !ok || node.Compensation == nil {
		return
	}
	r.executedUndo = append([]compensationEntry{{node: node, taskID: taskID}}, r.executedUndo...)
}

// abortWithCompensation 在任一节点失败时执行全局回滚协调，返回原始失败错误。
//
// 顺序不可交换：**先取消在途兄弟，再跑补偿**。反过来的话，仍在执行的兄弟节点
// 会在补偿完成之后才写入它的副作用，导致"补偿先于副作用"——回滚等于没做。
func (r *stateGraphRun) abortWithCompensation(ctx context.Context, failedNodeID string, failPayload []byte) {
	cancelled := r.cancelInFlightSiblings(ctx, failedNodeID)
	if cancelled > 0 {
		slog.WarnContext(ctx, "state_graph: cancelled in-flight sibling tasks before compensation",
			"parent_task_id", r.parentTaskID, "failed_node", failedNodeID, "cancelled", cancelled)
	}
	r.runCompensation(ctx, failedNodeID, failPayload)
}

// cancelInFlightSiblings 取消除失败节点外所有仍在执行的兄弟任务，返回取消数量。
//
// 用 Blackboard 的 CancelTask 而非直接改状态：取消需要同时中止 Worker 侧
// 正在运行的 goroutine（bb.cancels），只改 DB 状态不会让它停下来。
func (r *stateGraphRun) cancelInFlightSiblings(ctx context.Context, failedNodeID string) int {
	cancelled := 0
	for nodeID, taskID := range r.inFlight {
		if nodeID == failedNodeID {
			continue
		}
		if err := r.se.bb.CancelTask(ctx, taskID); err != nil {
			// 取消失败不阻断补偿：兄弟任务可能已自然结束，或 Worker 已退出。
			// 但必须留痕——持续失败意味着补偿可能与在途副作用竞争。
			slog.WarnContext(ctx, "state_graph: failed to cancel in-flight sibling, compensation may race with it",
				"parent_task_id", r.parentTaskID, "node_id", nodeID, "task_id", taskID, "err", err)
			continue
		}
		cancelled++
	}
	return cancelled
}

// runCompensation 逆序投递补偿任务，并为每个补偿任务启动结果监控。
//
// 与 PatternDAGExecutor.runCompensation 保持同一形态（投递到黑板 + 独立
// goroutine 监控 + 超时/失败 ESCALATE），复用 PipelineOrchestrator 的
// monitorCompensationTask——三条补偿路径共用同一套"补偿任务本身失败了怎么办"
// 的处置逻辑，不各写一份。
func (r *stateGraphRun) runCompensation(ctx context.Context, failedNodeID string, failPayload []byte) {
	if len(r.executedUndo) == 0 {
		return
	}
	slog.InfoContext(ctx, "state_graph: starting reverse compensation",
		"parent_task_id", r.parentTaskID, "failed_node", failedNodeID, "count", len(r.executedUndo))

	for _, entry := range r.executedUndo {
		node := entry.node
		if node.Compensation == nil {
			continue
		}
		compTaskID := fmt.Sprintf("%s-%s-compensate-%s", r.parentTaskID, node.ID, uuid.NewString()[:8])
		intentPayload, mErr := json.Marshal(map[string]any{
			"parent_task_id":    r.parentTaskID,
			"compensating_node": node.ID,
			"compensated_task":  entry.taskID,
			"failed_node":       failedNodeID,
			"failure":           string(failPayload),
			"args":              string(node.Compensation.Args),
			"compensating":      true,
		})
		if mErr != nil {
			// 构造失败 → 该节点的副作用无法回滚，必须显式告警而非静默跳过。
			slog.ErrorContext(ctx, "state_graph: compensate intent marshal failed, side effect left un-rolled-back",
				"parent_task_id", r.parentTaskID, "node_id", node.ID, "err", mErr)
			continue
		}

		task := &types.TaskEntry{
			ID:          compTaskID,
			Type:        node.Compensation.ToolName,
			Priority:    1,
			Status:      types.TaskPending,
			Intent:      intentPayload,
			IntentTaint: node.Compensation.TaintLevel,
			CreatedAt:   time.Now().UnixMilli(),
			UpdatedAt:   time.Now().UnixMilli(),
		}
		if err := r.se.bb.PostTask(ctx, task); err != nil {
			slog.ErrorContext(ctx, "state_graph: compensate task post failed, side effect left un-rolled-back",
				"parent_task_id", r.parentTaskID, "node_id", node.ID, "err", err)
			continue
		}
		slog.InfoContext(ctx, "state_graph: compensate task posted",
			"parent_task_id", r.parentTaskID, "node_id", node.ID, "type", node.Compensation.ToolName)

		if r.se.pipelineOrch == nil {
			// 未注入监控器时补偿仍会执行，但"补偿任务自身失败"无人处置。
			// 明确告警而非静默——这是数据一致性的最后一道观测。
			slog.WarnContext(ctx, "state_graph: no pipeline orchestrator injected, compensation outcome unmonitored",
				"parent_task_id", r.parentTaskID, "comp_task_id", compTaskID)
			continue
		}
		nodeID := node.ID
		concurrent.SafeGo(ctx, "orchestrator.state_graph_compensate_monitor", func(monCtx context.Context) {
			r.se.pipelineOrch.monitorCompensationTask(monCtx, compTaskID, nodeID, r.parentTaskID)
		})
	}
}
