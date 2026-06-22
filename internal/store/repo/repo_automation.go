package repo

import (
	"github.com/polarisagi/polaris/internal/protocol/repo"

	"context"
	"database/sql"
	"fmt"

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
			INSERT INTO automations(
				id, name, prompt, trigger_type, cron_schedule, channel_id,
				working_dir, reasoning_effort, result_action,
				sandbox_level, cedar_rules_json, enabled,
				requires_hitl, risk_level,
				next_run_at, created_at, updated_at, event_filter
			) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		row.ID, row.Name, row.Prompt, row.TriggerType, row.CronSchedule, row.ChannelID,
		row.WorkingDir, row.ReasoningEffort, row.ResultAction,
		row.SandboxLevel, row.CedarRulesJSON, row.Enabled,
		row.RequiresHITL, row.RiskLevel,
		row.NextRunAt, row.CreatedAt, row.UpdatedAt, row.EventFilter)
	if err != nil {
		return fmt.Errorf("SQLiteAutomationRepository.CreateAutomation: %w", err)
	}
	return nil
}

func (r *SQLiteAutomationRepository) UpdateAutomation(ctx context.Context, row repo.AutomationRow) error {
	_, err := r.db.ExecContext(ctx, `
			UPDATE automations SET
				name=?, prompt=?, cron_schedule=?, channel_id=?,
				working_dir=?, reasoning_effort=?, result_action=?,
				sandbox_level=?, cedar_rules_json=?, enabled=?,
				requires_hitl=?, risk_level=?,
				next_run_at=?, updated_at=?, event_filter=?
			WHERE id=?`,
		row.Name, row.Prompt, row.CronSchedule, row.ChannelID,
		row.WorkingDir, row.ReasoningEffort, row.ResultAction,
		row.SandboxLevel, row.CedarRulesJSON, row.Enabled,
		row.RequiresHITL, row.RiskLevel,
		row.NextRunAt, row.UpdatedAt, row.EventFilter, row.ID)
	if err != nil {
		return fmt.Errorf("SQLiteAutomationRepository.UpdateAutomation: %w", err)
	}
	return nil
}

func (r *SQLiteAutomationRepository) DeleteAutomation(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM automations WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("SQLiteAutomationRepository.DeleteAutomation: %w", err)
	}
	return nil
}

func (r *SQLiteAutomationRepository) GetAutomation(ctx context.Context, id string) (*repo.AutomationRow, error) {
	var row repo.AutomationRow
	var enabledInt, hitlInt int
	err := r.db.QueryRowContext(ctx, `
		SELECT id, name, prompt, trigger_type, cron_schedule, channel_id,
		       working_dir, reasoning_effort, result_action,
		       sandbox_level, cedar_rules_json, enabled,
		       requires_hitl, risk_level, next_run_at, last_run_status, created_at, updated_at, event_filter
		FROM automations WHERE id=?`, id).Scan(
		&row.ID, &row.Name, &row.Prompt, &row.TriggerType, &row.CronSchedule, &row.ChannelID,
		&row.WorkingDir, &row.ReasoningEffort, &row.ResultAction,
		&row.SandboxLevel, &row.CedarRulesJSON, &enabledInt,
		&hitlInt, &row.RiskLevel, &row.NextRunAt, &row.LastRunStatus, &row.CreatedAt, &row.UpdatedAt, &row.EventFilter)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("SQLiteAutomationRepository.GetAutomation: %w", err)
	}
	row.Enabled = enabledInt == 1
	row.RequiresHITL = hitlInt == 1
	return &row, nil
}

func (r *SQLiteAutomationRepository) ListAutomations(ctx context.Context) ([]repo.AutomationRow, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, name, prompt, trigger_type, cron_schedule, channel_id,
		       working_dir, reasoning_effort, result_action,
		       sandbox_level, cedar_rules_json, enabled,
		       requires_hitl, risk_level, next_run_at, last_run_status, created_at, updated_at, event_filter
		FROM automations ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("SQLiteAutomationRepository.ListAutomations: %w", err)
	}
	defer rows.Close()

	var result []repo.AutomationRow
	for rows.Next() {
		var row repo.AutomationRow
		var enabledInt, hitlInt int
		if err := rows.Scan(&row.ID, &row.Name, &row.Prompt, &row.TriggerType, &row.CronSchedule, &row.ChannelID,
			&row.WorkingDir, &row.ReasoningEffort, &row.ResultAction,
			&row.SandboxLevel, &row.CedarRulesJSON, &enabledInt,
			&hitlInt, &row.RiskLevel, &row.NextRunAt, &row.LastRunStatus, &row.CreatedAt, &row.UpdatedAt, &row.EventFilter); err != nil {
			return nil, fmt.Errorf("SQLiteAutomationRepository.ListAutomations scan: %w", err)
		}
		row.Enabled = enabledInt == 1
		row.RequiresHITL = hitlInt == 1
		result = append(result, row)
	}
	return result, rows.Err()
}

