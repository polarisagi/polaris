package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
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
	resultAction := wf.ResultAction
	if resultAction == "" {
		resultAction = "session"
	}
	source := wf.Source
	if source == "" {
		source = "user"
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workflows(id, type, name, description, trigger_type, cron_schedule, enabled,
		                      result_action, deliver_from, source,
		                      next_run_at, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		wf.ID, wfType, wf.Name, wf.Description, wf.TriggerType, wf.CronSchedule,
		boolToInt(wf.Enabled), resultAction, wf.DeliverFrom, source,
		wf.NextRunAt, wf.CreatedAt, wf.UpdatedAt,
	); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}

	for _, st := range steps {
		kind := st.Kind
		if kind == "" {
			kind = "agent"
		}
		configJSON := st.ConfigJSON
		if configJSON == "" {
			configJSON = "{}"
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO workflow_steps(id, workflow_id, seq, name, automation_id, prompt,
									   reasoning_effort, working_dir, input_from_prev,
									   depends_on, capability_type, compensation_tool,
									   compensation_args, max_retries,
									   step_key, kind, config_json)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			st.ID, st.WorkflowID, st.Seq, st.Name, st.AutomationID, st.Prompt,
			st.ReasoningEffort, st.WorkingDir, boolToInt(st.InputFromPrev),
			marshalDependsOn(st.DependsOn), st.CapabilityType, st.CompensationTool,
			st.CompensationArgs, st.MaxRetries,
			st.StepKey, kind, configJSON,
		); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "db error", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteWorkflowRepository.CreateWorkflowWithSteps commit", err)
	}
	return nil
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
	resultAction := wf.ResultAction
	if resultAction == "" {
		resultAction = "session"
	}
	source := wf.Source
	if source == "" {
		source = "user"
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE workflows SET type=?, name=?, description=?, trigger_type=?, cron_schedule=?,
		       enabled=?, result_action=?, deliver_from=?, source=?, next_run_at=?, updated_at=?
		WHERE id=?`,
		wfType, wf.Name, wf.Description, wf.TriggerType, wf.CronSchedule,
		boolToInt(wf.Enabled), resultAction, wf.DeliverFrom, source,
		wf.NextRunAt, wf.UpdatedAt, wf.ID,
	); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}

	if updateSteps {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM workflow_steps WHERE workflow_id=?`, wf.ID); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "db error", err)
		}
		for _, st := range steps {
			kind := st.Kind
			if kind == "" {
				kind = "agent"
			}
			configJSON := st.ConfigJSON
			if configJSON == "" {
				configJSON = "{}"
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO workflow_steps(id, workflow_id, seq, name, automation_id, prompt,
										   reasoning_effort, working_dir, input_from_prev,
										   depends_on, capability_type, compensation_tool,
										   compensation_args, max_retries,
										   step_key, kind, config_json)
				VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				st.ID, st.WorkflowID, st.Seq, st.Name, st.AutomationID, st.Prompt,
				st.ReasoningEffort, st.WorkingDir, boolToInt(st.InputFromPrev),
				marshalDependsOn(st.DependsOn), st.CapabilityType, st.CompensationTool,
				st.CompensationArgs, st.MaxRetries,
				st.StepKey, kind, configJSON,
			); err != nil {
				return apperr.Wrap(apperr.CodeInternal, "db error", err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteWorkflowRepository.UpdateWorkflowWithSteps commit", err)
	}
	return nil
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

	if err := tx.Commit(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteWorkflowRepository.DeleteWorkflow commit", err)
	}
	return nil
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

func (r *SQLiteWorkflowRepository) ResumeWorkflowRun(ctx context.Context, runID, updatedAt string) (bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	defer func() { _ = tx.Rollback() }()

	const keepNonError = `SELECT COALESCE(json_group_array(json(value)), '[]') FROM json_each(workflow_runs.step_outputs)
		WHERE COALESCE(json_extract(value, '$.status'), '') != 'error'`
	const countNonError = `SELECT COUNT(*) FROM json_each(workflow_runs.step_outputs)
		WHERE COALESCE(json_extract(value, '$.status'), '') != 'error'`
	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_runs
		SET status='running', finished_at='', error_msg='',
		    step_outputs=(`+keepNonError+`), current_step=(`+countNonError+`)
		WHERE id=? AND status IN ('interrupted','error')`, runID)
	if err != nil {
		return false, apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE workflows SET last_run_status='running', last_run_error='', updated_at=?
		WHERE id=(SELECT workflow_id FROM workflow_runs WHERE id=?)`, updatedAt, runID); err != nil {
		return false, apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	if err := tx.Commit(); err != nil {
		return false, apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	return true, nil
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

func (r *SQLiteWorkflowRepository) SaveRunData(ctx context.Context, runID, stepKey, status string, dataJSON []byte, itemCount int) error {
	dj := string(dataJSON)
	if dj == "" {
		dj = "{}"
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO workflow_run_data (run_id, step_key, status, data_json, item_count, created_at)
		VALUES (?, ?, ?, ?, ?, strftime('%Y-%m-%dT%H:%M:%SZ','now'))`,
		runID, stepKey, status, dj, itemCount,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SaveRunData failed", err)
	}
	return nil
}

func (r *SQLiteWorkflowRepository) LoadRunData(ctx context.Context, runID, stepKey string) (status string, dataJSON []byte, err error) {
	var dj string
	err = r.db.QueryRowContext(ctx, `
		SELECT status, data_json FROM workflow_run_data WHERE run_id=? AND step_key=?`,
		runID, stepKey,
	).Scan(&status, &dj)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil, nil
		}
		return "", nil, apperr.Wrap(apperr.CodeInternal, "LoadRunData failed", err)
	}
	return status, []byte(dj), nil
}

func (r *SQLiteWorkflowRepository) SeenKeys(ctx context.Context, workflowID string, keys []string) (map[string]bool, error) {
	res := make(map[string]bool)
	if len(keys) == 0 {
		return res, nil
	}

	const batchSize = 500
	for i := 0; i < len(keys); i += batchSize {
		end := i + batchSize
		if end > len(keys) {
			end = len(keys)
		}
		batch := keys[i:end]

		query := `SELECT item_key FROM workflow_seen_items WHERE workflow_id=? AND item_key IN (` +
			strings.Repeat("?,", len(batch)-1) + `?)`
		args := make([]any, 0, len(batch)+1)
		args = append(args, workflowID)
		for _, k := range batch {
			args = append(args, k)
		}

		rows, err := r.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SeenKeys query failed", err)
		}
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				rows.Close()
				return nil, apperr.Wrap(apperr.CodeInternal, "SeenKeys scan failed", err)
			}
			res[k] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, apperr.Wrap(apperr.CodeInternal, "SeenKeys rows failed", err)
		}
		rows.Close()
	}
	return res, nil
}

func (r *SQLiteWorkflowRepository) MarkSeen(ctx context.Context, workflowID string, keys []string, at string) error {
	if len(keys) == 0 {
		return nil
	}
	if at == "" {
		at = time.Now().UTC().Format(time.RFC3339)
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "MarkSeen begin tx", err)
	}
	defer tx.Rollback() //nolint:errcheck

	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR IGNORE INTO workflow_seen_items (workflow_id, item_key, first_seen_at)
		VALUES (?, ?, ?)`)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "MarkSeen prepare stmt", err)
	}
	defer stmt.Close()

	for _, k := range keys {
		if k == "" {
			continue
		}
		if _, err := stmt.ExecContext(ctx, workflowID, k, at); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "MarkSeen insert", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "MarkSeen commit", err)
	}
	return nil
}

func (r *SQLiteWorkflowRepository) LastSuccessfulRunStart(ctx context.Context, workflowID string) (string, error) {
	var startedAt string
	err := r.db.QueryRowContext(ctx, `
		SELECT started_at FROM workflow_runs
		WHERE workflow_id=? AND status='ok' AND skipped=0
		ORDER BY started_at DESC LIMIT 1`,
		workflowID,
	).Scan(&startedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", apperr.Wrap(apperr.CodeInternal, "LastSuccessfulRunStart failed", err)
	}
	return startedAt, nil
}

func (r *SQLiteWorkflowRepository) SetRunSkipped(ctx context.Context, runID string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE workflow_runs SET skipped=1 WHERE id=?`, runID)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SetRunSkipped failed", err)
	}
	return nil
}

func (r *SQLiteWorkflowRepository) PruneRunData(ctx context.Context, workflowID string, keepRuns int) error {
	if keepRuns <= 0 {
		return nil
	}
	_, err := r.db.ExecContext(ctx, `
		DELETE FROM workflow_run_data
		WHERE run_id IN (
			SELECT id FROM workflow_runs
			WHERE workflow_id=?
			ORDER BY started_at DESC
			LIMIT -1 OFFSET ?
		)`,
		workflowID, keepRuns,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "PruneRunData failed", err)
	}
	return nil
}

func (r *SQLiteWorkflowRepository) PruneSeen(ctx context.Context, olderThan string) error {
	if olderThan == "" {
		return nil
	}
	_, err := r.db.ExecContext(ctx, `DELETE FROM workflow_seen_items WHERE first_seen_at < ?`, olderThan)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "PruneSeen failed", err)
	}
	return nil
}

func (r *SQLiteWorkflowRepository) RunLLMUsage(ctx context.Context, sessionIDs []string) (inTok, outTok int64, costUSD float64, err error) {
	if len(sessionIDs) == 0 {
		return 0, 0, 0, nil
	}

	const batchSize = 500
	for i := 0; i < len(sessionIDs); i += batchSize {
		end := i + batchSize
		if end > len(sessionIDs) {
			end = len(sessionIDs)
		}
		batch := sessionIDs[i:end]

		query := `SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(cost_usd), 0)
			FROM llm_calls WHERE session_id IN (` + strings.Repeat("?,", len(batch)-1) + `?)`
		args := make([]any, len(batch))
		for j, id := range batch {
			args[j] = id
		}

		var bIn, bOut int64
		var bCost float64
		if err := r.db.QueryRowContext(ctx, query, args...).Scan(&bIn, &bOut, &bCost); err != nil {
			return 0, 0, 0, apperr.Wrap(apperr.CodeInternal, "RunLLMUsage query failed", err)
		}
		inTok += bIn
		outTok += bOut
		costUSD += bCost
	}
	return inTok, outTok, costUSD, nil
}
