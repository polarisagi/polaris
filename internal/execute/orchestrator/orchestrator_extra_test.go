package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

func TestOrchestratorExtra(t *testing.T) {
	db, err := newMockSQLiteDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	bb := NewSQLiteBlackboard(db)
	registry := NewAgentRegistry()
	orch := NewOrchestrator(bb, registry, 3)

	// RegisterWorker
	worker := NewWorker("agent-1", bb, nil)
	orch.RegisterWorker(worker)

	// queryAgentLoads
	ctx, cancel := context.WithCancel(context.Background())
	loads := orch.queryAgentLoads(ctx)
	if len(loads) != 0 {
		t.Errorf("expected 0 agent load initially")
	}

	// ListenLoop with cancel
	errCh := make(chan error, 1)
	go func() {
		errCh <- orch.ListenLoop(ctx)
	}()

	// Add a task
	task := &types.TaskEntry{ID: "task-1", Type: "agent-1"}
	_ = bb.PostTask(ctx, task)

	// Add another task
	_ = bb.PostTask(ctx, &types.TaskEntry{ID: "task-2", Type: "agent-1"})

	time.Sleep(100 * time.Millisecond)
	cancel()
	err = <-errCh
	if err != nil && err != context.Canceled {
		t.Errorf("expected canceled error, got %v", err)
	}
}

func TestReaper(t *testing.T) {
	db, err := newMockSQLiteDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	bb := NewSQLiteBlackboard(db)

	ctx, cancel := context.WithCancel(context.Background())
	r := NewReaper(bb)
	r.scanInterval = 10 * time.Millisecond
	r.gcInterval = 20 * time.Millisecond

	go r.Run(ctx)

	r.Phase2(ctx)
	time.Sleep(50 * time.Millisecond)
	cancel()

	// Test SupervisorEpoch Get and Increment
	var se SupervisorEpoch
	if se.Get() != 0 {
		t.Errorf("expected 0")
	}
	se.Increment()
	if se.Get() != 1 {
		t.Errorf("expected 1")
	}
}