func (r *SQLiteAutomationRepository) UpdateAutomationStatus(ctx context.Context, id, lastRunStatus string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE automations SET last_run_status=? WHERE id=?`, lastRunStatus, id)
	if err != nil {
		return fmt.Errorf("SQLiteAutomationRepository.UpdateAutomationStatus: %w", err)
	}
	return nil
}

func (r *SQLiteAutomationRepository) UpdateAutomationStatusAndSchedule(ctx context.Context, id, lastRunStatus, lastRunAt, nextRunAt string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE automations
		SET last_run_status=?, last_run_at=?, next_run_at=?, updated_at=datetime('now')
		WHERE id=?`, lastRunStatus, lastRunAt, nextRunAt, id)
	if err != nil {
		return fmt.Errorf("SQLiteAutomationRepository.UpdateAutomationStatusAndSchedule: %w", err)
	}
	return nil
}

func (r *SQLiteAutomationRepository) CreateRun(ctx context.Context, row repo.AutomationRunRow) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO automation_runs (id, automation_id, status, error, started_at)
		VALUES (?, ?, ?, ?, ?)`, row.ID, row.AutomationID, row.Status, row.Error, row.StartedAt)
	if err != nil {
		return fmt.Errorf("SQLiteAutomationRepository.CreateRun: %w", err)
	}
	return nil
}

func (r *SQLiteAutomationRepository) UpdateRunStatus(ctx context.Context, id, status, errorMsg, completedAt string, durationMs int64) error {
	if durationMs > 0 {
		_, err := r.db.ExecContext(ctx, `UPDATE automation_runs SET status=?, error=?, completed_at=?, duration_ms=? WHERE id=?`,
			status, errorMsg, completedAt, durationMs, id)
		if err != nil {
			return fmt.Errorf("error: %w", err)
		}
		return nil
	}
	_, err := r.db.ExecContext(ctx, `UPDATE automation_runs SET status=?, error=?, completed_at=? WHERE id=?`,
		status, errorMsg, completedAt, id)
	if err != nil {
		return fmt.Errorf("error: %w", err)
	}
	return nil
}

func (r *SQLiteAutomationRepository) DeleteRunsByAutomationID(ctx context.Context, automationID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM automation_runs WHERE automation_id=?`, automationID)
	if err != nil {
		return fmt.Errorf("SQLiteAutomationRepository.DeleteRunsByAutomationID: %w", err)
	}
	return nil
}

func (r *SQLiteAutomationRepository) TimeoutRuns(ctx context.Context, startedBefore string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE automation_runs SET status='timeout', error='execution timeout' WHERE status='running' AND started_at < ?`, startedBefore)
	if err != nil {
		return fmt.Errorf("SQLiteAutomationRepository.TimeoutRuns: %w", err)
	}
	return nil
}

func (r *SQLiteAutomationRepository) UpdateAutomationStats(ctx context.Context, id, status, errMsg, finishedAt string, circuitBreakThreshold int) (int, error) {
	if status == "error" {
		_, err := r.db.ExecContext(ctx, "UPDATE automations SET last_run_status=?, last_run_error=?, run_count=run_count+1, failure_count=failure_count+1, circuit_open=CASE WHEN failure_count+1 >= ? THEN 1 ELSE circuit_open END, circuit_opened_at=CASE WHEN failure_count+1 >= ? AND circuit_open=0 THEN ? ELSE circuit_opened_at END, updated_at=? WHERE id=?", status, errMsg, circuitBreakThreshold, circuitBreakThreshold, finishedAt, finishedAt, id)
		if err != nil {
			if err != nil {
				return 0, fmt.Errorf("error: %w", err)
			}
			return 0, nil
		}
		var circuitOpen int
		_ = r.db.QueryRowContext(ctx, "SELECT circuit_open FROM automations WHERE id=?", id).Scan(&circuitOpen)
		return circuitOpen, nil
	}
	_, err := r.db.ExecContext(ctx, "UPDATE automations SET last_run_status=?, last_run_error='', run_count=run_count+1, failure_count=0, circuit_open=0, circuit_opened_at='', updated_at=? WHERE id=?", status, finishedAt, id)
	if err != nil {
		return 0, fmt.Errorf("error: %w", err)
	}
	return 0, nil
}
