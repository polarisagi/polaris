package cognition

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	perrors "github.com/polarisagi/polaris/internal/errors"
)

// mockRunner 实现 NodeRunner，用于 DAGExecutor 单元测试。
// failNodeID 非空时，Run 对指定节点返回 error 以触发 Saga 补偿路径。
type mockRunner struct {
	failNodeID string
	runCalled  atomic.Int32
	undoCalled atomic.Int32
}

func (m *mockRunner) Run(_ context.Context, node DAGNode) ([]byte, error) {
	m.runCalled.Add(1)
	if node.ID == m.failNodeID {
		return nil, perrors.New(perrors.CodeInternal, "mock: node failed")
	}
	return []byte(node.ID), nil
}

func (m *mockRunner) Undo(_ context.Context, _ string) error {
	m.undoCalled.Add(1)
	return nil
}

// TestDAGExecutor_LinearDAG_Success 验证线性链 A→B→C 顺序执行，三个节点全部被调用。
func TestDAGExecutor_LinearDAG_Success(t *testing.T) {
	nodes := []DAGNode{
		{ID: "A"},
		{ID: "B"},
		{ID: "C"},
	}
	edges := []DAGEdge{
		{From: "A", To: "B"},
		{From: "B", To: "C"},
	}
	runner := &mockRunner{}
	ex := NewDAGExecutor(nodes, edges, 4)

	if err := ex.Execute(context.Background(), runner); err != nil {
		t.Fatalf("期望成功，得到 error: %v", err)
	}
	if got := runner.runCalled.Load(); got != 3 {
		t.Errorf("runCalled = %d，期望 3", got)
	}
}

// TestDAGExecutor_ParallelDAG_Success 验证菱形拓扑 A→(B,C)→D，四个节点全部被调用。
func TestDAGExecutor_ParallelDAG_Success(t *testing.T) {
	nodes := []DAGNode{
		{ID: "A"},
		{ID: "B"},
		{ID: "C"},
		{ID: "D"},
	}
	edges := []DAGEdge{
		{From: "A", To: "B"},
		{From: "A", To: "C"},
		{From: "B", To: "D"},
		{From: "C", To: "D"},
	}
	runner := &mockRunner{}
	ex := NewDAGExecutor(nodes, edges, 4)

	if err := ex.Execute(context.Background(), runner); err != nil {
		t.Fatalf("期望成功，得到 error: %v", err)
	}
	if got := runner.runCalled.Load(); got != 4 {
		t.Errorf("runCalled = %d，期望 4", got)
	}
}

// TestDAGExecutor_NodeFailure_TriggersSagaUndo 验证 A 成功→B 失败时：
// 返回 error，且 undoCalled > 0（Saga 逆序补偿已触发）。
func TestDAGExecutor_NodeFailure_TriggersSagaUndo(t *testing.T) {
	nodes := []DAGNode{
		{ID: "A"},
		{ID: "B"},
	}
	edges := []DAGEdge{
		{From: "A", To: "B"},
	}
	runner := &mockRunner{failNodeID: "B"}
	ex := NewDAGExecutor(nodes, edges, 4)

	err := ex.Execute(context.Background(), runner)
	if err == nil {
		t.Fatal("期望返回 error，实际为 nil")
	}
	if runner.undoCalled.Load() == 0 {
		t.Error("期望 undoCalled > 0，Saga 补偿未触发")
	}
}

// TestDAGExecutor_Deadlock_NoReadyNodes 验证互相依赖（X→Y 且 Y→X）时，
// Execute 返回含 "deadlock" 关键词的 error。
func TestDAGExecutor_Deadlock_NoReadyNodes(t *testing.T) {
	nodes := []DAGNode{
		{ID: "X"},
		{ID: "Y"},
	}
	// X 依赖 Y，Y 依赖 X → 死锁
	edges := []DAGEdge{
		{From: "Y", To: "X"},
		{From: "X", To: "Y"},
	}
	runner := &mockRunner{}
	ex := NewDAGExecutor(nodes, edges, 4)

	err := ex.Execute(context.Background(), runner)
	if err == nil {
		t.Fatal("期望死锁 error，实际为 nil")
	}
	if !strings.Contains(err.Error(), "deadlock") {
		t.Errorf("期望包含 'deadlock' 的错误信息, 实际: %s", err.Error())
	}
}

// TestDynamicReplan_LowSurprise_NoChange 验证 surpriseIndex=0.3 时，
// 返回与原始 nodes/edges 完全相同的切片引用。
func TestDynamicReplan_LowSurprise_NoChange(t *testing.T) {
	nodes := []DAGNode{
		{ID: "A"},
		{ID: "B"},
	}
	edges := []DAGEdge{
		{From: "A", To: "B"},
	}
	ex := NewDAGExecutor(nodes, edges, 4)

	gotNodes, gotEdges, err := ex.DynamicReplan(context.Background(), "A", 0.3)
	if err != nil {
		t.Fatalf("期望无 error，得到: %v", err)
	}
	if len(gotNodes) != len(nodes) {
		t.Errorf("nodes 长度 = %d，期望 %d", len(gotNodes), len(nodes))
	}
	if len(gotEdges) != len(edges) {
		t.Errorf("edges 长度 = %d，期望 %d", len(gotEdges), len(edges))
	}
}

// TestDynamicReplan_HighSurprise_RemovesUnexecutedDownstream 验证 surpriseIndex=0.9 时，
// 已完成的节点 A 保留，未执行的下游 B/C 不在返回的 newNodes 中。
func TestDynamicReplan_HighSurprise_RemovesUnexecutedDownstream(t *testing.T) {
	nodes := []DAGNode{
		{ID: "A"},
		{ID: "B"},
		{ID: "C"},
	}
	edges := []DAGEdge{
		{From: "A", To: "B"},
		{From: "A", To: "C"},
	}
	ex := NewDAGExecutor(nodes, edges, 4)
	// 直接设置 A 为已完成，模拟 A 执行成功后触发高惊喜重规划
	ex.completed["A"] = true

	gotNodes, _, err := ex.DynamicReplan(context.Background(), "A", 0.9)
	if err != nil {
		t.Fatalf("期望无 error，得到: %v", err)
	}

	// 高惊喜路径：已完成节点 A 保留，未执行的下游（B/C 是 A 的下游）应被移除
	for _, n := range gotNodes {
		if n.ID == "B" || n.ID == "C" {
			t.Errorf("节点 %s 是未执行下游，不应出现在 newNodes 中", n.ID)
		}
	}
	// A 已完成且不在 affected 内，应保留
	found := false
	for _, n := range gotNodes {
		if n.ID == "A" {
			found = true
			break
		}
	}
	if !found {
		t.Error("已完成节点 A 应保留在 newNodes 中")
	}
}
