package orchestrator

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
)

// runStateGraphMockWorkers 订阅 task_posted 事件，对每个投递的任务按其 CapabilityType
// 查表决定 mock 输出后自动认领+完成。outputFor 允许按 capType 维护有状态的重试计数
// （如"前两次失败，第三次通过"）。
//
// 注：PostTask（sqlite_blackboard.go）当前将 TaskEntry.Type 写入 tasks.session_id 列
// （而非字面意义的 session 标识），tasks 表本身并无独立的 type 列（对照
// internal/protocol/schema/007_tasks.sql）。此为既有实现约定，本测试按此约定读取，
// 不在本次 StateGraph 开发范围内改动。
func runStateGraphMockWorkers(ctx context.Context, t *testing.T, bb *SQLiteBlackboard, outputFor func(capType string) []byte) {
	events, _ := bb.Subscribe(ctx)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-events:
				if !ok {
					return
				}
				if ev.Type != "task_posted" {
					continue
				}
				taskID := ev.TaskID
				var capType string
				if err := bb.db.QueryRowContext(ctx, "SELECT session_id FROM tasks WHERE task_id=?", taskID).Scan(&capType); err != nil {
					t.Logf("mock worker: query capability type for %s failed: %v", taskID, err)
					continue
				}
				claimed, err := bb.ClaimTask(ctx, taskID, "agent")
				if err != nil || !claimed {
					t.Logf("mock worker: claim %s failed: claimed=%v err=%v", taskID, claimed, err)
					continue
				}
				if err := bb.CompleteTask(ctx, taskID, "agent", outputFor(capType)); err != nil {
					t.Logf("mock worker: complete %s failed: %v", taskID, err)
				}
			}
		}
	}()
}

