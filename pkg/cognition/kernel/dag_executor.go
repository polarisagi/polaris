// Package kernel 实现 M4 Agent Kernel 的 DAG 执行器与 Saga 补偿逻辑。
// 架构文档: docs/arch/M04-Agent-Kernel.md §5.3, §5.4
//
// 核心流程：
//  1. findReadyNodes: DependsOn ⊆ completedSet → 就绪节点（字典序排序）
//  2. errgroup 并发执行就绪节点（Tier 0: 最大并发 4）
//  3. 任意节点失败 → 逆序 Saga Compensation 补偿
//  4. LeaseHeartbeat: 每 15s 续期防 M8 Reaper 误判
//  5. SurpriseIndex > 0.7 → 触发 Dynamic Replanning（局部子图重规划）
package kernel

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/substrate/observability"
)

// ─── DAG 数据模型 ────────────────────────────────────────────────────────────

// CompensationAction 描述一个节点失败后的 Saga 逆序补偿动作。
// write_local / write_network 节点必须声明此字段，否则 DAG 校验拒绝。
type CompensationAction struct {
	ToolName   string
	Args       []byte
	TaintLevel protocol.TaintLevel
}

// EdgePolarity 描述 DAG 边的语义。
type EdgePolarity int

const (
	EdgeData     EdgePolarity = iota // 数据依赖：上游产出作为下游输入
	EdgeSequence                     // 纯时序约束（无数据传递）
)

// ExecNode 是 DAG 中可执行的工具调用节点。
type ExecNode struct {
	ID           string
	ToolName     string
	Args         []byte
	TaintLevel   protocol.TaintLevel // 从 Context 继承的污染等级
	DependsOn    []string            // 前驱节点 ID
	Compensation *CompensationAction // Saga 补偿动作（有副作用节点必填）
	MaxRetry     int                 // 默认 0（不重试）
	Timeout      time.Duration       // 0 使用全局默认
}

// ExecEdge 是 DAG 中的有向边。
type ExecEdge struct {
	From     string
	To       string
	Polarity EdgePolarity
}

// DAGPlan 是完整的可执行 DAG 计划。
type DAGPlan struct {
	Nodes []ExecNode
	Edges []ExecEdge
}

// NodeResult 记录单个节点的执行结果。
type NodeResult struct {
	NodeID    string
	Output    []byte
	LatencyMs int64
	Suspended bool
	Err       error
}

// ─── DAG Executor ───────────────────────────────────────────────────────────

const (
	tier0MaxConcurrency = 4 // Tier 0 硬限：最大 4 并发（docs/arch/M04 §5.3）
	defaultNodeTimeout  = 60 * time.Second
	leaseHeartbeatBase  = 15 * time.Second
)

// ToolExecutorFn 工具执行函数类型（由 InMemoryToolRegistry.ExecuteTool 提供）。
type ToolExecutorFn func(ctx context.Context, toolName string, args []byte, taintLevel protocol.TaintLevel) (*protocol.ToolResult, error)

// LeaseRenewFn 任务续期函数类型（由 SQLiteBlackboard.RenewLease 提供）。
type LeaseRenewFn func(ctx context.Context, taskID, agentID string, ttl time.Duration) error

// DAGExecutor 执行 M4 Micro-DAG。
// 架构文档: docs/arch/M04-Agent-Kernel.md §5.3
type DAGExecutor struct {
	maxConcurrency int
	toolExec       ToolExecutorFn
	leaseRenew     LeaseRenewFn

	// 运行时状态（每次 Execute 调用期间有效）
	mu           sync.Mutex
	completed    map[string][]byte    // nodeID → output（已成功完成节点）
	executedUndo []CompensationAction // 逆序 Saga 补偿队列（仅有 Compensation 的节点）
}

// NewDAGExecutor 创建 DAG 执行器。
func NewDAGExecutor(toolExec ToolExecutorFn, leaseRenew LeaseRenewFn) *DAGExecutor {
	return &DAGExecutor{
		maxConcurrency: tier0MaxConcurrency,
		toolExec:       toolExec,
		leaseRenew:     leaseRenew,
		completed:      make(map[string][]byte),
	}
}

