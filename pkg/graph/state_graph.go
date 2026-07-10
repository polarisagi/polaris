package graph

import (
	"fmt"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// StateGraphMaxTotalVisitBudget 单次状态图执行允许的节点访问总预算硬上限。
// 与 ValidateTopology（严格 DAG，拒绝任何环）不同，StateGraph 允许有界循环
// （节点 MaxVisits > 1），因此改用"总访问预算"作为物理终止保证——HE-Rule-2
// （可验证执行）要求安全边界物理/密码学可验证，故不采用"拓扑分析猜测是否可能
// 死循环"这类概率性判断，而是对最坏情况下的任务投递总量设置硬上限。
const StateGraphMaxTotalVisitBudget = 200

// ValidateStateGraphTopology 状态图拓扑校验（允许环，替代 ValidateTopology 用于
// 编排模式10 StateGraphExecutor）。校验规则：
//  1. 节点数不超过熔断上限（复用 ValidateTopology 的 50 上限）；
//  2. 每条边的 From/To 均引用已声明节点（引用完整性）；
//  3. 至少存在一个合法入口节点——入度为 0，或被显式标记为 isEntry（否则执行无法
//     启动；纯环图若无任何节点显式声明 isEntry 则无合法起点）；
//  4. 所有节点 effectiveMaxVisits（未声明或 <=0 时按 1 处理）之和不超过
//     StateGraphMaxTotalVisitBudget（Tier-0 资源熔断）。
//
// nodes: 节点 ID 列表；edges: [from, to] 对列表；maxVisits: 节点 ID -> 声明的 MaxVisits；
// isEntry: 节点 ID -> 是否显式标记为入口（参与循环反馈的节点入度恒 > 0，
// 仅靠入度分析无法识别，需显式标记，见 WorkflowNodeSpec.IsEntry 注释）。
func ValidateStateGraphTopology(nodes []string, edges [][2]string, maxVisits map[string]int, isEntry map[string]bool) error {
	if len(nodes) > 50 {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("node count %d exceeds circuit-breaker limit 50", len(nodes)))
	}

	nodeSet := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		nodeSet[n] = true
	}

	inDeg := make(map[string]int, len(nodes))
	for _, e := range edges {
		from, to := e[0], e[1]
		if !nodeSet[from] {
			return apperr.New(apperr.CodeInternal, fmt.Sprintf("edge references undeclared node: %s", from))
		}
		if !nodeSet[to] {
			return apperr.New(apperr.CodeInternal, fmt.Sprintf("edge references undeclared node: %s", to))
		}
		inDeg[to]++
	}

	hasEntry := false
	totalBudget := 0
	for _, n := range nodes {
		if inDeg[n] == 0 || isEntry[n] {
			hasEntry = true
		}
		v := maxVisits[n]
		if v <= 0 {
			v = 1
		}
		totalBudget += v
	}
	if len(nodes) > 0 && !hasEntry {
		return apperr.New(apperr.CodeInternal, "state graph has no entry node (every node has an incoming edge; execution cannot start)")
	}
	if totalBudget > StateGraphMaxTotalVisitBudget {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("total visit budget %d exceeds circuit-breaker limit %d", totalBudget, StateGraphMaxTotalVisitBudget))
	}

	return nil
}
