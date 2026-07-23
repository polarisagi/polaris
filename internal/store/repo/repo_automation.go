package repo

import (
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"

	"context"
	"database/sql"

	"github.com/polarisagi/polaris/internal/protocol"
)

type SQLiteAutomationRepository struct {
	db protocol.SQLQuerier
}

var _ repo.AutomationRepository = (*SQLiteAutomationRepository)(nil)

func NewSQLiteAutomationRepository(db protocol.SQLQuerier) *SQLiteAutomationRepository {
	return &SQLiteAutomationRepository{db: db}
}

func (r *SQLiteAutomationRepository) CreateAutomation(ctx context.Context, row repo.AutomationRow) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO automations (
			id, name, prompt, trigger_type, cron_schedule, channel_id,
			working_dir, env_type, reasoning_effort, result_action,
			sandbox_level, cedar_rules_json, enabled, requires_hitl, risk_level,
			last_run_at, next_run_at, run_count, last_run_status, last_run_error,
			created_at, updated_at, event_filter
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.ID, row.Name, row.Prompt, row.TriggerType, row.CronSchedule, row.ChannelID,
		row.WorkingDir, row.EnvType, row.ReasoningEffort, row.ResultAction,
		row.SandboxLevel, row.CedarRulesJSON, row.Enabled, row.RequiresHITL, row.RiskLevel,
		row.LastRunAt, row.NextRunAt, row.RunCount, row.LastRunStatus, row.LastRunError,
		row.CreatedAt, row.UpdatedAt, row.EventFilter)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteAutomationRepository.CreateAutomation", err)
	}
	return nil
}

func (r *SQLiteAutomationRepository) UpdateAutomation(ctx context.Context, row repo.AutomationRow) error {
	_, err := r.db.ExecContext(ctx, `
			UPDATE automations SET
				name=?, prompt=?, cron_schedule=?, channel_id=?,
				working_dir=?, env_type=?, reasoning_effort=?, result_action=?,
				sandbox_level=?, cedar_rules_json=?, enabled=?,
				requires_hitl=?, risk_level=?,
				next_run_at=?, updated_at=?, event_filter=?
			WHERE id=?`,
		row.Name, row.Prompt, row.CronSchedule, row.ChannelID,
		row.WorkingDir, row.EnvType, row.ReasoningEffort, row.ResultAction,
		row.SandboxLevel, row.CedarRulesJSON, row.Enabled,
		row.RequiresHITL, row.RiskLevel,
		row.NextRunAt, row.UpdatedAt, row.EventFilter, row.ID)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteAutomationRepository.UpdateAutomation", err)
	}
	return nil
}

func (r *SQLiteAutomationRepository) DeleteAutomation(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM automations WHERE id=?`, id)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteAutomationRepository.DeleteAutomation", err)
	}
	return nil
}

func (r *SQLiteAutomationRepository) GetAutomation(ctx context.Context, id string) (*repo.AutomationRow, error) {
	var row repo.AutomationRow
	var enabledInt, hitlInt int
	err := r.db.QueryRowContext(ctx, `
		SELECT id, name, prompt, trigger_type, cron_schedule, channel_id,
		       working_dir, env_type, reasoning_effort, result_action,
		       sandbox_level, cedar_rules_json, enabled, requires_hitl, risk_level,
		       last_run_at, next_run_at, run_count, last_run_status, last_run_error,
		       created_at, updated_at, event_filter
		FROM automations WHERE id=?`, id).Scan(
		&row.ID, &row.Name, &row.Prompt, &row.TriggerType, &row.CronSchedule, &row.ChannelID,
		&row.WorkingDir, &row.EnvType, &row.ReasoningEffort, &row.ResultAction,
		&row.SandboxLevel, &row.CedarRulesJSON, &enabledInt, &hitlInt, &row.RiskLevel,
		&row.LastRunAt, &row.NextRunAt, &row.RunCount, &row.LastRunStatus, &row.LastRunError,
		&row.CreatedAt, &row.UpdatedAt, &row.EventFilter)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteAutomationRepository.GetAutomation", err)
	}
	row.Enabled = enabledInt == 1
	row.RequiresHITL = hitlInt == 1
	return &row, nil
}

func (r *SQLiteAutomationRepository) ListAutomations(ctx context.Context) ([]repo.AutomationRow, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, name, prompt, trigger_type, cron_schedule, channel_id,
		       working_dir, env_type, reasoning_effort, result_action,
		       sandbox_level, cedar_rules_json, enabled, requires_hitl, risk_level,
		       last_run_at, next_run_at, run_count, last_run_status, last_run_error,
		       created_at, updated_at, event_filter
		FROM automations ORDER BY created_at DESC`)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteAutomationRepository.ListAutomations", err)
	}
	defer rows.Close()

	var result []repo.AutomationRow
	for rows.Next() {
		var row repo.AutomationRow
		var enabledInt, hitlInt int
		if err := rows.Scan(
			&row.ID, &row.Name, &row.Prompt, &row.TriggerType, &row.CronSchedule, &row.ChannelID,
			&row.WorkingDir, &row.EnvType, &row.ReasoningEffort, &row.ResultAction,
			&row.SandboxLevel, &row.CedarRulesJSON, &enabledInt, &hitlInt, &row.RiskLevel,
			&row.LastRunAt, &row.NextRunAt, &row.RunCount, &row.LastRunStatus, &row.LastRunError,
			&row.CreatedAt, &row.UpdatedAt, &row.EventFilter); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteAutomationRepository.ListAutomations scan", err)
		}
		row.Enabled = enabledInt == 1
		row.RequiresHITL = hitlInt == 1
		result = append(result, row)
	}
	return result, rows.Err()
}

