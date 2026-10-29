package dag

import (
	"context"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

// ─── 测试用 mockToolExec ─────────────────────────────────────────────────────

func successExec(_ context.Context, toolName string, _ []byte, taintLevel types.TaintLevel) (*types.ToolResult, error) {
	return &types.ToolResult{Success: true, Output: []byte("output:" + toolName)}, nil
}

//nolint:unused
func failExec(_ context.Context, _ string, _ []byte, taintLevel types.TaintLevel) (*types.ToolResult, error) {
	return &types.ToolResult{Success: false, Error: "tool failed"}, nil
}

// ─── 拓扑校验测试 ────────────────────────────────────────────────────────────

func TestValidateDAGTopology_LinearChain(t *testing.T) {
	plan := &DAGPlan{
		Nodes: []ExecNode{
			{ID: "a", ToolName: "t1"},
			{ID: "b", ToolName: "t2", DependsOn: []string{"a"}},
			{ID: "c", ToolName: "t3", DependsOn: []string{"b"}},
		},
	}
	if err := validateDAGTopology(plan); err != nil {
		t.Fatalf("expected valid DAG, got: %v", err)
	}
}

func TestValidateDAGTopology_CycleDetected(t *testing.T) {
	plan := &DAGPlan{
		Nodes: []ExecNode{
			{ID: "a", ToolName: "t1", DependsOn: []string{"b"}},
			{ID: "b", ToolName: "t2", DependsOn: []string{"a"}},
		},
	}
	if err := validateDAGTopology(plan); err == nil {
		t.Fatal("expected cycle error, got nil")
	}
}

func TestValidateDAGTopology_NodeCountCircuitBreaker(t *testing.T) {
	nodes := make([]ExecNode, 51)
	for i := range nodes {
		nodes[i] = ExecNode{ID: string(rune('a'+i%26)) + string(rune('0'+i/26))}
	}
	plan := &DAGPlan{Nodes: nodes}
	if err := validateDAGTopology(plan); err == nil {
		t.Fatal("expected circuit breaker error, got nil")
	}
}

// ─── DAGExecutor 执行测试 ─────────────────────────────────────────────────────

func TestDAGExecutor_LinearChainSuccess(t *testing.T) {
	exec := NewDAGExecutor(successExec, nil)
	plan := &DAGPlan{
		Nodes: []ExecNode{
			{ID: "a", ToolName: "tool-a"},
			{ID: "b", ToolName: "tool-b", DependsOn: []string{"a"}},
			{ID: "c", ToolName: "tool-c", DependsOn: []string{"b"}},
		},
	}
	results, err := exec.Execute(context.Background(), plan, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	for _, r := range results {
		if r.Err != nil {
			t.Errorf("node %s failed: %v", r.NodeID, r.Err)
		}
	}
}

func TestDAGExecutor_ParallelBranchSuccess(t *testing.T) {
	exec := NewDAGExecutor(successExec, nil)
	plan := &DAGPlan{
		Nodes: []ExecNode{
			{ID: "root", ToolName: "root"},
			{ID: "left", ToolName: "left", DependsOn: []string{"root"}},
			{ID: "right", ToolName: "right", DependsOn: []string{"root"}},
			{ID: "merge", ToolName: "merge", DependsOn: []string{"left", "right"}},
		},
	}
	results, err := exec.Execute(context.Background(), plan, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 4 {
		t.Fatalf("expected 4 results, got %d", len(results))
	}
}

func TestDAGExecutor_NodeFailTriggersSagaCompensation(t *testing.T) {
	compensated := false
	exec := NewDAGExecutor(func(ctx context.Context, toolName string, args []byte, taintLevel types.TaintLevel) (*types.ToolResult, error) {
		if toolName == "fail" {
			return &types.ToolResult{Success: false, Error: "injected failure"}, nil
		}
		if toolName == "undo-write" {
			compensated = true
		}
		return &types.ToolResult{Success: true, Output: []byte("ok")}, nil
	}, nil)

	plan := &DAGPlan{
		Nodes: []ExecNode{
			{
				ID:       "write",
				ToolName: "write",
				Compensation: &CompensationAction{
					ToolName: "undo-write",
					Args:     []byte(`{"rollback":true}`),
				},
			},
			{
				ID:        "fail",
				ToolName:  "fail",
				DependsOn: []string{"write"},
			},
		},
	}

	_, err := exec.Execute(context.Background(), plan, "", "")
	if err == nil {
		t.Fatal("expected failure, got nil")
	}
	if !compensated {
		t.Fatal("Saga compensation was not triggered")
	}
}

func TestDAGExecutor_SingleNodePlan(t *testing.T) {
	exec := NewDAGExecutor(successExec, nil)
	plan := &DAGPlan{
		Nodes: []ExecNode{
			{ID: "only", ToolName: "tool-only"},
		},
	}
	results, err := exec.Execute(context.Background(), plan, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 || results[0].NodeID != "only" {
		t.Fatalf("unexpected results: %v", results)
	}
}

func TestDAGExecutor_ContextCancellation(t *testing.T) {
	slowExec := func(ctx context.Context, _ string, _ []byte, taintLevel types.TaintLevel) (*types.ToolResult, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Second):
			return &types.ToolResult{Success: true, Output: []byte("ok")}, nil
		}
	}

	exec := NewDAGExecutor(slowExec, nil)
	plan := &DAGPlan{
		Nodes: []ExecNode{
			{ID: "slow", ToolName: "slow"},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := exec.Execute(ctx, plan, "", "")
	if err == nil {
		t.Fatal("expected context cancellation error, got nil")
	}
}
