// Package kernel 实现 M4 Agent Kernel 的 DAG 执行器与 Saga 补偿逻辑。
// 架构文档: docs/arch/M04-Agent-Kernel.md §5.3, §5.4
//
// 核心流程：
//  1. findReadyNodes: DependsOn ⊆ completedSet → 就绪节点（字典序排序）
//  2. errgroup 并发执行就绪节点（Tier 0: 最大并发 4）
//  3. 任意节点失败 → 逆序 Saga Compensation 补偿
//  4. LeaseHeartbeat: 每 15s 续期防 M8 Reaper 误判
//  5. SurpriseIndex > 0.7 → 触发 Dynamic Replanning（局部子图重规划）
package dag

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ─── DAG 数据模型 ────────────────────────────────────────────────────────────
//
// CompensationAction / NodeStatus / ExecNode 权威定义已上移至
// internal/protocol/dag_node.go（M04 §B2：跨模块共享类型须在 internal/protocol/
// 定义，internal/swarm/planner 消费方不再直接 import 本包）。此处仅保留别名。

type CompensationAction = protocol.CompensationAction
type NodeStatus = protocol.NodeStatus
type ExecNode = protocol.ExecNode

const (
	NodePending   = protocol.NodePending
	NodeRunning   = protocol.NodeRunning
	NodeCompleted = protocol.NodeCompleted
	NodeFailed    = protocol.NodeFailed
	NodeSkipped   = protocol.NodeSkipped // 因上游失败而跳过
)

// EdgePolarity 描述 DAG 边的语义（仅 agent/dag 包内使用，无需上移）。
type EdgePolarity int

const (
	EdgeData     EdgePolarity = iota // 数据依赖：上游产出作为下游输入
	EdgeSequence                     // 纯时序约束（无数据传递）
)

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
	NodeID     string
	Output     []byte
	LatencyMs  int64
	Suspended  bool
	Err        error
	TaintLevel types.TaintLevel
	ImageParts []types.ImagePart
}

// ─── DAG Executor ───────────────────────────────────────────────────────────

const (
	tier0MaxConcurrency = 4 // Tier 0 硬限：最大 4 并发（docs/arch/M04 §5.3）
	defaultNodeTimeout  = 60 * time.Second
	leaseHeartbeatBase  = 15 * time.Second
)

// ToolExecutorFn 工具执行函数类型（由 InMemoryToolRegistry.ExecuteTool 提供）。
type ToolExecutorFn func(ctx context.Context, toolName string, args []byte, taintLevel types.TaintLevel) (*types.ToolResult, error)

// LeaseRenewFn 任务续期函数类型（由 SQLiteBlackboard.RenewLease 提供）。
type LeaseRenewFn func(ctx context.Context, taskID, agentID string, ttl time.Duration) error

// DAGExecutor 执行 M4 Micro-DAG。
// 架构文档: docs/arch/M04-Agent-Kernel.md §5.3
type DAGExecutor struct {
	maxConcurrency int
	toolExec       ToolExecutorFn
	leaseRenew     LeaseRenewFn

	// 运行时状态（每次 Execute 调用期间有效）
	mu             sync.Mutex
	completed      map[string][]byte    // nodeID → output（已成功完成节点）
	executedUndo   []CompensationAction // 逆序 Saga 补偿队列（仅有 Compensation 的节点）
	DegradedReplan bool                 // 是否降级重规划
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
		return nil, apperr.Wrap(apperr.CodeInternal, "dag_executor: topology error", err)
	}

	if taskID != "" {
		ctx = context.WithValue(ctx, protocol.CtxTaskIDKey{}, taskID)
	}
	if agentID != "" {
		ctx = context.WithValue(ctx, protocol.CtxAgentIDKey{}, agentID)
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
		//nolint:bare-goroutine // 历史代码暂留，需结合上下文梳理 ctx 传递链路，后续重构替换
		go e.leaseHeartbeat(hbCtx, taskID, agentID)
	}

	return e.runScheduler(ctx, plan)
}

func (e *DAGExecutor) runScheduler(ctx context.Context, plan *DAGPlan) ([]NodeResult, error) {
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
			// 所有节点已调度完毕则正常退出；否则剩余节点永远无法就绪 → 运行时死锁。
			// 注：validateDAGTopology 只排除拓扑环；当工具返回 nil Output 时会
			// 与 in-progress sentinel 混淆，导致下游节点误判依赖未完成。
			e.mu.Lock()
			scheduled := len(e.completed)
			e.mu.Unlock()
			if scheduled < len(plan.Nodes) {
				e.runCompensation(ctx)
				return allResults, apperr.New(apperr.CodeInternal,
					fmt.Sprintf("dag_executor: deadlock — %d/%d nodes stuck, no ready nodes",
						scheduled, len(plan.Nodes)))
			}
			break
		}

		var wg sync.WaitGroup
		for _, node := range ready {
			// 标记为 in-flight（提前加入 completedSet 以防重复调度）
			e.mu.Lock()
			e.completed[node.ID] = nil // nil = in-progress sentinel
			e.mu.Unlock()

			wg.Add(1)
			n := node // 捕获副本
			//nolint:bare-goroutine // 历史代码暂留，需结合上下文梳理 ctx 传递链路，后续重构替换
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
						if errors.Is(result.Err, protocol.ErrAllProvidersFailed) {
							e.pruneDownstream(ctx, n.ID, plan)
							e.DegradedReplan = true
							firstErr = protocol.ErrAllProvidersFailed
						} else {
							firstErr = apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("node %s failed", n.ID), result.Err)
						}
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

// findReadyNodes / executeNode / runCompensation / leaseHeartbeat /
// validateDAGTopology / buildNodeMap / pruneDownstream 见 executor_node.go（R7 拆分）。
