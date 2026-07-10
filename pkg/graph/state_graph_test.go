package graph

import "testing"

func TestValidateStateGraphTopology_AllowsCycleWithExplicitEntry(t *testing.T) {
	// executor -> verifier -> executor（有界循环），executor 显式标记为入口
	// （纯入度分析无法识别它——它同时是循环边的目标，入度恒 > 0）。
	nodes := []string{"executor", "verifier"}
	edges := [][2]string{
		{"executor", "verifier"},
		{"verifier", "executor"},
	}
	maxVisits := map[string]int{"executor": 3, "verifier": 3}
	isEntry := map[string]bool{"executor": true}

	if err := ValidateStateGraphTopology(nodes, edges, maxVisits, isEntry); err != nil {
		t.Fatalf("expected cycle with explicit entry + bounded MaxVisits to be accepted, got: %v", err)
	}
}

func TestValidateStateGraphTopology_RejectsNoEntryNode(t *testing.T) {
	// 纯环：每个节点都有入边，且都未显式标记 IsEntry，无合法起点
	nodes := []string{"a", "b"}
	edges := [][2]string{
		{"a", "b"},
		{"b", "a"},
	}

	err := ValidateStateGraphTopology(nodes, edges, nil, nil)
	if err == nil {
		t.Fatal("expected error for graph with no entry node")
	}
}

func TestValidateStateGraphTopology_RejectsUndeclaredNodeReference(t *testing.T) {
	nodes := []string{"a"}
	edges := [][2]string{
		{"a", "ghost"},
	}

	err := ValidateStateGraphTopology(nodes, edges, nil, nil)
	if err == nil {
		t.Fatal("expected error for edge referencing undeclared node")
	}
}

func TestValidateStateGraphTopology_RejectsBudgetOverflow(t *testing.T) {
	nodes := []string{"a", "b"}
	edges := [][2]string{{"a", "b"}}
	maxVisits := map[string]int{"a": 1, "b": StateGraphMaxTotalVisitBudget} // sum 超过上限

	err := ValidateStateGraphTopology(nodes, edges, maxVisits, nil)
	if err == nil {
		t.Fatal("expected error for total visit budget overflow")
	}
}

func TestValidateStateGraphTopology_RejectsNodeCountOverflow(t *testing.T) {
	nodes := make([]string, 51)
	for i := range nodes {
		nodes[i] = string(rune('a' + i%26))
	}
	err := ValidateStateGraphTopology(nodes, nil, nil, nil)
	if err == nil {
		t.Fatal("expected error for node count exceeding circuit-breaker limit")
	}
}

func TestValidateStateGraphTopology_DefaultMaxVisitsIsOne(t *testing.T) {
	// 未声明 MaxVisits（nil map）时，简单线性无环图应当照常通过（向后兼容 DAG 语义）。
	nodes := []string{"a", "b", "c"}
	edges := [][2]string{
		{"a", "b"},
		{"b", "c"},
	}
	if err := ValidateStateGraphTopology(nodes, edges, nil, nil); err != nil {
		t.Fatalf("expected simple linear graph to pass with default MaxVisits=1, got: %v", err)
	}
}
