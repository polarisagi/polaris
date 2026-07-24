package orchestrator

// StateGraphExecutor 显式状态图编排引擎（编排模式10，GD-8-001）。
//
// 架构文档: docs/arch/M08-Multi-Agent-Orchestrator.md §3-quinquies
// 决策记录: docs/arch/decisions/ADR-0041-state-graph-orchestration.md
//
// 设计动机：PatternDAGExecutor（编排模式9）已将执行控制流从"Blackboard 去中心化
// 认领"收敛为"显式拓扑驱动"，但要求严格无环、静态依赖，无法表达 LangGraph 式
// "条件路由 + 循环反馈"（如验证节点失败 → 路由回执行节点重试，达到上限后终止）。
// StateGraphExecutor 在 PatternDAGExecutor 之上泛化支持：
//  1. 条件边（WorkflowEdgeSpec.Condition）：仅当上游节点输出满足声明式条件才触发；
//  2. 有界循环（WorkflowNodeSpec.MaxVisits）：节点可被重复触达，由硬计数器强制上限；
//  3. 扇入 AND-Join（2026-07-12 workflow DAG 并行接入时补齐）：无条件边（Condition==nil
//     且 From!=To）语义上表达"硬依赖"，多条这类边汇入同一节点时按 AND 语义——等待全部
//     声明的前驱均完成才触发一次，而非任一前驱完成即触发（区别于条件边/自环边的
//     "任一满足即触发"OR 语义，后者用于路由分支与重试循环，语义上不表达"依赖"）。
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
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/graph"
	"github.com/polarisagi/polaris/pkg/types"
)

// StateGraphExecutor 跨 Agent 条件路由 + 有界循环状态图编排引擎。
type StateGraphExecutor struct {
	chkRepo protocol.TaskCheckpointRepository
	bb      *SQLiteBlackboard
}

// NewStateGraphExecutor 创建 StateGraphExecutor。
func NewStateGraphExecutor(bb *SQLiteBlackboard) *StateGraphExecutor {
	return &StateGraphExecutor{
		bb:      bb,
		chkRepo: repo.NewSQLiteTaskCheckpointRepository(bb.DB()),
	}
}

// stateGraphRun 承载单次 Execute 调用的可变运行时状态，将其从局部变量收拢为
// 结构体是为了让 Execute 主循环保持低圈复杂度（各分支下沉到方法中）。
type stateGraphRun struct {
	se              *StateGraphExecutor
	parentTaskID    string
	nodeMap         map[string]protocol.WorkflowNodeSpec
	outEdges        map[string][]protocol.WorkflowEdgeSpec
	maxVisits       map[string]int
	visits          map[string]int
	inFlight        map[string]string // nodeID -> taskID
	requiredPreds   map[string]map[string]bool
	arrivedPreds    map[string]map[string]bool
	totalPosted     int
	syntheticEvents []types.BlackboardEvent
}

