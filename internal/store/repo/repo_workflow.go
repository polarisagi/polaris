package repo

import (
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"

	"context"
	"database/sql"
	"encoding/json"
)

type SQLiteWorkflowRepository struct {
	db *sql.DB
}

var _ repo.WorkflowRepository = (*SQLiteWorkflowRepository)(nil)

func NewSQLiteWorkflowRepository(db *sql.DB) *SQLiteWorkflowRepository {
	return &SQLiteWorkflowRepository{db: db}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// marshalDependsOn 序列化前驱 step ID 列表为 JSON 数组，nil/空 → "[]"（与
// 029_workflows.sql depends_on 列默认值一致，避免 NULL/空字符串二义）。
func marshalDependsOn(deps []string) string {
	if len(deps) == 0 {
		return "[]"
	}
	b, err := json.Marshal(deps)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func (r *SQLiteWorkflowRepository) CreateWorkflowWithSteps(ctx context.Context, wf repo.WorkflowRow, steps []repo.WorkflowStepRow) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	defer tx.Rollback() //nolint:errcheck

	wfType := wf.Type
	if wfType == "" {
		wfType = "chain"
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workflows(id, type, name, description, trigger_type, cron_schedule, enabled,
		                      next_run_at, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		wf.ID, wfType, wf.Name, wf.Description, wf.TriggerType, wf.CronSchedule,
		boolToInt(wf.Enabled), wf.NextRunAt, wf.CreatedAt, wf.UpdatedAt,
	); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}

	for _, st := range steps {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO workflow_steps(id, workflow_id, seq, name, automation_id, prompt,
									   reasoning_effort, working_dir, input_from_prev,
									   depends_on, capability_type, compensation_tool,
									   compensation_args, max_retries)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			st.ID, st.WorkflowID, st.Seq, st.Name, st.AutomationID, st.Prompt,
			st.ReasoningEffort, st.WorkingDir, boolToInt(st.InputFromPrev),
			marshalDependsOn(st.DependsOn), st.CapabilityType, st.CompensationTool,
			st.CompensationArgs, st.MaxRetries,
		); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "db error", err)
		}
	}

	return tx.Commit()
}

func (r *SQLiteWorkflowRepository) UpdateWorkflowWithSteps(ctx context.Context, wf repo.WorkflowRow, steps []repo.WorkflowStepRow, updateSteps bool) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	defer tx.Rollback() //nolint:errcheck

	wfType := wf.Type
	if wfType == "" {
		wfType = "chain"
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE workflows SET type=?, name=?, description=?, trigger_type=?, cron_schedule=?,
		       enabled=?, next_run_at=?, updated_at=?
		WHERE id=?`,
		wfType, wf.Name, wf.Description, wf.TriggerType, wf.CronSchedule,
		boolToInt(wf.Enabled), wf.NextRunAt, wf.UpdatedAt, wf.ID,
	); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}

	if updateSteps {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM workflow_steps WHERE workflow_id=?`, wf.ID); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "db error", err)
		}
		for _, st := range steps {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO workflow_steps(id, workflow_id, seq, name, automation_id, prompt,
										   reasoning_effort, working_dir, input_from_prev,
										   depends_on, capability_type, compensation_tool,
										   compensation_args, max_retries)
				VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				st.ID, st.WorkflowID, st.Seq, st.Name, st.AutomationID, st.Prompt,
				st.ReasoningEffort, st.WorkingDir, boolToInt(st.InputFromPrev),
				marshalDependsOn(st.DependsOn), st.CapabilityType, st.CompensationTool,
				st.CompensationArgs, st.MaxRetries,
			); err != nil {
				return apperr.Wrap(apperr.CodeInternal, "db error", err)
			}
		}
	}

	return tx.Commit()
}

func (r *SQLiteWorkflowRepository) DeleteWorkflow(ctx context.Context, wfID string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	defer tx.Rollback() //nolint:errcheck

	for _, q := range []string{
		`DELETE FROM workflow_runs WHERE workflow_id=?`,
		`DELETE FROM workflow_steps WHERE workflow_id=?`,
		`DELETE FROM workflows WHERE id=?`,
	} {
		if _, err := tx.ExecContext(ctx, q, wfID); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "db error", err)
		}
	}

	return tx.Commit()
}

func (r *SQLiteWorkflowRepository) CreateWorkflowRun(ctx context.Context, runID, wfID, trigger, status string, currentStep, totalSteps int, startedAt string) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO workflow_runs(id, workflow_id, trigger, status, current_step, total_steps, started_at)
		VALUES(?,?,?,?,?,?,?)`,
		runID, wfID, trigger, status, currentStep, totalSteps, startedAt,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	return nil
}

func (r *SQLiteWorkflowRepository) UpdateWorkflowRunStatus(ctx context.Context, runID, status, finishedAt, errMsg, stepOutputs string, currentStep int) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE workflow_runs
		SET status=?, finished_at=?, error_msg=?, step_outputs=?, current_step=?
		WHERE id=?`,
		status, finishedAt, errMsg, stepOutputs, currentStep, runID,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	return nil
}

func (r *SQLiteWorkflowRepository) UpdateWorkflowRunCurrentStep(ctx context.Context, runID string, currentStep int) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE workflow_runs SET current_step=? WHERE id=?`, currentStep, runID,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	return nil
}

func (r *SQLiteWorkflowRepository) UpdateWorkflowLastRun(ctx context.Context, wfID, lastRunAt, nextRunAt, lastRunStatus, updatedAt string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE workflows SET last_run_at=?, next_run_at=?, last_run_status=?, updated_at=?
		WHERE id=?`,
		lastRunAt, nextRunAt, lastRunStatus, updatedAt, wfID,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	return nil
}

func (r *SQLiteWorkflowRepository) AppendWorkflowRunStepOutput(ctx context.Context, runID string, stepOutputJSON []byte) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE workflow_runs SET step_outputs = json_insert(step_outputs, '$[#]', json(?)) WHERE id=?`,
		string(stepOutputJSON), runID,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	return nil
}

func (r *SQLiteWorkflowRepository) IncrementWorkflowRunCurrentStep(ctx context.Context, runID string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE workflow_runs SET current_step = current_step + 1 WHERE id=?`, runID,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	return nil
}

func (r *SQLiteWorkflowRepository) UpdateWorkflowStats(ctx context.Context, wfID, status, errMsg, finishedAt string, circuitBreakThreshold int) error {
	if status == "error" {
		_, err := r.db.ExecContext(ctx, `
			UPDATE workflows
			SET last_run_status=?, last_run_error=?,
			    run_count=run_count+1,
			    failure_count=failure_count+1,
			    circuit_open=CASE WHEN failure_count+1 >= ? THEN 1 ELSE circuit_open END,
			    circuit_opened_at=CASE WHEN failure_count+1 >= ? AND circuit_open=0
			                          THEN ? ELSE circuit_opened_at END,
			    updated_at=?
			WHERE id=?`,
			status, errMsg,
			circuitBreakThreshold, circuitBreakThreshold, finishedAt,
			finishedAt, wfID,
		)
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE workflows
		SET last_run_status=?, last_run_error='',
		    run_count=run_count+1,
		    failure_count=0, circuit_open=0, circuit_opened_at='',
		    updated_at=?
		WHERE id=?`,
		status, finishedAt, wfID,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	return nil
}
