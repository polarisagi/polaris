// Deprecated: DAG 执行器已迁移到 pkg/cognition/kernel/dag_executor.go（含完整 Saga 补偿、LeaseHeartbeat）。
// 此文件保留以兼容旧调用方，新代码应直接使用 kernel.DAGExecutor。
package cognition

import (
	"context"
	"math/rand"
	"sort"
	"time"

	"fmt"

	"golang.org/x/sync/errgroup"

	perrors "github.com/polarisagi/polaris/internal/errors"
)

// NewDAGExecutor 创建 DAG 执行器。
func NewDAGExecutor(nodes []DAGNode, edges []DAGEdge, maxConcurrent int) *DAGExecutor {
	return &DAGExecutor{
		maxConcurrent: maxConcurrent,
		nodes:         nodes,
		edges:         edges,
		completed:     make(map[string]bool),
		results:       make(map[string][]byte),
	}
}

// Execute 执行 DAG 直至全部节点完成或任一节点失败。
// 失败触发 Saga 逆序补偿: 已完成并行节点逆序 Undo。
func (ex *DAGExecutor) Execute(ctx context.Context, runner NodeRunner) error {
	for len(ex.completed) < len(ex.nodes) {
		ready := ex.findReadyNodes()
		if len(ready) == 0 {
			return perrors.New(perrors.CodeInternal, fmt.Sprintf("dag deadlock: no ready nodes, %d/%d completed", len(ex.completed), len(ex.nodes)))
		}

		// 同批按字典序优先
		sort.Slice(ready, func(i, j int) bool {
			return ready[i].ID < ready[j].ID
		})

		sem := make(chan struct{}, ex.maxConcurrent)
		g, gCtx := errgroup.WithContext(ctx)

		for _, node := range ready {
			node := node
			sem <- struct{}{}
			g.Go(func() error {
				defer func() { <-sem }()
				result, err := runner.Run(gCtx, node)
				if err != nil {
					return perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("node %s failed", node.ID), err)
				}
				ex.mu.Lock()
				ex.completed[node.ID] = true
				ex.results[node.ID] = result
				ex.mu.Unlock()
				return nil
			})
		}

		if err := g.Wait(); err != nil {
			// Saga 逆序补偿
			ex.undoCompleted(ctx, runner)
			return err
		}
	}
	return nil
}

func (ex *DAGExecutor) findReadyNodes() []DAGNode {
	depMap := ex.buildDepMap()
	var ready []DAGNode
	for _, node := range ex.nodes {
		if ex.completed[node.ID] {
			continue
		}
		deps, ok := depMap[node.ID]
		if !ok || ex.allCompleted(deps) {
			ready = append(ready, node)
		}
	}
	return ready
}

func (ex *DAGExecutor) buildDepMap() map[string][]string {
	depMap := make(map[string][]string)
	for _, edge := range ex.edges {
		depMap[edge.To] = append(depMap[edge.To], edge.From)
	}
	return depMap
}

func (ex *DAGExecutor) allCompleted(nodeIDs []string) bool {
	for _, id := range nodeIDs {
		if !ex.completed[id] {
			return false
		}
	}
	return true
}

func (ex *DAGExecutor) undoCompleted(ctx context.Context, runner NodeRunner) {
	var completedIDs []string //nolint:prealloc
	for id := range ex.completed {
		completedIDs = append(completedIDs, id)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(completedIDs)))

	for _, id := range completedIDs {
		if err := runner.Undo(ctx, id); err != nil {
			// 补偿失败不阻塞其余补偿——记录审计事件
			_ = err
		}
	}
}

// DynamicReplan 节点输出 SurpriseIndex > 0.7 → 未执行下游子图局部重规划。
// 已成功节点保留（防双重副作用）。若须覆盖已执行节点: 先 Saga 补偿成功后才加入 replan。
func (ex *DAGExecutor) DynamicReplan(ctx context.Context, nodeID string, surpriseIndex float64) ([]DAGNode, []DAGEdge, error) {
	if surpriseIndex <= 0.7 {
		return ex.nodes, ex.edges, nil
	}
	// 收集受影响的未执行下游节点
	affected := ex.findDownstream(nodeID)
	var newNodes []DAGNode
	var newEdges []DAGEdge
	for _, node := range ex.nodes {
		if ex.completed[node.ID] && !contains(affected, node.ID) {
			newNodes = append(newNodes, node)
		}
	}
	return newNodes, newEdges, nil
}

func (ex *DAGExecutor) findDownstream(nodeID string) []string {
	// BFS 收集下游
	var downstream []string
	visited := map[string]bool{nodeID: true}
	queue := []string{nodeID}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, edge := range ex.edges {
			if edge.From == current && !visited[edge.To] {
				visited[edge.To] = true
				downstream = append(downstream, edge.To)
				queue = append(queue, edge.To)
			}
		}
	}
	return downstream
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// LeaseHeartbeat 每 15s(±5s jitter) 续期，防 M8 Reaper 误判超时。
func LeaseHeartbeat(ctx context.Context, renew func() error, done <-chan struct{}) {
	ticker := time.NewTicker(15*time.Second + time.Duration(jitter5s()))
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			_ = renew()
		case <-done:
			return
		case <-ctx.Done():
			return
		}
	}
}

func jitter5s() int64 { return rand.Int63n(10*int64(time.Second)) - 5*int64(time.Second) }
