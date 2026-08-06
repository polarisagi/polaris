package orchestrator

import (
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

func newSagaRun(nodes ...protocol.WorkflowNodeSpec) *stateGraphRun {
	nodeMap := make(map[string]protocol.WorkflowNodeSpec, len(nodes))
	for _, n := range nodes {
		nodeMap[n.ID] = n
	}
	return &stateGraphRun{
		se:           &StateGraphExecutor{},
		parentTaskID: "parent-1",
		nodeMap:      nodeMap,
		inFlight:     make(map[string]string),
	}
}

func compNode(id, undoTool string) protocol.WorkflowNodeSpec {
	return protocol.WorkflowNodeSpec{
		ID: id,
		Compensation: &protocol.CompensationAction{
			ToolName:   undoTool,
			Args:       []byte(`{}`),
			TaintLevel: types.TaintLow,
		},
	}
}

// TestRecordCompensable_ReverseOrder Saga 语义要求**逆序**补偿：
// 后完成的节点先回滚。顺序错了会让"创建资源→授权"这类链条按
// "先撤创建、后撤授权"回滚，第二步作用在已不存在的对象上。
func TestRecordCompensable_ReverseOrder(t *testing.T) {
	r := newSagaRun(compNode("a", "undo_a"), compNode("b", "undo_b"), compNode("c", "undo_c"))

	r.inFlight["a"] = "task-a"
	r.recordCompensable("a", "task-a")
	r.inFlight["b"] = "task-b"
	r.recordCompensable("b", "task-b")
	r.inFlight["c"] = "task-c"
	r.recordCompensable("c", "task-c")

	want := []string{"c", "b", "a"}
	if len(r.executedUndo) != len(want) {
		t.Fatalf("want %d compensable entries, got %d", len(want), len(r.executedUndo))
	}
	for i, w := range want {
		if r.executedUndo[i].node.ID != w {
			t.Fatalf("compensation order[%d]: want %s, got %s", i, w, r.executedUndo[i].node.ID)
		}
	}
}

// TestRecordCompensable_SkipsNodesWithoutCompensation 未声明补偿的节点不登记
// ——否则 runCompensation 会为它投递一个 Type 为空串的补偿任务。
func TestRecordCompensable_SkipsNodesWithoutCompensation(t *testing.T) {
	r := newSagaRun(
		protocol.WorkflowNodeSpec{ID: "plain"}, // 无 Compensation
		compNode("writer", "undo_writer"),
	)

	r.recordCompensable("plain", "task-plain")
	r.recordCompensable("writer", "task-writer")
	r.recordCompensable("unknown-node", "task-x") // 不在 nodeMap 中

	if len(r.executedUndo) != 1 {
		t.Fatalf("only nodes declaring Compensation may be recorded, got %d", len(r.executedUndo))
	}
	if r.executedUndo[0].node.ID != "writer" {
		t.Fatalf("wrong node recorded: %s", r.executedUndo[0].node.ID)
	}
	if r.executedUndo[0].taskID != "task-writer" {
		t.Fatalf("compensation entry must carry the concrete taskID, got %q", r.executedUndo[0].taskID)
	}
}

// TestRunCompensation_NoopWhenNothingSucceeded 首个节点就失败时无已成功节点，
// 补偿必须是彻底的 no-op（不得投递任何任务、不得 panic）。
func TestRunCompensation_NoopWhenNothingSucceeded(t *testing.T) {
	r := newSagaRun(compNode("a", "undo_a"))
	// bb 为 nil：若实现里漏了空队列短路，这里会因 nil 解引用 panic，
	// 正是本测试要守护的点。
	r.runCompensation(t.Context(), "a", []byte("boom"))
}

// TestCancelInFlightSiblings_ExcludesFailedNode 失败节点自身不参与取消——
// 它已经终态，重复取消会把 failed 覆写并多播一次 task_failed 事件。
func TestCancelInFlightSiblings_ExcludesFailedNode(t *testing.T) {
	r := newSagaRun(compNode("a", "undo_a"), compNode("b", "undo_b"))
	r.inFlight["a"] = "task-a" // 失败节点
	r.inFlight["b"] = "task-b" // 在途兄弟

	// bb 为 nil 时 CancelTask 无法调用，这里只验证遍历逻辑跳过了失败节点：
	// 若未跳过，下面对 nil bb 的调用会 panic。
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("failed node must be excluded before touching blackboard: %v", rec)
		}
	}()
	r.inFlight = map[string]string{"a": "task-a"} // 仅剩失败节点自身
	if n := r.cancelInFlightSiblings(t.Context(), "a"); n != 0 {
		t.Fatalf("want 0 cancellations when only the failed node is in flight, got %d", n)
	}
}
