package orchestrator

// StateGraphExecutor 显式状态图编排引擎（编排模式10，GD-8-001）。
//
// 架构文档: docs/arch/M08-Multi-Agent-Orchestrator.md §3-quinquies
// 决策记录: docs/arch/decisions/ADR-0040-state-graph-orchestration.md
//
// 设计动机：PatternDAGExecutor（编排模式9）已将执行控制流从"Blackboard 去中心化
// 认领"收敛为"显式拓扑驱动"，但要求严格无环、静态依赖，无法表达 LangGraph 式
// "条件路由 + 循环反馈"（如验证节点失败 → 路由回执行节点重试，达到上限后终止）。
// StateGraphExecutor 在 PatternDAGExecutor 之上泛化支持：
//  1. 条件边（WorkflowEdgeSpec.Condition）：仅当上游节点输出满足声明式条件才触发；
//  2. 有界循环（WorkflowNodeSpec.MaxVisits）：节点可被重复触达，由硬计数器强制上限。
//
// 不变量：仍复用 Blackboard 作为持久化任务队列/事件总线（bb.PostTask/bb.Subscribe），
// 不替换、不新增独立状态存储，符合 HE-Rule-6（State-in-DB）与 HE-Rule-3（跨模块走
// 结构化事件而非另起炉灶）。termination 由 pkg/graph.ValidateStateGraphTopology 的
// 总访问预算硬上限保证（HE-Rule-2：物理熔断，而非拓扑分析猜测是否可能死循环）。

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/graph"
	"github.com/polarisagi/polaris/pkg/types"
)

// StateGraphExecutor 跨 Agent 条件路由 + 有界循环状态图编排引擎。
type StateGraphExecutor struct {
	bb *SQLiteBlackboard
}

// NewStateGraphExecutor 创建 StateGraphExecutor。
func NewStateGraphExecutor(bb *SQLiteBlackboard) *StateGraphExecutor {
	return &StateGraphExecutor{bb: bb}
}

// stateGraphRun 承载单次 Execute 调用的可变运行时状态，将其从局部变量收拢为
// 结构体是为了让 Execute 主循环保持低圈复杂度（各分支下沉到方法中）。
type stateGraphRun struct {
	se           *StateGraphExecutor
	parentTaskID string
	nodeMap      map[string]protocol.WorkflowNodeSpec
	outEdges     map[string][]protocol.WorkflowEdgeSpec
	maxVisits    map[string]int
	visits       map[string]int
	inFlight     map[string]string // nodeID -> taskID
	totalPosted  int
}

// Execute 接收状态图规范并调度执行，直至所有已触发节点完成且无新触发产生，
// 或任一节点失败（Fail-Fast），或 ctx 取消。
func (se *StateGraphExecutor) Execute(ctx context.Context, parentTaskID string, spec protocol.WorkflowGraphSpec) error {
	if len(spec.Nodes) == 0 {
		return nil
	}

	nodeMap, outEdges, maxVisits, err := se.initializeStateGraph(&spec)
	if err != nil {
		return err
	}

	run := &stateGraphRun{
		se:           se,
		parentTaskID: parentTaskID,
		nodeMap:      nodeMap,
		outEdges:     outEdges,
		maxVisits:    maxVisits,
		visits:       make(map[string]int),
		inFlight:     make(map[string]string),
	}

	events, err := se.bb.Subscribe(ctx)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "failed to subscribe to blackboard", err)
	}

	if err := run.postEntryNodes(ctx, &spec); err != nil {
		return err
	}

	for len(run.inFlight) > 0 {
		select {
		case <-ctx.Done():
			return apperr.Wrap(apperr.CodeInternal, "state_graph canceled", ctx.Err())
		case ev, ok := <-events:
			if !ok {
				return apperr.New(apperr.CodeInternal, "blackboard events channel closed")
			}
			if err := run.handleEvent(ctx, ev); err != nil {
				return err
			}
		}
	}

	return nil
}

// postEntryNodes 投递所有入口节点：入度为 0，或被显式标记 IsEntry（参与循环
// 反馈的节点入度恒 > 0，仅靠入度分析无法识别，见 WorkflowNodeSpec.IsEntry）。
// 图校验阶段已保证至少存在一个合法入口。
func (r *stateGraphRun) postEntryNodes(ctx context.Context, spec *protocol.WorkflowGraphSpec) error {
	inDeg := make(map[string]int)
	for _, edges := range r.outEdges {
		for _, e := range edges {
			inDeg[e.To]++
		}
	}
	for _, n := range spec.Nodes {
		if inDeg[n.ID] != 0 && !n.IsEntry {
			continue
		}
		if err := r.tryPostNode(ctx, r.nodeMap[n.ID], nil); err != nil {
			return err
		}
	}
	return nil
}