// Execute 接收状态图规范并调度执行，直至所有已触发节点完成且无新触发产生，
// 或任一节点失败（Fail-Fast），或 ctx 取消。
func (se *StateGraphExecutor) Execute(ctx context.Context, parentTaskID string, spec protocol.WorkflowGraphSpec) error {
	if len(spec.Nodes) == 0 {
		return nil
	}

	nodeMap, outEdges, maxVisits, requiredPreds, err := se.initializeStateGraph(&spec)
	if err != nil {
		return err
	}

	run := &stateGraphRun{
		se:            se,
		parentTaskID:  parentTaskID,
		nodeMap:       nodeMap,
		outEdges:      outEdges,
		maxVisits:     maxVisits,
		visits:        make(map[string]int),
		inFlight:      make(map[string]string),
		requiredPreds: requiredPreds,
		arrivedPreds:  make(map[string]map[string]bool, len(requiredPreds)),
	}

	events, err := se.bb.Subscribe(ctx)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "failed to subscribe to blackboard", err)
	}

	if err := run.postEntryNodes(ctx, &spec); err != nil {
		return err
	}

	for len(run.inFlight) > 0 || len(run.syntheticEvents) > 0 {
		if len(run.syntheticEvents) > 0 {
			ev := run.syntheticEvents[0]
			run.syntheticEvents = run.syntheticEvents[1:]
			if err := run.handleEvent(ctx, ev); err != nil {
				return err
			}
			continue
		}

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

		// 写入 checkpoint done
		if r.se.chkRepo != nil {
			_ = r.se.chkRepo.UpsertCheckpoint(ctx, types.TaskCheckpointRow{
				TaskID:      r.parentTaskID,
				NodeID:      nodeID,
				Attempt:     r.visits[nodeID],
				Status:      "done",
				OutputJSON:  string(ev.Payload),
				CompletedAt: time.Now().UnixMilli(),
			})
		}

		for _, edge := range r.outEdges[nodeID] {
			if !evalEdgeCondition(edge.Condition, ev.Payload) {
				continue
			}
			if edge.Condition == nil && edge.From != edge.To {
				if !r.arriveJoin(edge.To, edge.From) {
					continue
				}
			}
			if err := r.tryPostNode(ctx, r.nodeMap[edge.To], ev.Payload); err != nil {
				return err
			}
		}
		return nil
	case "task_failed":
		// 写入 checkpoint failed
		if r.se.chkRepo != nil {
			_ = r.se.chkRepo.UpsertCheckpoint(ctx, types.TaskCheckpointRow{
				TaskID:      r.parentTaskID,
				NodeID:      nodeID,
				Attempt:     r.visits[nodeID],
				Status:      "failed",
				Error:       string(ev.Payload),
				CompletedAt: time.Now().UnixMilli(),
			})
		}
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("state graph node %s failed: %s", nodeID, string(ev.Payload)))
	default:
		return nil
	}
}

// arriveJoin 记录 fromNode 对 toNode 的一次硬依赖到达，返回该目标节点是否已集齐
// 全部声明的前驱（首次集齐返回 true 触发一次 tryPostNode；此前/此后重复到达均返回
// false，避免重复触发——tryPostNode 自身的 inFlight/visits 门控作为第二重保险）。
func (r *stateGraphRun) arriveJoin(toNode, fromNode string) bool {
	required := r.requiredPreds[toNode]
	if len(required) <= 1 {
		// 单前驱（或无前驱记账，理论上不会出现在此分支）等价于既有立即触发语义。
		return true
	}
	arrived := r.arrivedPreds[toNode]
	if arrived == nil {
		arrived = make(map[string]bool, len(required))
		r.arrivedPreds[toNode] = arrived
	}
	if arrived[fromNode] {
		return false // 同一前驱重复到达（理论上不应发生，防御性去重）
	}
	arrived[fromNode] = true
	return len(arrived) >= len(required)
}

