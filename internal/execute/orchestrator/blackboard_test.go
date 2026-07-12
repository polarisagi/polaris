package orchestrator

import (
	"context"
	"sync"
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

func TestBlackboard_Lifecycle(t *testing.T) {
	b := NewBlackboard()
	ctx := context.Background()

	task := &types.TaskEntry{
		ID: "task-1",
	}

	b.PostTask(ctx, task)

	// Check initial state
	if task.Status != types.TaskPending {
		t.Errorf("Expected TaskPending, got %v", task.Status)
	}

	// Claim
	success, err := b.ClaimTask(ctx, "task-1", "agent-a")
	if err != nil {
		t.Fatalf("Claim failed: %v", err)
	}
	if !success {
		t.Fatal("Expected claim to succeed")
	}

	if task.Status != types.TaskClaimed {
		t.Errorf("Expected TaskClaimed, got %v", task.Status)
	}

	// Start execution
	err = b.StartExecution(ctx, "task-1", "agent-a")
	if err != nil {
		t.Fatalf("StartExecution failed: %v", err)
	}
	if task.Status != types.TaskExecuting {
		t.Errorf("Expected TaskExecuting, got %v", task.Status)
	}

	// Complete
	err = b.CompleteTask(ctx, "task-1", "agent-a", []byte("success"))
	if err != nil {
		t.Fatalf("CompleteTask failed: %v", err)
	}
	if task.Status != types.TaskDone {
		t.Errorf("Expected TaskDone, got %v", task.Status)
	}
}

func TestBlackboard_CAS_Concurrency(t *testing.T) {
	b := NewBlackboard()
	ctx := context.Background()
	task := &types.TaskEntry{ID: "task-2"}
	b.PostTask(ctx, task)

	var wg sync.WaitGroup
	workers := 50
	successCount := 0
	var countMu sync.Mutex

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			success, _ := b.ClaimTask(ctx, "task-2", "agent-worker")
			if success {
				countMu.Lock()
				successCount++
				countMu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if successCount != 1 {
		t.Errorf("Expected exactly 1 claim success, got %d", successCount)
	}
}

// func TestBlackboard_Reaper_Phase1(t *testing.T) {
// 	b := NewBlackboard()
// 	r := &Reaper{blackboard: b}
//
// 	task := &TaskEntry{ID: "task-3"}
// 	b.PostTask(task)
//
// 	b.Claim("task-3", "agent-a")
//
// 	// Manually expire the lease
// 	now := time.Now().Unix()
// 	task.ExpiresAt = now - 10
//
// 	ctx := context.Background()
// 	r.Phase1(ctx, now) // Should reap the task
//
// 	// Reaped task should be back to pending
// 	if task.ClaimedBy.Load() != nil {
// 		t.Errorf("Expected ClaimedBy to be nil after reap")
// 	}
// 	if task.Status.Load() != int32(TaskPending) {
// 		t.Errorf("Expected status to be Pending after reap, got %d", task.Status.Load())
// 	}
// }

// func TestBlackboard_Reaper_Phase2(t *testing.T) {
// 	b := NewBlackboard()
// 	r := &Reaper{blackboard: b}
//
// 	task := &TaskEntry{ID: "task-4"}
// 	b.PostTask(task)
// 	b.Claim("task-4", "agent-a")
// 	b.CompleteTask("task-4", "agent-a", []byte("done"))
//
// 	// Task is done. Set updated_at to 301 seconds ago.
// 	now := time.Now().Unix()
// 	task.UpdatedAt = now - 301
//
// 	r.Phase2(now)
//
// 	b.mu.RLock()
// 	_, ok := b.tasks["task-4"]
// 	b.mu.RUnlock()
//
// 	if ok {
// 		t.Errorf("Expected task to be evicted from blackboard")
// 	}
// }
