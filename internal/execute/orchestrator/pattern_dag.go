package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/graph"
	"github.com/polarisagi/polaris/pkg/types"
)

// PatternDAGExecutor 跨 Agent 强类型 DAG 编排引擎。
// 架构文档: docs/arch/M08-Multi-Agent-Orchestrator.md §3
type PatternDAGExecutor struct {
	bb           *SQLiteBlackboard
	pipelineOrch *PipelineOrchestrator // 复用 pipeline 的 compensation 逻辑
}

func NewPatternDAGExecutor(bb *SQLiteBlackboard, po *PipelineOrchestrator) *PatternDAGExecutor {
	return &PatternDAGExecutor{
		bb:           bb,
		pipelineOrch: po,
	}
}

// Execute 接收图规范并调度执行。
func (pe *PatternDAGExecutor) Execute(ctx context.Context, parentTaskID string, spec protocol.WorkflowGraphSpec) error {
	if len(spec.Nodes) == 0 {
		return nil
	}

	adjDependsOn, nodeMap, err := pe.initializePatternDAG(&spec)
	if err != nil {
		return err
	}

	// 2. 初始化状态
	completed := make(map[string][]byte) // nodeID -> output
	inFlight := make(map[string]string)  // nodeID -> taskID
	executedUndo := make([]protocol.WorkflowNodeSpec, 0)

	events, err := pe.bb.Subscribe(ctx)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "failed to subscribe to blackboard", err)
	}

	if err := pe.postReadyNodes(ctx, &spec, adjDependsOn, completed, inFlight, parentTaskID); err != nil {
		return err
	}

	// 3. 事件循环
	for len(completed) < len(spec.Nodes) {
		select {
		case <-ctx.Done():
			return apperr.Wrap(apperr.CodeInternal, "pattern_dag canceled", ctx.Err())
		case ev, ok := <-events:
			if !ok {
				return apperr.New(apperr.CodeInternal, "blackboard events channel closed")
			}
			matchedNodeID := findMatchedNodeID(ev.TaskID, inFlight)
			if matchedNodeID == "" {
				continue
			}

			switch ev.Type {
			case "task_completed":
				delete(inFlight, matchedNodeID)
				completed[matchedNodeID] = ev.Payload
				node := nodeMap[matchedNodeID]
				if node.Compensation != nil {
					executedUndo = append([]protocol.WorkflowNodeSpec{node}, executedUndo...)
				}

				if err := pe.postReadyNodes(ctx, &spec, adjDependsOn, completed, inFlight, parentTaskID); err != nil {
					return err
				}
			case "task_failed":
				pe.runCompensation(ctx, parentTaskID, executedUndo)
				return apperr.New(apperr.CodeInternal, fmt.Sprintf("dag node %s failed: %s", matchedNodeID, string(ev.Payload)))
			}
		}
	}

	return nil
}

func findMatchedNodeID(evTaskID string, inFlight map[string]string) string {
	for nodeID, taskID := range inFlight {
		if evTaskID == taskID {
			return nodeID
		}
	}
	return ""
}

func (pe *PatternDAGExecutor) initializePatternDAG(spec *protocol.WorkflowGraphSpec) (map[string][]string, map[string]protocol.WorkflowNodeSpec, error) {
	nodes := make([]string, 0, len(spec.Nodes))
	adjDependsOn := make(map[string][]string)

	for _, n := range spec.Nodes {
		nodes = append(nodes, n.ID)
	}
	for _, edge := range spec.Edges {
		adjDependsOn[edge.To] = append(adjDependsOn[edge.To], edge.From)
	}

	if err := graph.ValidateTopology(nodes, adjDependsOn); err != nil {
		return nil, nil, apperr.Wrap(apperr.CodeInvalidInput, "invalid dag topology", err)
	}

	nodeMap := make(map[string]protocol.WorkflowNodeSpec)
	for _, n := range spec.Nodes {
		nodeMap[n.ID] = n
	}

	return adjDependsOn, nodeMap, nil
}

