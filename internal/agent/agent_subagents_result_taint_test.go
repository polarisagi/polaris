package agent

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/execute/orchestrator"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestChildResultTaintPersistsThroughPeekAndAgentResult(t *testing.T) {
	ctx := context.Background()
	db := setupTestDBWithBlackboardSchema(t)
	bb := orchestrator.NewSQLiteBlackboard(db)
	if err := bb.PostTask(ctx, &types.TaskEntry{ID: "child-reviewed", Type: "agent_handoff:research", IntentTaint: types.TaintHigh}); err != nil {
		t.Fatal(err)
	}
	if _, err := bb.ClaimTask(ctx, "child-reviewed", "worker"); err != nil {
		t.Fatal(err)
	}
	if err := bb.CompleteTaskWithTaint(ctx, "child-reviewed", "worker", []byte("reviewed answer"), types.TaintUserReviewed); err != nil {
		t.Fatal(err)
	}
	snapshot, err := bb.PeekTask(ctx, "child-reviewed")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ResultTaint != types.TaintUserReviewed || string(snapshot.Result) != "reviewed answer" {
		t.Fatalf("result and taint must be read from the same durable task row: %+v", snapshot)
	}

	parent := NewAgentWithDefaults("parent-result-taint")
	parent.InjectHandoffPoster(bb)
	result, resultTaint, err := parent.AgentResultWithTaint(ctx, "child-reviewed")
	if err != nil {
		t.Fatal(err)
	}
	if result != "reviewed answer" || resultTaint != types.TaintUserReviewed {
		t.Fatalf("agent_result must preserve child level 4, result=%q taint=%v", result, resultTaint)
	}

	parent.sCtx.HandoffTaskID = "child-reviewed"
	restored, err := parent.executeTransferToAgent(ctx, "research", "", types.TaintHigh)
	if err != nil {
		t.Fatal(err)
	}
	if restored.TaintLevel != types.TaintUserReviewed {
		t.Fatalf("handoff recovery must preserve child level 4, got %v", restored.TaintLevel)
	}
}

func TestLegacyZeroTaskResultTaintFailsClosedAsHigh(t *testing.T) {
	ctx := context.Background()
	db := setupTestDBWithBlackboardSchema(t)
	bb := orchestrator.NewSQLiteBlackboard(db)
	if err := bb.PostTask(ctx, &types.TaskEntry{ID: "child-legacy", Type: "agent_handoff:research"}); err != nil {
		t.Fatal(err)
	}
	if _, err := bb.ClaimTask(ctx, "child-legacy", "worker"); err != nil {
		t.Fatal(err)
	}
	if err := bb.CompleteTask(ctx, "child-legacy", "worker", []byte("legacy answer")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "UPDATE tasks SET result_taint=0 WHERE task_id=?", "child-legacy"); err != nil {
		t.Fatal(err)
	}
	parent := NewAgentWithDefaults("parent-legacy-taint")
	parent.InjectHandoffPoster(bb)
	_, level, err := parent.AgentResultWithTaint(ctx, "child-legacy")
	if err != nil {
		t.Fatal(err)
	}
	if level != types.TaintHigh {
		t.Fatalf("legacy zero result taint must fail closed to High, got %v", level)
	}
}

func TestTaskCompletionTaintNeverDropsIntentOrExistingResultLevel(t *testing.T) {
	ctx := context.Background()
	db := setupTestDBWithBlackboardSchema(t)
	bb := orchestrator.NewSQLiteBlackboard(db)
	if err := bb.PostTask(ctx, &types.TaskEntry{ID: "intent-reviewed", Type: "worker", IntentTaint: types.TaintUserReviewed}); err != nil {
		t.Fatal(err)
	}
	if _, err := bb.ClaimTask(ctx, "intent-reviewed", "worker"); err != nil {
		t.Fatal(err)
	}
	if err := bb.CompleteTaskWithTaint(ctx, "intent-reviewed", "worker", []byte("done"), types.TaintHigh); err != nil {
		t.Fatal(err)
	}
	snapshot, err := bb.PeekTask(ctx, "intent-reviewed")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ResultTaint != types.TaintUserReviewed {
		t.Fatalf("completion must retain the maximum intent/result taint, got %v", snapshot.ResultTaint)
	}

	if err := bb.PostTask(ctx, &types.TaskEntry{ID: "failure-reviewed", Type: "worker"}); err != nil {
		t.Fatal(err)
	}
	if _, err := bb.ClaimTask(ctx, "failure-reviewed", "worker"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "UPDATE tasks SET result_taint=? WHERE task_id=?", int(types.TaintUserReviewed), "failure-reviewed"); err != nil {
		t.Fatal(err)
	}
	if err := bb.FailTask(ctx, "failure-reviewed", "worker", []byte("failed")); err != nil {
		t.Fatal(err)
	}
	snapshot, err = bb.PeekTask(ctx, "failure-reviewed")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ResultTaint != types.TaintUserReviewed {
		t.Fatalf("failure body write must not lower an existing result taint, got %v", snapshot.ResultTaint)
	}
}
