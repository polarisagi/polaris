package graph

import (
	"strings"
	"testing"
)

// TestValidateTopology_IndependentNodesAllowed 原生并行 tool_calls 生成无边的独立节点
// （agent.toolCallsToDAGJSON），执行器并行调度，是合法 DAG（ADR-0098 决策五）。
func TestValidateTopology_IndependentNodesAllowed(t *testing.T) {
	if err := ValidateTopology([]string{"a", "b"}, map[string][]string{}); err != nil {
		t.Fatalf("独立并行节点应合法: %v", err)
	}
}

// TestValidateTopology_DanglingDependencyRejected 依赖引用未定义节点（LLM 写错 ID）
// 此前 DFS 直接递归不报错，漏检。
func TestValidateTopology_DanglingDependencyRejected(t *testing.T) {
	err := ValidateTopology([]string{"a", "b"}, map[string][]string{"b": {"ghost"}})
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("悬空依赖必须被拒并指明 ID，得到 %v", err)
	}
}
