package dag

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/graph"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// ============================================================================
// 单节点执行/重试 + Saga 补偿 + LeaseHeartbeat + L0 拓扑校验 + 图辅助函数
// （R7 拆分自 executor.go）。
// DAG 数据模型 / DAGExecutor 结构体与构造 / Execute / runScheduler 见 executor.go。
// ============================================================================

// findReadyNodes 返回 DependsOn 全部已完成（output != nil）的就绪节点，按 ID 字典序排序。
func (e *DAGExecutor) findReadyNodes(plan *DAGPlan, nodeMap map[string]*ExecNode) []ExecNode {
	e.mu.Lock()
	defer e.mu.Unlock()

	var ready []ExecNode
	for _, node := range plan.Nodes {
		if _, done := e.completed[node.ID]; done {
			// 已调度（in-progress 或 completed）
			continue
		}
		allReady := true
		for _, dep := range node.DependsOn {
			out, exists := e.completed[dep]
			if !exists || out == nil {
				// 前驱未完成或仍在 in-progress
				allReady = false
				break
			}
		}
		if allReady {
			ready = append(ready, node)
		}
	}
	// 字典序确保确定性排序（规约 par_inv_05）
	sort.Slice(ready, func(i, j int) bool { return ready[i].ID < ready[j].ID })
	return ready
}

// executeNode 执行单个节点，含重试逻辑。
func (e *DAGExecutor) executeNode(ctx context.Context, node ExecNode) NodeResult {
	timeout := node.Timeout
	if timeout == 0 {
		timeout = defaultNodeTimeout
	}
	nodeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if node.IdempotencyKey != "" {
		nodeCtx = context.WithValue(nodeCtx, protocol.CtxIdempotencyKey{}, node.IdempotencyKey)
	}

	var lastErr error
	maxAttempts := node.MaxRetry + 1
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			// 指数退避（上限 30s）
			backoff := time.Duration(1<<uint(attempt)) * time.Second
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			select {
			case <-nodeCtx.Done():
				return NodeResult{NodeID: node.ID, Err: nodeCtx.Err()}
			case <-time.After(backoff):
			}
		}

		// 处于重放模式时物理切断外部副作用（工具调用）
		if protocol.IsReplaying() {
			return NodeResult{NodeID: node.ID, Output: []byte(`{"replayed":true}`)}
		}

		// 传递从 LLM 解析并继承的最高级污点 TaintLevel
		res, err := e.toolExec(nodeCtx, node.ToolName, node.Args, node.TaintLevel)
		if err == nil { //nolint:nestif
			if !res.Success {
				lastErr = apperr.New(apperr.CodeInternal, fmt.Sprintf("tool failed: %s", res.Error))
			} else {
				si := metrics.GlobalSurpriseIndex().ComputeBasic(nodeCtx, nil, []string{node.ToolName})
				if si > 0.7 {
					return NodeResult{NodeID: node.ID, Err: apperr.New(apperr.CodeInternal, fmt.Sprintf("dynamic replanning: surprise index %.2f > 0.7", si))}
				}
				// nil Output 与 in-progress sentinel(nil) 冲突，至少写入空切片表示"已完成"。
				out := res.Output
				if out == nil {
					out = []byte{}
				}
				return NodeResult{NodeID: node.ID, Output: out, Suspended: res.Suspended, TaintLevel: res.TaintLevel, ImageParts: res.ImageParts}
			}
		} else {
			lastErr = err
		}
	}
	return NodeResult{NodeID: node.ID, Err: lastErr}
}

// runCompensation 逆序执行 Saga 补偿动作（尽力而为，不阻塞 Cancel）。
// 架构文档: docs/arch/M04-Agent-Kernel.md §5.3 step 5
func (e *DAGExecutor) runCompensation(ctx context.Context) {
	// 使用后台上下文——避免父 ctx 已取消时补偿被跳过
	compCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	e.mu.Lock()
	undos := append([]CompensationAction{}, e.executedUndo...)
	e.mu.Unlock()

	for _, comp := range undos {
		select {
		case <-compCtx.Done():
			return
		default:
		}

		// 处于重放模式时物理切断外部副作用
		if protocol.IsReplaying() {
			continue
		}

		// 补偿失败记录但继续（Saga 尽力补偿原则）
		// 补偿动作继承与原节点相同的污染等级
		if res, err := e.toolExec(compCtx, comp.ToolName, comp.Args, comp.TaintLevel); err != nil || (res != nil && !res.Success) {
			// 写入审计日志：生产环境应通过 EventLog 记录 saga_compensation_failed
			errMsg := ""
			if err != nil {
				errMsg = err.Error()
			} else if res != nil {
				errMsg = res.Error
			}
			slog.Warn("dag_executor: saga compensation failed",
				"tool", comp.ToolName,
				"error", errMsg,
			)
		}
	}
}

// leaseHeartbeat 每 15s 续期 Lease，防 M8 Reaper 误判超时回收。
func (e *DAGExecutor) leaseHeartbeat(ctx context.Context, taskID, agentID string) {
	ticker := time.NewTicker(leaseHeartbeatBase)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			// jitter: ±5s（通过时间偏移模拟）
			_ = e.leaseRenew(ctx, taskID, agentID, 60*time.Second)
		case <-ctx.Done():
			return
		}
	}
}

// ─── 拓扑校验 ────────────────────────────────────────────────────────────────

// validateDAGTopology L0 拓扑校验（<1ms）：节点数熔断、DFS 环检测、深度熔断、孤立节点。
// 架构文档: docs/arch/M04-Agent-Kernel.md §4 "L0 拓扑"
func validateDAGTopology(plan *DAGPlan) error {
	nodes := make([]string, 0, len(plan.Nodes))
	adj := make(map[string][]string, len(plan.Nodes))
	for _, n := range plan.Nodes {
		nodes = append(nodes, n.ID)
		adj[n.ID] = n.DependsOn
	}
	if err := graph.ValidateTopology(nodes, adj); err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "invalid dag topology", err)
	}
	return nil
}

// buildNodeMap 将节点列表转为 ID 索引。
func buildNodeMap(nodes []ExecNode) map[string]*ExecNode {
	m := make(map[string]*ExecNode, len(nodes))
	for i := range nodes {
		m[nodes[i].ID] = &nodes[i]
	}
	return m
}

// pruneDownstream 将 nodeID 的所有可达后继节点（BFS）标记为 NodeSkipped。
func (e *DAGExecutor) pruneDownstream(ctx context.Context, nodeID string, plan *DAGPlan) {
	e.mu.Lock()
	defer e.mu.Unlock()
	visited := map[string]bool{nodeID: true}
	queue := []string{nodeID}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, edge := range plan.Edges {
			if edge.From == cur && !visited[edge.To] {
				visited[edge.To] = true
				queue = append(queue, edge.To)
				for i := range plan.Nodes {
					if plan.Nodes[i].ID == edge.To {
						plan.Nodes[i].Status = NodeSkipped
					}
				}
			}
		}
	}
}