// initializeStateGraph 校验拓扑并构建正向邻接表 + 节点索引 + effectiveMaxVisits +
// 硬依赖前驱集合（AND-Join 记账用）。
func (se *StateGraphExecutor) initializeStateGraph(spec *protocol.WorkflowGraphSpec) (
	map[string]protocol.WorkflowNodeSpec,
	map[string][]protocol.WorkflowEdgeSpec,
	map[string]int,
	map[string]map[string]bool,
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
			return nil, nil, nil, nil, apperr.New(apperr.CodeInvalidInput,
				fmt.Sprintf("state graph node %s: MaxVisits>1 与 Compensation 同时声明不支持（补偿逆序语义未定义）", n.ID))
		}
	}

	edgePairs := make([][2]string, 0, len(spec.Edges))
	outEdges := make(map[string][]protocol.WorkflowEdgeSpec, len(spec.Nodes))
	// requiredPreds：仅统计无条件（Condition==nil）且非自环（From!=To）的"硬依赖"边，
	// 用于 AND-Join 记账（arriveJoin）。条件边/自环边不计入——它们表达路由分支或
	// 重试循环，语义上是"任一满足即触发"，不是"依赖"。
	requiredPreds := make(map[string]map[string]bool, len(spec.Nodes))
	for _, e := range spec.Edges {
		edgePairs = append(edgePairs, [2]string{e.From, e.To})
		outEdges[e.From] = append(outEdges[e.From], e)
		if e.Condition == nil && e.From != e.To {
			if requiredPreds[e.To] == nil {
				requiredPreds[e.To] = make(map[string]bool, 2)
			}
			requiredPreds[e.To][e.From] = true
		}
	}

	if err := graph.ValidateStateGraphTopology(nodes, edgePairs, maxVisits, isEntry); err != nil {
		return nil, nil, nil, nil, apperr.Wrap(apperr.CodeInvalidInput, "invalid state graph topology", err)
	}

	return nodeMap, outEdges, maxVisits, requiredPreds, nil
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

	attempt := r.visits[node.ID] + 1
	if r.se.chkRepo != nil {
		cp, err := r.se.chkRepo.GetCheckpoint(ctx, r.parentTaskID, node.ID, attempt)
		if err != nil {
			slog.Warn("state_graph: failed to read checkpoint", "err", err, "node_id", node.ID)
		}
		if cp != nil && cp.Status == "done" {
			slog.Info("state_graph: skip executed node from checkpoint", "node_id", node.ID, "attempt", attempt)
			r.visits[node.ID]++
			r.totalPosted++

			synTaskID := fmt.Sprintf("synthetic-%s", node.ID)
			r.inFlight[node.ID] = synTaskID
			r.syntheticEvents = append(r.syntheticEvents, types.BlackboardEvent{
				Type:    "task_completed",
				TaskID:  synTaskID,
				Payload: []byte(cp.OutputJSON),
			})
			return nil
		}
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

	// 写入 checkpoint executing (ADR-0076)
	if r.se.chkRepo != nil {
		_ = r.se.chkRepo.UpsertCheckpoint(ctx, types.TaskCheckpointRow{
			TaskID:    r.parentTaskID,
			NodeID:    node.ID,
			Attempt:   attempt,
			Status:    "executing",
			StartedAt: time.Now().UnixMilli(),
		})
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
// HE-Rule-2：仅支持声明式字段比较 + 结构化 And/Or 复合（GD-14-002 复核扩展见
// protocol.EdgeCondition 类型注释），禁止内嵌脚本/表达式引擎（避免"可验证执行"
// 退化为"运行任意代码决定控制流"，ADR-0041 已就引入 CEL 的原始 finding 做出否决，
// 维持不变）。无法解析上游输出或字段缺失时 fail-closed（宁可漏触发下游边，也不在
// 未验证的输出上误触发）。JSON 反序列化本身在 Execute 主循环中每条边仅执行一次，
// 不存在重复解析开销问题。
func evalEdgeCondition(cond *protocol.EdgeCondition, upstreamOutput []byte) bool {
	if cond == nil {
		return true
	}
	if len(cond.And) > 0 {
		for _, sub := range cond.And {
			if !evalEdgeCondition(sub, upstreamOutput) {
				return false
			}
		}
		return true
	}
	if len(cond.Or) > 0 {
		for _, sub := range cond.Or {
			if evalEdgeCondition(sub, upstreamOutput) {
				return true
			}
		}
		return false
	}

	var m map[string]any
	if err := json.Unmarshal(upstreamOutput, &m); err != nil {
		return false
	}
	val, ok := m[cond.Field]
	if cond.Op == protocol.CondExists {
		return ok
	}
	if !ok {
		return false
	}
	valStr := fmt.Sprintf("%v", val)
	switch cond.Op {
	case protocol.CondEquals:
		return valStr == cond.Value
	case protocol.CondNotEquals:
		return valStr != cond.Value
	case protocol.CondContains:
		return strings.Contains(valStr, cond.Value)
	case protocol.CondGreaterThan, protocol.CondLessThan, protocol.CondGreaterOrEqual, protocol.CondLessOrEqual:
		return evalNumericCondition(cond.Op, valStr, cond.Value)
	default:
		return false
	}
}

// evalNumericCondition 对 gt/lt/ge/le 算子做数值比较：两侧均需可解析为 float64，
// 否则 fail-closed（返回 false）——与"字段缺失/JSON 解析失败"同一条 fail-closed
// 原则，避免对非数字字段做隐式类型转换后产生误判。
func evalNumericCondition(op protocol.EdgeConditionOp, valStr, target string) bool {
	got, err1 := strconv.ParseFloat(valStr, 64)
	want, err2 := strconv.ParseFloat(target, 64)
	if err1 != nil || err2 != nil {
		return false
	}
	switch op {
	case protocol.CondGreaterThan:
		return got > want
	case protocol.CondLessThan:
		return got < want
	case protocol.CondGreaterOrEqual:
		return got >= want
	case protocol.CondLessOrEqual:
		return got <= want
	default:
		return false
	}
}
