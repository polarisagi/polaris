package workflowadmin

import (
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
)

func findNode(t *testing.T, spec protocol.WorkflowGraphSpec, id string) protocol.WorkflowNodeSpec {
	t.Helper()
	for _, n := range spec.Nodes {
		if n.ID == id {
			return n
		}
	}
	t.Fatalf("node %s not found", id)
	return protocol.WorkflowNodeSpec{}
}

func countEdges(spec protocol.WorkflowGraphSpec, from, to string) int {
	n := 0
	for _, e := range spec.Edges {
		if e.From == from && e.To == to {
			n++
		}
	}
	return n
}

// TestBuildGraphSpec_ChainIgnoresDependsOn 验证 chain 模式（默认/向后兼容）完全
// 忽略 DependsOn，按 Seq 合成顺序链——即便某步骤误填了 DependsOn，也不应影响
// chain 语义（避免脏数据/UI 误操作意外产生 DAG 行为）。
func TestBuildGraphSpec_ChainIgnoresDependsOn(t *testing.T) {
	steps := []workflowStep{
		{ID: "s1", Seq: 0},
		{ID: "s2", Seq: 1, DependsOn: []string{"s1"}}, // 显式声明也应被忽略
		{ID: "s3", Seq: 2},
	}
	spec := buildGraphSpec("chain", "wf1", "run1", steps)

	if len(spec.Nodes) != 3 {
		t.Fatalf("expected 3 nodes, got %d", len(spec.Nodes))
	}
	if countEdges(spec, "s1", "s2") != 1 {
		t.Error("expected chain edge s1->s2")
	}
	if countEdges(spec, "s2", "s3") != 1 {
		t.Error("expected chain edge s2->s3")
	}
	if !findNode(t, spec, "s1").IsEntry {
		t.Error("s1 should be entry (no predecessor)")
	}
	if findNode(t, spec, "s2").IsEntry {
		t.Error("s2 should not be entry (has chain predecessor)")
	}
}

// TestBuildGraphSpec_DAGHonorsDependsOn 验证 dag 模式如实按 DependsOn（Seq 索引
// 字符串）构造边：空 DependsOn 是真正的并行入口（不会被合成为"依赖上一个 Seq"），
// 多前驱产生对应数量的边（由 StateGraphExecutor 的 AND-Join 记账保证等待全部完成）。
func TestBuildGraphSpec_DAGHonorsDependsOn(t *testing.T) {
	steps := []workflowStep{
		{ID: "a", Seq: 0}, // 空 DependsOn，并行入口
		{ID: "b", Seq: 1}, // 空 DependsOn，并行入口
		{ID: "c", Seq: 2, DependsOn: []string{"0", "1"}}, // 汇入 seq0(a)、seq1(b)
	}
	spec := buildGraphSpec("dag", "wf1", "run1", steps)

	if !findNode(t, spec, "a").IsEntry {
		t.Error("a should be entry (empty depends_on in dag mode)")
	}
	if !findNode(t, spec, "b").IsEntry {
		t.Error("b should be entry (empty depends_on in dag mode)")
	}
	if findNode(t, spec, "c").IsEntry {
		t.Error("c should not be entry (has real depends_on)")
	}
	if countEdges(spec, "a", "c") != 1 || countEdges(spec, "b", "c") != 1 {
		t.Error("expected edges a->c and b->c")
	}
	// dag 模式不应合成 a->b 这类 Seq 顺序边（与 chain 模式的关键区别）。
	if countEdges(spec, "a", "b") != 0 {
		t.Error("dag mode must not synthesize Seq-order edges")
	}
}

// TestBuildGraphSpec_MaxRetriesAddsSelfLoop 验证 max_retries>0 附加自环条件边 +
// MaxVisits=1+max_retries，且不影响该节点的 IsEntry 判定（自环不应被误算作
// "外部前驱"，否则会导致 postEntryNodes 无法把该节点识别为入口）。
func TestBuildGraphSpec_MaxRetriesAddsSelfLoop(t *testing.T) {
	steps := []workflowStep{
		{ID: "s1", Seq: 0, MaxRetries: 2},
	}
	spec := buildGraphSpec("chain", "wf1", "run1", steps)

	node := findNode(t, spec, "s1")
	if node.MaxVisits != 3 {
		t.Errorf("expected MaxVisits=3 (1+2), got %d", node.MaxVisits)
	}
	if !node.IsEntry {
		t.Error("s1 should remain entry despite self-loop retry edge")
	}
	if countEdges(spec, "s1", "s1") != 1 {
		t.Fatalf("expected exactly 1 self-loop edge, got %d", countEdges(spec, "s1", "s1"))
	}
	for _, e := range spec.Edges {
		if e.From == "s1" && e.To == "s1" {
			if e.Condition == nil || e.Condition.Field != "status" || e.Condition.Value != "error" {
				t.Errorf("unexpected self-loop condition: %+v", e.Condition)
			}
		}
	}
}

// TestValidateStepRetryCompensation_RejectsBoth 验证 max_retries 与
// compensation_tool 互斥校验（与 StateGraphExecutor 自身的 MaxVisits+Compensation
// 校验规则一致，此处提前于 HTTP 层拒绝）。
func TestValidateStepRetryCompensation_RejectsBoth(t *testing.T) {
	steps := []workflowStep{
		{ID: "s1", MaxRetries: 1, CompensationTool: "undo_s1"},
	}
	if err := validateStepRetryCompensation(steps); err == nil {
		t.Error("expected error when max_retries>0 and compensation_tool both set")
	}
}

func TestValidateStepRetryCompensation_AllowsEither(t *testing.T) {
	steps := []workflowStep{
		{ID: "s1", MaxRetries: 1},
		{ID: "s2", CompensationTool: "undo_s2"},
	}
	if err := validateStepRetryCompensation(steps); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