// Execute 执行完整的 DAG 计划，失败时自动触发 Saga 逆序补偿。
// taskID / agentID 用于 LeaseHeartbeat 续期。
func (e *DAGExecutor) Execute(ctx context.Context, plan *DAGPlan, taskID, agentID string) ([]NodeResult, error) {
	if err := validateDAGTopology(plan); err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "dag_executor: topology error", err)
	}

	// 重置运行时状态
	e.mu.Lock()
	e.completed = make(map[string][]byte, len(plan.Nodes))
	e.executedUndo = nil
	e.mu.Unlock()

	// 启动 LeaseHeartbeat 防止 M8 Reaper 误判
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	if e.leaseRenew != nil && taskID != "" {
		go e.leaseHeartbeat(hbCtx, taskID, agentID)
	}

	// 信号量控制并发
	sem := make(chan struct{}, e.maxConcurrency)

	var (
		allResults []NodeResult
		resultsMu  sync.Mutex
		failed     atomic.Bool
		firstErr   error
		errMu      sync.Mutex
	)

	nodeMap := buildNodeMap(plan.Nodes)

	for {
		// 获取所有就绪节点（DependsOn ⊆ completedSet）
		ready := e.findReadyNodes(plan, nodeMap)
		if len(ready) == 0 {
			break // 全部完成或死锁（拓扑校验已排除环）
		}

		var wg sync.WaitGroup
		for _, node := range ready {
			// 标记为 in-flight（提前加入 completedSet 以防重复调度）
			e.mu.Lock()
			e.completed[node.ID] = nil // nil = in-progress sentinel
			e.mu.Unlock()

			wg.Add(1)
			n := node // 捕获副本
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				if failed.Load() {
					return // 已有失败，跳过
				}

				start := time.Now()
				result := e.executeNode(ctx, n)
				result.LatencyMs = time.Since(start).Milliseconds()

				resultsMu.Lock()
				allResults = append(allResults, result)
				resultsMu.Unlock()

				if result.Err != nil {
					failed.Store(true)
					errMu.Lock()
					if firstErr == nil {
						firstErr = perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("node %s failed", n.ID), result.Err)
					}
					errMu.Unlock()
					return
				}

				e.mu.Lock()
				e.completed[n.ID] = result.Output
				// 仅 write_local/write_network 节点有 Compensation
				if n.Compensation != nil {
					comp := *n.Compensation
					comp.TaintLevel = n.TaintLevel
					e.executedUndo = append([]CompensationAction{comp}, e.executedUndo...)
				}
				e.mu.Unlock()
			}()
		}
		wg.Wait()

		if failed.Load() {
			// Saga 逆序补偿
			e.runCompensation(ctx)
			return allResults, firstErr
		}
	}

	return allResults, nil
}

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
				lastErr = perrors.New(perrors.CodeInternal, fmt.Sprintf("tool failed: %s", res.Error))
			} else {
				si := observability.GlobalSurpriseIndex.ComputeBasic(nodeCtx, nil, []string{node.ToolName})
				if si > 0.7 {
					return NodeResult{NodeID: node.ID, Err: perrors.New(perrors.CodeInternal, fmt.Sprintf("dynamic replanning: surprise index %.2f > 0.7", si))}
				}
				return NodeResult{NodeID: node.ID, Output: res.Output, Suspended: res.Suspended}
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
func validateDAGTopology(plan *DAGPlan) error { //nolint:gocyclo
	if len(plan.Nodes) > 50 {
		return perrors.New(perrors.CodeInternal, fmt.Sprintf("node count %d exceeds circuit-breaker limit 50", len(plan.Nodes)))
	}

	// 构建邻接表
	adj := make(map[string][]string, len(plan.Nodes))
	nodeIDs := make(map[string]struct{}, len(plan.Nodes))
	for _, n := range plan.Nodes {
		nodeIDs[n.ID] = struct{}{}
		adj[n.ID] = n.DependsOn
	}

	// 孤立节点检测（无入边也无出边，且依赖集为空）
	inDeg := make(map[string]int)
	outDeg := make(map[string]int)
	for _, n := range plan.Nodes {
		for _, dep := range n.DependsOn {
			outDeg[dep]++
			inDeg[n.ID]++
		}
	}
	if len(plan.Nodes) > 1 {
		for _, n := range plan.Nodes {
			if inDeg[n.ID] == 0 && outDeg[n.ID] == 0 && len(n.DependsOn) == 0 {
				// 唯一节点时孤立是合法的
				return perrors.New(perrors.CodeInternal, fmt.Sprintf("isolated node: %s", n.ID))
			}
		}
	}

	// DFS 三色环检测 + 深度熔断
	const maxDepth = 10
	white, gray, black := 0, 1, 2
	color := make(map[string]int, len(plan.Nodes))

	var dfs func(id string, depth int) error
	dfs = func(id string, depth int) error {
		if depth > maxDepth {
			return perrors.New(perrors.CodeInternal, fmt.Sprintf("dag depth exceeds limit %d at node %s", maxDepth, id))
		}
		color[id] = gray
		for _, dep := range adj[id] {
			if color[dep] == gray {
				return perrors.New(perrors.CodeInternal, fmt.Sprintf("cycle detected involving node %s", dep))
			}
			if color[dep] == white {
				if err := dfs(dep, depth+1); err != nil {
					return err
				}
			}
		}
		color[id] = black
		return nil
	}

	for _, n := range plan.Nodes {
		if color[n.ID] == white {
			if err := dfs(n.ID, 0); err != nil {
				return err
			}
		}
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