// handleEvent 处理单条 Blackboard 事件：task_completed 触发下游条件边求值与
// (重新)投递，task_failed 立即 Fail-Fast 返回错误。与本次运行无关的事件（未匹配
// 到 inFlight 任务）直接忽略。
func (r *stateGraphRun) handleEvent(ctx context.Context, ev types.BlackboardEvent) error {
	nodeID := findMatchedNodeID(ev.TaskID, r.inFlight)
	if nodeID == "" {
		return nil
	}

	switch ev.Type {
	case "task_completed":
		delete(r.inFlight, nodeID)
		for _, edge := range r.outEdges[nodeID] {
			if !evalEdgeCondition(edge.Condition, ev.Payload) {
				continue
			}
			if err := r.tryPostNode(ctx, r.nodeMap[edge.To], ev.Payload); err != nil {
				return err
			}
		}
		return nil
	case "task_failed":
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("state graph node %s failed: %s", nodeID, string(ev.Payload)))
	default:
		return nil
	}
}

// initializeStateGraph 校验拓扑并构建正向邻接表 + 节点索引 + effectiveMaxVisits。
func (se *StateGraphExecutor) initializeStateGraph(spec *protocol.WorkflowGraphSpec) (
	map[string]protocol.WorkflowNodeSpec,
	map[string][]protocol.WorkflowEdgeSpec,
	map[string]int,
	error,
) {
	nodes := make([]string, 0, len(spec.Nodes))
	nodeMap := make(map[string]protocol.WorkflowNodeSpec, len(spec.Nodes))
	maxVisits := make(map[string]int, len(spec.Nodes))
	isEntry := make(map[string]bool, len(spec.Nodes))

	for _, n := range spec.Nodes {
		nodes = append(nodes, n.ID)
		nodeMap[n.ID] = n
		maxVisits[n.ID] = n.MaxVisits
		isEntry[n.ID] = n.IsEntry
		// 多次执行的节点的 Saga 逆序补偿语义未定义，校验阶段拒绝而非静默忽略
		// Compensation（HE-Rule-2：fail-closed，不做"看起来能跑"的隐式行为）。
		if n.MaxVisits > 1 && n.Compensation != nil {
			return nil, nil, nil, apperr.New(apperr.CodeInvalidInput,
				fmt.Sprintf("state graph node %s: MaxVisits>1 与 Compensation 同时声明不支持（补偿逆序语义未定义）", n.ID))
		}
	}

	edgePairs := make([][2]string, 0, len(spec.Edges))
	outEdges := make(map[string][]protocol.WorkflowEdgeSpec, len(spec.Nodes))
	for _, e := range spec.Edges {
		edgePairs = append(edgePairs, [2]string{e.From, e.To})
		outEdges[e.From] = append(outEdges[e.From], e)
	}

	if err := graph.ValidateStateGraphTopology(nodes, edgePairs, maxVisits, isEntry); err != nil {
		return nil, nil, nil, apperr.Wrap(apperr.CodeInvalidInput, "invalid state graph topology", err)
	}

	return nodeMap, outEdges, maxVisits, nil
}

// tryPostNode 若节点未超过 effectiveMaxVisits 且全局预算未耗尽，则投递一次任务。
// 超出上限不是错误——意味着该分支的循环/路由自然终止（如验证通过后不再回到执行节点）。
func (r *stateGraphRun) tryPostNode(ctx context.Context, node protocol.WorkflowNodeSpec, upstreamOutput []byte) error {
	limit := r.maxVisits[node.ID]
	if limit <= 0 {
		limit = 1
	}
	if r.visits[node.ID] >= limit {
		return nil
	}
	if r.totalPosted >= graph.StateGraphMaxTotalVisitBudget {
		slog.Warn("state_graph: total visit budget exhausted, dropping further triggers",
			"node_id", node.ID, "budget", graph.StateGraphMaxTotalVisitBudget)
		return nil
	}
	if _, alreadyInFlight := r.inFlight[node.ID]; alreadyInFlight {
		return nil
	}

	taskID := fmt.Sprintf("%s-%s-%s", r.parentTaskID, node.ID, uuid.NewString()[:8])
	intentData := map[string]any{
		"state_graph_node_id": node.ID,
		"template":            node.IntentTemplate,
	}
	if upstreamOutput != nil {
		intentData["upstream_output"] = string(upstreamOutput)
	}
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

	if err := r.se.bb.PostTask(ctx, task); err != nil {
		return err
	}
	r.inFlight[node.ID] = taskID
	r.visits[node.ID]++
	r.totalPosted++
	slog.Info("state_graph: posted task", "node_id", node.ID, "task_id", taskID, "visit", r.visits[node.ID])
	return nil
}

// evalEdgeCondition 声明式条件求值。nil = 无条件边，恒真。
// HE-Rule-2：仅支持声明式字段比较，禁止内嵌脚本/表达式引擎（避免"可验证执行"
// 退化为"运行任意代码决定控制流"）。无法解析上游输出或字段缺失时 fail-closed
// （宁可漏触发下游边，也不在未验证的输出上误触发）。
func evalEdgeCondition(cond *protocol.EdgeCondition, upstreamOutput []byte) bool {
	if cond == nil {
		return true
	}
	var m map[string]any
	if err := json.Unmarshal(upstreamOutput, &m); err != nil {
		return false
	}
	val, ok := m[cond.Field]
	if !ok {
		return false
	}
	valStr := fmt.Sprintf("%v", val)
	switch cond.Op {
	case protocol.CondEquals:
		return valStr == cond.Value
	case protocol.CondNotEquals:
		return valStr != cond.Value
	default:
		return false
	}
}
