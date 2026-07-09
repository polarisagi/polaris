package graph

import (
	"fmt"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// ValidateTopology L0 拓扑校验（<1ms）：节点数熔断、DFS 环检测、深度熔断、孤立节点。
// nodes 为所有节点 ID 的列表，adj 为 adjacency list (nodeID -> dependsOnIDs)。
// 注意：adj 表示依赖关系，即边是从当前节点指向其依赖的节点（有向边表示 "depends on"）。
func ValidateTopology(nodes []string, adj map[string][]string) error { //nolint:gocyclo
	if len(nodes) > 50 {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("node count %d exceeds circuit-breaker limit 50", len(nodes)))
	}

	// 孤立节点检测（无入边也无出边，且依赖集为空）
	inDeg := make(map[string]int)
	outDeg := make(map[string]int)
	for _, n := range nodes {
		for _, dep := range adj[n] {
			outDeg[dep]++
			inDeg[n]++
		}
	}
	if len(nodes) > 1 {
		for _, n := range nodes {
			if inDeg[n] == 0 && outDeg[n] == 0 && len(adj[n]) == 0 {
				// 唯一节点时孤立是合法的
				return apperr.New(apperr.CodeInternal, fmt.Sprintf("isolated node: %s", n))
			}
		}
	}

	// DFS 三色环检测 + 深度熔断
	const maxDepth = 10
	white, gray, black := 0, 1, 2
	color := make(map[string]int, len(nodes))

	var dfs func(id string, depth int) error
	dfs = func(id string, depth int) error {
		if depth > maxDepth {
			return apperr.New(apperr.CodeInternal, fmt.Sprintf("dag depth exceeds limit %d at node %s", maxDepth, id))
		}
		color[id] = gray
		for _, dep := range adj[id] {
			if color[dep] == gray {
				return apperr.New(apperr.CodeInternal, fmt.Sprintf("cycle detected involving node %s", dep))
			}
			if color[dep] == white {
				if err := dfs(dep, depth+1); err != nil {
					return apperr.Wrap(apperr.CodeInternal, "ValidateTopology", err)
				}
			}
		}
		color[id] = black
		return nil
	}

	for _, n := range nodes {
		if color[n] == white {
			if err := dfs(n, 0); err != nil {
				return apperr.Wrap(apperr.CodeInternal, "ValidateTopology", err)
			}
		}
	}

	return nil
}