func TestStateGraphExecutor_LinearNoCondition(t *testing.T) {
	bb := setupPatternBlackboard(t)
	executor := NewStateGraphExecutor(bb)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// A -> B -> C，均为无条件边，MaxVisits 全部未声明（默认 1），
	// 与既有 PatternDAGExecutor 语义等价的向后兼容场景。
	spec := protocol.WorkflowGraphSpec{
		Nodes: []protocol.WorkflowNodeSpec{
			{ID: "A", CapabilityType: "capA"},
			{ID: "B", CapabilityType: "capB"},
			{ID: "C", CapabilityType: "capC"},
		},
		Edges: []protocol.WorkflowEdgeSpec{
			{From: "A", To: "B"},
			{From: "B", To: "C"},
		},
	}

	runStateGraphMockWorkers(ctx, t, bb, func(string) []byte { return []byte(`{"ok":true}`) })

	if err := executor.Execute(ctx, "parent", spec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestStateGraphExecutor_ConditionalRouting(t *testing.T) {
	bb := setupPatternBlackboard(t)
	executor := NewStateGraphExecutor(bb)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// verify 输出 verdict=pass 时走 done；verdict=fail 时走 retry。
	// mock 恒定返回 pass，因此 retry 节点绝不应被触发。
	spec := protocol.WorkflowGraphSpec{
		Nodes: []protocol.WorkflowNodeSpec{
			{ID: "verify", CapabilityType: "capVerify"},
			{ID: "done", CapabilityType: "capDone"},
			{ID: "retry", CapabilityType: "capRetry"},
		},
		Edges: []protocol.WorkflowEdgeSpec{
			{From: "verify", To: "done", Condition: &protocol.EdgeCondition{Field: "verdict", Op: protocol.CondEquals, Value: "pass"}},
			{From: "verify", To: "retry", Condition: &protocol.EdgeCondition{Field: "verdict", Op: protocol.CondEquals, Value: "fail"}},
		},
	}

	var mu sync.Mutex
	retryTriggered := false
	runStateGraphMockWorkers(ctx, t, bb, func(capType string) []byte {
		if capType == "capRetry" {
			mu.Lock()
			retryTriggered = true
			mu.Unlock()
		}
		return []byte(`{"verdict":"pass"}`)
	})

	if err := executor.Execute(ctx, "parent", spec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if retryTriggered {
		t.Error("retry node should never have been triggered (verify always outputs verdict=pass)")
	}
}

func TestStateGraphExecutor_BoundedLoopTerminates(t *testing.T) {
	bb := setupPatternBlackboard(t)
	executor := NewStateGraphExecutor(bb)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// executor -> verify -> (fail: 回 executor 重试，最多 3 次；pass: -> done)
	// executor 同时是循环边目标，入度恒 > 0，需显式 IsEntry 标记才能作为起点。
	spec := protocol.WorkflowGraphSpec{
		Nodes: []protocol.WorkflowNodeSpec{
			{ID: "executor", CapabilityType: "capExecutor", MaxVisits: 3, IsEntry: true},
			{ID: "verify", CapabilityType: "capVerify", MaxVisits: 3},
			{ID: "done", CapabilityType: "capDone"},
		},
		Edges: []protocol.WorkflowEdgeSpec{
			{From: "executor", To: "verify"},
			{From: "verify", To: "executor", Condition: &protocol.EdgeCondition{Field: "verdict", Op: protocol.CondEquals, Value: "fail"}},
			{From: "verify", To: "done", Condition: &protocol.EdgeCondition{Field: "verdict", Op: protocol.CondEquals, Value: "pass"}},
		},
	}

	var mu sync.Mutex
	verifyAttempts := 0
	runStateGraphMockWorkers(ctx, t, bb, func(capType string) []byte {
		if capType != "capVerify" {
			return []byte(`{}`)
		}
		mu.Lock()
		verifyAttempts++
		attempt := verifyAttempts
		mu.Unlock()
		if attempt < 3 {
			return []byte(`{"verdict":"fail"}`)
		}
		return []byte(`{"verdict":"pass"}`)
	})

	if err := executor.Execute(ctx, "parent", spec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if verifyAttempts != 3 {
		t.Errorf("expected verify to run exactly 3 times (2 fail + 1 pass), got %d", verifyAttempts)
	}
}

func TestStateGraphExecutor_LoopExhaustsMaxVisitsWithoutPassing(t *testing.T) {
	bb := setupPatternBlackboard(t)
	executor := NewStateGraphExecutor(bb)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// verify 恒定失败：executor/verify 均在 MaxVisits=2 耗尽后自然停止投递，
	// 不应无限循环，Execute 应正常返回 nil（不是"错误"，只是没有路径走到 done）。
	spec := protocol.WorkflowGraphSpec{
		Nodes: []protocol.WorkflowNodeSpec{
			{ID: "executor", CapabilityType: "capExecutor", MaxVisits: 2, IsEntry: true},
			{ID: "verify", CapabilityType: "capVerify", MaxVisits: 2},
		},
		Edges: []protocol.WorkflowEdgeSpec{
			{From: "executor", To: "verify"},
			{From: "verify", To: "executor", Condition: &protocol.EdgeCondition{Field: "verdict", Op: protocol.CondEquals, Value: "fail"}},
		},
	}

	runStateGraphMockWorkers(ctx, t, bb, func(string) []byte { return []byte(`{"verdict":"fail"}`) })

	done := make(chan error, 1)
	go func() { done <- executor.Execute(ctx, "parent", spec) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("Execute did not terminate — bounded loop failed to stop after MaxVisits exhausted")
	}
}

func TestStateGraphExecutor_TaskFailedPropagates(t *testing.T) {
	bb := setupPatternBlackboard(t)
	executor := NewStateGraphExecutor(bb)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	spec := protocol.WorkflowGraphSpec{
		Nodes: []protocol.WorkflowNodeSpec{
			{ID: "A", CapabilityType: "capA"},
		},
	}

	events, _ := bb.Subscribe(ctx)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-events:
				if !ok {
					return
				}
				if ev.Type == "task_posted" {
					claimed, _ := bb.ClaimTask(ctx, ev.TaskID, "agent")
					if claimed {
						bb.FailTask(ctx, ev.TaskID, "agent", []byte("boom")) //nolint:errcheck
					}
				}
			}
		}
	}()

	err := executor.Execute(ctx, "parent", spec)
	if err == nil {
		t.Fatal("expected fail-fast error when node task_failed")
	}
}

func TestStateGraphExecutor_RejectsMaxVisitsWithCompensation(t *testing.T) {
	bb := setupPatternBlackboard(t)
	executor := NewStateGraphExecutor(bb)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	spec := protocol.WorkflowGraphSpec{
		Nodes: []protocol.WorkflowNodeSpec{
			{
				ID: "A", CapabilityType: "capA", MaxVisits: 2,
				Compensation: &protocol.CompensationAction{ToolName: "undo_a"},
			},
		},
	}

	if err := executor.Execute(ctx, "parent", spec); err == nil {
		t.Fatal("expected validation error for MaxVisits>1 combined with Compensation")
	}
}

// TestStateGraphExecutor_ANDJoinWaitsForAllParents 验证扇入 AND-Join 记账
// （2026-07-12 workflow DAG 并行接入时补齐）：C 依赖 A、B 两条无条件边，只有 A
// 完成时不得触发 C（旧实现的 OR 语义会在此处误触发），必须等 B 也完成才触发一次。
func TestStateGraphExecutor_ANDJoinWaitsForAllParents(t *testing.T) {
	bb := setupPatternBlackboard(t)
	executor := NewStateGraphExecutor(bb)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	spec := protocol.WorkflowGraphSpec{
		Nodes: []protocol.WorkflowNodeSpec{
			{ID: "A", CapabilityType: "capA"},
			{ID: "B", CapabilityType: "capB"},
			{ID: "C", CapabilityType: "capC"},
		},
		Edges: []protocol.WorkflowEdgeSpec{
			{From: "A", To: "C"},
			{From: "B", To: "C"},
		},
	}

	releaseB := make(chan struct{})
	cReached := make(chan struct{})
	var cReachedOnce sync.Once

	events, _ := bb.Subscribe(ctx)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-events:
				if !ok {
					return
				}
				if ev.Type != "task_posted" {
					continue
				}
				taskID := ev.TaskID
				var capType string
				if err := bb.db.QueryRowContext(ctx, "SELECT session_id FROM tasks WHERE task_id=?", taskID).Scan(&capType); err != nil {
					continue
				}
				claimed, err := bb.ClaimTask(ctx, taskID, "agent")
				if err != nil || !claimed {
					continue
				}
				switch capType {
				case "capA":
					bb.CompleteTask(ctx, taskID, "agent", []byte(`{}`)) //nolint:errcheck
				case "capB":
					// B 故意延迟完成：若 C 在 B 完成前被触发，说明 AND-Join 记账失效
					// （退化为旧 OR 语义，仅凭 A 完成就误触发 C）。
					go func(tid string) {
						<-releaseB
						bb.CompleteTask(ctx, tid, "agent", []byte(`{}`)) //nolint:errcheck
					}(taskID)
				case "capC":
					cReachedOnce.Do(func() { close(cReached) })
					bb.CompleteTask(ctx, taskID, "agent", []byte(`{}`)) //nolint:errcheck
				}
			}
		}
	}()

	done := make(chan error, 1)
	go func() { done <- executor.Execute(ctx, "parent", spec) }()

	select {
	case <-cReached:
		t.Fatal("C 不应在 B 完成前被触发（AND-Join 记账失效，退化为 OR 语义）")
	case <-time.After(300 * time.Millisecond):
		// 符合预期：A 已完成但 B 仍在阻塞，C 应保持未触发。
	}

	close(releaseB)

	select {
	case <-cReached:
		// C 在 B 完成后被触发，符合预期。
	case <-time.After(4 * time.Second):
		t.Fatal("C 在 B 完成后应被触发，但超时未触发")
	}

	if err := <-done; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestStateGraphExecutor_RejectsInvalidTopology(t *testing.T) {
	bb := setupPatternBlackboard(t)
	executor := NewStateGraphExecutor(bb)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// A<->B 互为唯一彼此的前驱，无入度为 0 的入口节点。
	spec := protocol.WorkflowGraphSpec{
		Nodes: []protocol.WorkflowNodeSpec{
			{ID: "A", CapabilityType: "capA"},
			{ID: "B", CapabilityType: "capB"},
		},
		Edges: []protocol.WorkflowEdgeSpec{
			{From: "A", To: "B"},
			{From: "B", To: "A"},
		},
	}

	if err := executor.Execute(ctx, "parent", spec); err == nil {
		t.Fatal("expected topology error for graph with no entry node")
	}
}
