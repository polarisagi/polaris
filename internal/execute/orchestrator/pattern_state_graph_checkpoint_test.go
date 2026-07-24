package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStateGraphExecutor_CheckpointResume(t *testing.T) {
	db := setupTestDB(t)
	_, err := db.Exec(`
		CREATE TABLE task_checkpoints (
			task_id       TEXT NOT NULL,
			node_id       TEXT NOT NULL,
			attempt       INTEGER NOT NULL DEFAULT 1,
			status        TEXT NOT NULL,
			output_json   TEXT,
			idempotency_key TEXT,
			taint_level   INTEGER NOT NULL,
			started_at    INTEGER,
			completed_at  INTEGER,
			error         TEXT,
			PRIMARY KEY (task_id, node_id, attempt)
		);
	`)
	require.NoError(t, err)
	bb := NewSQLiteBlackboard(db)
	executor := NewStateGraphExecutor(bb)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	spec := protocol.WorkflowGraphSpec{
		Nodes: []protocol.WorkflowNodeSpec{
			{ID: "n1", CapabilityType: "echo", MaxVisits: 1, IsEntry: true},
			{ID: "n2", CapabilityType: "echo", MaxVisits: 1},
		},
		Edges: []protocol.WorkflowEdgeSpec{
			{From: "n1", To: "n2"},
		},
	}

	taskID := "task-checkpoint-test"

	// Pre-fill a checkpoint for n1 as "done"
	_ = executor.chkRepo.UpsertCheckpoint(ctx, types.TaskCheckpointRow{
		TaskID:      taskID,
		NodeID:      "n1",
		Attempt:     1,
		Status:      "done",
		OutputJSON:  `{"res":"skipped"}`,
		CompletedAt: time.Now().UnixMilli(),
	})

	errCh := make(chan error, 1)
	go func() {
		errCh <- executor.Execute(ctx, taskID, spec)
	}()

	// Wait slightly for Execute to process the synthetic event
	time.Sleep(200 * time.Millisecond)

	rows, err := db.QueryContext(ctx, "SELECT task_id FROM tasks WHERE task_id LIKE 'task-checkpoint-test-%'")
	require.NoError(t, err)
	defer rows.Close()

	var tasks []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			tasks = append(tasks, id)
		}
	}

	assert.Len(t, tasks, 1)
	assert.Contains(t, tasks[0], "n2") // n2 should be triggered since n1 is skipped via checkpoint

	cancel() // stop executor
}