func (r *SQLiteAutomationRepository) ListDueAutomations(ctx context.Context, nowRFC3339 string) ([]repo.AutomationRow, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, name, prompt, trigger_type, cron_schedule, channel_id,
		       working_dir, env_type, reasoning_effort, result_action,
		       sandbox_level, cedar_rules_json, enabled, requires_hitl, risk_level,
		       last_run_at, next_run_at, run_count, last_run_status, last_run_error,
		       created_at, updated_at, event_filter
		FROM automations
		WHERE enabled=1
		  AND circuit_open=0
		  AND (trigger_type='cron' OR trigger_type='both')
		  AND cron_schedule != ''
		  AND (next_run_at = '' OR next_run_at <= ?)
		  AND last_run_status != 'running'`, nowRFC3339)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteAutomationRepository.ListDueAutomations", err)
	}
	defer rows.Close()

	var result []repo.AutomationRow
	for rows.Next() {
		var row repo.AutomationRow
		var enabledInt, hitlInt int
		if err := rows.Scan(
			&row.ID, &row.Name, &row.Prompt, &row.TriggerType, &row.CronSchedule, &row.ChannelID,
			&row.WorkingDir, &row.EnvType, &row.ReasoningEffort, &row.ResultAction,
			&row.SandboxLevel, &row.CedarRulesJSON, &enabledInt, &hitlInt, &row.RiskLevel,
			&row.LastRunAt, &row.NextRunAt, &row.RunCount, &row.LastRunStatus, &row.LastRunError,
			&row.CreatedAt, &row.UpdatedAt, &row.EventFilter); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteAutomationRepository.ListDueAutomations scan", err)
		}
		row.Enabled = enabledInt == 1
		row.RequiresHITL = hitlInt == 1
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "rows iteration error", err)
	}
	return result, nil
}

func (r *SQLiteAutomationRepository) ListEventAutomations(ctx context.Context) ([]repo.AutomationRow, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, name, prompt, trigger_type, cron_schedule, channel_id,
		       working_dir, env_type, reasoning_effort, result_action,
		       sandbox_level, cedar_rules_json, enabled, requires_hitl, risk_level,
		       last_run_at, next_run_at, run_count, last_run_status, last_run_error,
		       created_at, updated_at, event_filter
		FROM automations
		WHERE enabled=1
		  AND circuit_open=0
		  AND (trigger_type='event' OR trigger_type='both')
		  AND event_filter != '' AND event_filter != '{}'
		  AND last_run_status != 'running'`)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteAutomationRepository.ListEventAutomations", err)
	}
	defer rows.Close()

	var result []repo.AutomationRow
	for rows.Next() {
		var row repo.AutomationRow
		var enabledInt, hitlInt int
		if err := rows.Scan(
			&row.ID, &row.Name, &row.Prompt, &row.TriggerType, &row.CronSchedule, &row.ChannelID,
			&row.WorkingDir, &row.EnvType, &row.ReasoningEffort, &row.ResultAction,
			&row.SandboxLevel, &row.CedarRulesJSON, &enabledInt, &hitlInt, &row.RiskLevel,
			&row.LastRunAt, &row.NextRunAt, &row.RunCount, &row.LastRunStatus, &row.LastRunError,
			&row.CreatedAt, &row.UpdatedAt, &row.EventFilter); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteAutomationRepository.ListEventAutomations scan", err)
		}
		row.Enabled = enabledInt == 1
		row.RequiresHITL = hitlInt == 1
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "rows iteration error", err)
	}
	return result, nil
}