func (pe *PatternDAGExecutor) postReadyNodes(
	ctx context.Context,
	spec *protocol.WorkflowGraphSpec,
	adjDependsOn map[string][]string,
	completed map[string][]byte,
	inFlight map[string]string,
	parentTaskID string,
) error {
	ready := findReadyWorkflowNodes(spec, adjDependsOn, completed, inFlight)
	for _, node := range ready {
		taskID := fmt.Sprintf("%s-%s-%s", parentTaskID, node.ID, uuid.NewString()[:8])
		inFlight[node.ID] = taskID

		intentData := map[string]any{
			"dag_node_id": node.ID,
			"template":    node.IntentTemplate,
		}
		upstreamOutputs := make(map[string]string)
		for _, dep := range adjDependsOn[node.ID] {
			upstreamOutputs[dep] = string(completed[dep])
		}
		intentData["upstream_outputs"] = upstreamOutputs
		intentBytes, _ := json.Marshal(intentData)

		task := &types.TaskEntry{
			ID:          taskID,
			Type:        node.CapabilityType,
			Priority:    1,
			Status:      types.TaskPending,
			Intent:      intentBytes,
			IntentTaint: types.TaintMedium,
			CreatedAt:   time.Now().UnixMilli(),
			UpdatedAt:   time.Now().UnixMilli(),
		}

		if err := pe.bb.PostTask(ctx, task); err != nil {
			return err
		}
		slog.Info("pattern_dag: posted task", "node_id", node.ID, "task_id", taskID)
	}
	return nil
}

func findReadyWorkflowNodes(
	spec *protocol.WorkflowGraphSpec,
	adjDependsOn map[string][]string,
	completed map[string][]byte,
	inFlight map[string]string,
) []protocol.WorkflowNodeSpec {
	var ready []protocol.WorkflowNodeSpec
	for _, node := range spec.Nodes {
		if _, done := completed[node.ID]; done {
			continue
		}
		if _, flight := inFlight[node.ID]; flight {
			continue
		}

		allReady := true
		for _, dep := range adjDependsOn[node.ID] {
			if _, done := completed[dep]; !done {
				allReady = false
				break
			}
		}
		if allReady {
			ready = append(ready, node)
		}
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].ID < ready[j].ID })
	return ready
}

func (pe *PatternDAGExecutor) runCompensation(ctx context.Context, parentTaskID string, undos []protocol.WorkflowNodeSpec) {
	if len(undos) == 0 {
		return
	}
	slog.Info("pattern_dag: starting reverse compensation", "count", len(undos))

	for _, node := range undos {
		if node.Compensation == nil {
			continue
		}
		taskID := fmt.Sprintf("%s-%s-compensate-%s", parentTaskID, node.ID, uuid.NewString()[:8])
		intentPayload, _ := json.Marshal(map[string]any{
			"compensating_node": node.ID,
			"args":              string(node.Compensation.Args),
		})

		task := &types.TaskEntry{
			ID:          taskID,
			Type:        node.Compensation.ToolName,
			Priority:    1,
			Status:      types.TaskPending,
			Intent:      intentPayload,
			IntentTaint: node.Compensation.TaintLevel,
			CreatedAt:   time.Now().UnixMilli(),
			UpdatedAt:   time.Now().UnixMilli(),
		}

		if err := pe.bb.PostTask(ctx, task); err != nil {
			slog.Warn("pattern_dag: compensate task post failed", "node", node.ID, "err", err)
			continue
		}

		if pe.pipelineOrch != nil {
			concurrent.SafeGo(ctx, "swarm.dag_compensate_monitor", func(monCtx context.Context) {
				pe.pipelineOrch.monitorCompensationTask(monCtx, taskID, node.ID, parentTaskID)
			})
		}
	}
}
