package repo

import (
	"context"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// SQLiteSubagentRunRepository subagent_runs（042）的写入实现，供 orchestrator.SubagentRunRecorder 消费。
type SQLiteSubagentRunRepository struct {
	db protocol.SQLQuerier
}

func NewSQLiteSubagentRunRepository(db protocol.SQLQuerier) *SQLiteSubagentRunRepository {
	return &SQLiteSubagentRunRepository{db: db}
}

// Start 用 upsert：委派任务被重试时 AgentID(=task id) 复用，重置为 running 而非因主键冲突丢记录。
func (r *SQLiteSubagentRunRepository) Start(ctx context.Context, row repo.SubagentRunRow) error {
	var taskID any
	if row.TaskID != "" {
		taskID = row.TaskID
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO subagent_runs(id, parent_session_id, child_session_id, task_id, agent_type, entry, status, prompt, started_at)
		 VALUES(?, ?, ?, ?, ?, ?, 'running', ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   parent_session_id=excluded.parent_session_id, child_session_id=excluded.child_session_id,
		   task_id=excluded.task_id, agent_type=excluded.agent_type, entry=excluded.entry,
		   status='running', prompt=excluded.prompt, output='', error='', continuations=0,
		   started_at=excluded.started_at, finished_at=NULL`,
		row.ID, row.ParentSessionID, row.ChildSessionID, taskID, row.AgentType, row.Entry, row.Prompt, row.StartedAtMs)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteSubagentRunRepository.Start", err)
	}
	return nil
}

func (r *SQLiteSubagentRunRepository) Finish(ctx context.Context, id, status, output, errMsg string, continuations int) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE subagent_runs SET status=?, output=?, error=?, continuations=?, finished_at=? WHERE id=?`,
		status, output, errMsg, continuations, time.Now().UnixMilli(), id)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteSubagentRunRepository.Finish", err)
	}
	return nil
}