func (r *SQLiteAutomationRepository) UpdateAutomationStatus(ctx context.Context, id, lastRunStatus string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE automations SET last_run_status=? WHERE id=?`, lastRunStatus, id)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteAutomationRepository.UpdateAutomationStatus", err)
	}
	return nil
}

func (r *SQLiteAutomationRepository) UpdateAutomationStatusAndSchedule(ctx context.Context, id, lastRunStatus, lastRunAt, nextRunAt string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE automations
		SET last_run_status=?, last_run_at=?, next_run_at=?, updated_at=datetime('now')
		WHERE id=?`, lastRunStatus, lastRunAt, nextRunAt, id)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteAutomationRepository.UpdateAutomationStatusAndSchedule", err)
	}
	return nil
}

func (r *SQLiteAutomationRepository) CreateRun(ctx context.Context, row repo.AutomationRunRow) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO automation_runs (id, automation_id, trigger, status, session_id, started_at, finished_at, error_msg, prompt_snapshot)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, row.ID, row.AutomationID, row.Trigger, row.Status, row.SessionID, row.StartedAt, row.FinishedAt, row.ErrorMsg, row.PromptSnapshot)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteAutomationRepository.CreateRun", err)
	}
	return nil
}

func (r *SQLiteAutomationRepository) UpdateRunStatus(ctx context.Context, id, status, errorMsg, completedAt string, durationMs int64) error {
	if durationMs > 0 {
		_, err := r.db.ExecContext(ctx, `UPDATE automation_runs SET status=?, error=?, completed_at=?, duration_ms=? WHERE id=?`,
			status, errorMsg, completedAt, durationMs, id)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "error", err)
		}
		return nil
	}
	_, err := r.db.ExecContext(ctx, `UPDATE automation_runs SET status=?, error=?, completed_at=? WHERE id=?`,
		status, errorMsg, completedAt, id)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "error", err)
	}
	return nil
}

func (r *SQLiteAutomationRepository) DeleteRunsByAutomationID(ctx context.Context, automationID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM automation_runs WHERE automation_id=?`, automationID)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteAutomationRepository.DeleteRunsByAutomationID", err)
	}
	return nil
}

func (r *SQLiteAutomationRepository) ListRunsByAutomationID(ctx context.Context, automationID string, limit int) ([]repo.AutomationRunRow, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, automation_id, trigger, status, session_id,
		       started_at, finished_at, error_msg, prompt_snapshot
		FROM automation_runs
		WHERE automation_id=?
		ORDER BY started_at DESC LIMIT ?`, automationID, limit)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteAutomationRepository.ListRunsByAutomationID", err)
	}
	defer rows.Close()

	var list []repo.AutomationRunRow
	for rows.Next() {
		var run repo.AutomationRunRow
		if err := rows.Scan(
			&run.ID, &run.AutomationID, &run.Trigger, &run.Status, &run.SessionID,
			&run.StartedAt, &run.FinishedAt, &run.ErrorMsg, &run.PromptSnapshot,
		); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteAutomationRepository.ListRunsByAutomationID scan", err)
		}
		list = append(list, run)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "rows iteration error", err)
	}
	return list, nil
}

func (r *SQLiteAutomationRepository) TimeoutRuns(ctx context.Context, startedBefore string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE automation_runs SET status='timeout', error='execution timeout' WHERE status='running' AND started_at < ?`, startedBefore)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteAutomationRepository.TimeoutRuns", err)
	}
	return nil
}

func (r *SQLiteAutomationRepository) UpdateAutomationStats(ctx context.Context, id, status, errMsg, finishedAt string, circuitBreakThreshold int) (int, error) {
	if status == "error" {
		_, err := r.db.ExecContext(ctx, "UPDATE automations SET last_run_status=?, last_run_error=?, run_count=run_count+1, failure_count=failure_count+1, circuit_open=CASE WHEN failure_count+1 >= ? THEN 1 ELSE circuit_open END, circuit_opened_at=CASE WHEN failure_count+1 >= ? AND circuit_open=0 THEN ? ELSE circuit_opened_at END, updated_at=? WHERE id=?", status, errMsg, circuitBreakThreshold, circuitBreakThreshold, finishedAt, finishedAt, id)
		if err != nil {
			if err != nil {
				return 0, apperr.Wrap(apperr.CodeInternal, "error", err)
			}
			return 0, nil
		}
		var circuitOpen int
		_ = r.db.QueryRowContext(ctx, "SELECT circuit_open FROM automations WHERE id=?", id).Scan(&circuitOpen)
		return circuitOpen, nil
	}
	_, err := r.db.ExecContext(ctx, "UPDATE automations SET last_run_status=?, last_run_error='', run_count=run_count+1, failure_count=0, circuit_open=0, circuit_opened_at='', updated_at=? WHERE id=?", status, finishedAt, id)
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "error", err)
	}
	return 0, nil
}
