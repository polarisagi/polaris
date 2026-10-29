package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestStateGraphExecutor_CheckpointResume(t *testing.T) {
	// setupTestDB 已通过 SSoT schema（035_task_checkpoints.sql）建好
	// task_checkpoints 表，此前这里额外内联复制一份 CREATE TABLE 会与之冲突
	// （table already exists），已移除（2026-08-02，见 schema_test_helper_test.go）。
	db := setupTestDB(t)
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
	require.NoError(t, executor.chkRepo.UpsertCheckpoint(ctx, types.TaskCheckpointRow{
		TaskID:      taskID,
		NodeID:      "n1",
		Attempt:     1,
		Status:      "done",
		OutputJSON:  `{"res":"skipped"}`,
		CompletedAt: time.Now().UnixMilli(),
	}))

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
