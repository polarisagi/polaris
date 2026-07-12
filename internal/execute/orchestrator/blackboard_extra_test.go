package orchestrator

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

func newTestBlackboard() *Blackboard {
	return &Blackboard{
		tasks:        make(map[string]*types.TaskEntry),
		taskVersions: make(map[string]int32),
		events:       make(chan types.BlackboardEvent, 100),
		agents:       make(map[string]*AgentHandle),
	}
}

func TestBlackboardExtra(t *testing.T) {
	b := newTestBlackboard()
	ctx := context.Background()

	// Test Epoch
	b.SetEpoch(42)
	if b.GetEpoch() != 42 {
		t.Errorf("expected epoch 42")
	}

	// Test PostTask
	task1 := &types.TaskEntry{
		ID:   "task-1",
		Type: "agent-1",
	}
	err := b.PostTask(ctx, task1)
	if err != nil {
		t.Fatal(err)
	}

	// Test PostBatch
	task2 := &types.TaskEntry{ID: "task-2", Type: "agent-2"}
	err = b.PostBatch(ctx, []*types.TaskEntry{task2})
	if err != nil {
		t.Fatal(err)
	}

	// Test PeekTask
	taskSnap, err := b.PeekTask(ctx, "task-1")
	if err != nil {
		t.Errorf("failed to peek task")
	}
	if taskSnap == nil {
		t.Errorf("expected task snapshot")
	}

	// Test ClaimTask
	ok, err := b.ClaimTask(ctx, "task-1", "worker-1")
	if err != nil || !ok {
		t.Errorf("failed to claim task")
	}

	// Test RenewLease
	err = b.RenewLease(ctx, "task-1", "worker-1")
	if err != nil {
		t.Fatal(err)
	}

	// Test StartExecution
	err = b.StartExecution(ctx, "task-1", "worker-1")
	if err != nil {
		t.Fatal(err)
	}

	// Test SideEffectPreCheck
	err = b.SideEffectPreCheck(ctx, "task-1", "worker-1", b.taskVersions["task-1"])
	if err != nil {
		t.Fatal(err)
	}

	// Test BeginCompensation and EndCompensation
	err = b.BeginCompensation(ctx, "task-1", "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	err = b.EndCompensation(ctx, "task-1", "worker-1")
	if err != nil {
		t.Fatal(err)
	}

	// Test SuspendForHITL and ResumeFromHITL
	task3 := &types.TaskEntry{ID: "task-3", Type: "agent-1"}
	_ = b.PostTask(ctx, task3)
	_, _ = b.ClaimTask(ctx, "task-3", "worker-1")
	_ = b.StartExecution(ctx, "task-3", "worker-1")

	err = b.SuspendForHITL(ctx, "task-3", "worker-1", 100)
	if err != nil {
		t.Fatal(err)
	}
	err = b.ResumeFromHITL(ctx, "task-3", "worker-1", true)
	if err != nil {
		t.Fatal(err)
	}

	// Test UpdateTaskTokens
	err = b.UpdateTaskTokens(ctx, "task-3", 100, 200, 50, 0.01)
	if err != nil {
		t.Fatal(err)
	}

	// Test CompleteTask
	err = b.CompleteTask(ctx, "task-3", "worker-1", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Test FailTask
	b.ClaimTask(ctx, "task-2", "worker-1")
	b.StartExecution(ctx, "task-2", "worker-1")
	err = b.FailTask(ctx, "task-2", "worker-1", []byte("some error"))
	if err != nil {
		t.Fatal(err)
	}

	// Test Error
	be := BlackboardError{msg: "error test"}
	if be.Error() != "error test" {
		t.Errorf("unexpected error message")
	}
}
