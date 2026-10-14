package workflowadmin

import (
	"context"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// ListPipelineSummaries 返回所有 pipeline 类型的自动化工作流摘要（含近 7 天 Token 消耗）。
func (h *WorkflowAdmin) ListPipelineSummaries(ctx context.Context) ([]protocol.AutomationSummary, error) {
	rows, err := h.DB.QueryContext(ctx, `
		SELECT id, name, trigger_type, cron_schedule, source, enabled,
		       COALESCE(last_run_status, ''), COALESCE(last_run_at, ''), COALESCE(next_run_at, '')
		FROM workflows
		ORDER BY created_at DESC`)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "list workflows", err)
	}
	defer rows.Close()

	type wfSummaryRow struct {
		id, name, triggerType, cron, source string
		enabled                             int
		lastStatus, lastRunAt, nextRunAt    string
	}
	var wfRows []wfSummaryRow
	for rows.Next() {
		var r wfSummaryRow
		if err := rows.Scan(&r.id, &r.name, &r.triggerType, &r.cron, &r.source, &r.enabled,
			&r.lastStatus, &r.lastRunAt, &r.nextRunAt); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "scan workflow row", err)
		}
		wfRows = append(wfRows, r)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "iterate workflows", err)
	}

	since7d := time.Now().UTC().AddDate(0, 0, -7).Format(time.RFC3339)
	summaries := make([]protocol.AutomationSummary, 0, len(wfRows))

	for _, r := range wfRows {
		tokens7d := h.query7dTokens(ctx, r.id, since7d)
		summaries = append(summaries, protocol.AutomationSummary{
			ID:            r.id,
			Kind:          "pipeline",
			Name:          r.name,
			TriggerType:   r.triggerType,
			Cron:          r.cron,
			Source:        r.source,
			LastRunStatus: r.lastStatus,
			LastRunAt:     r.lastRunAt,
			NextRunAt:     r.nextRunAt,
			Enabled:       r.enabled == 1,
			CircuitOpen:   false,
			LLMTokens7d:   tokens7d,
		})
	}
	return summaries, nil
}

func (h *WorkflowAdmin) query7dTokens(ctx context.Context, wfID, since7d string) int {
	if h.WorkflowRepo == nil {
		return 0
	}
	runRows, err := h.DB.QueryContext(ctx, `
		SELECT id FROM workflow_runs
		WHERE workflow_id = ? AND started_at >= ?`, wfID, since7d)
	if err != nil {
		return 0
	}
	defer runRows.Close()

	var runIDs []string
	for runRows.Next() {
		var runID string
		if err := runRows.Scan(&runID); err == nil {
			runIDs = append(runIDs, runID)
		}
	}
	if err := runRows.Err(); err != nil {
		return 0
	}
	if len(runIDs) == 0 {
		return 0
	}
	inTok, outTok, _, _ := h.WorkflowRepo.RunLLMUsage(ctx, runIDs)
	return int(inTok + outTok)
}

// CountEnabledBySource 统计指定来源且已启用的工作流数量。
func (h *WorkflowAdmin) CountEnabledBySource(ctx context.Context, source string) (int, error) {
	var count int
	err := h.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM workflows WHERE enabled = 1 AND source = ?`, source).Scan(&count)
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "count workflows by source", err)
	}
	return count, nil
}

// DeleteWorkflow 删除指定工作流。
func (h *WorkflowAdmin) DeleteWorkflow(ctx context.Context, id string) error {
	if h.WorkflowRepo != nil {
		if err := h.WorkflowRepo.DeleteWorkflow(ctx, id); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "delete workflow", err)
		}
	}
	return nil
}

// TriggerWorkflow 手动触发指定工作流运行。
func (h *WorkflowAdmin) TriggerWorkflow(ctx context.Context, id string) (string, error) {
	var wf workflow
	var enabledInt int
	if err := h.DB.QueryRowContext(ctx, `
		SELECT id, type, name, description, trigger_type, cron_schedule, enabled
		FROM workflows WHERE id=?`, id).Scan(
		&wf.ID, &wf.Type, &wf.Name, &wf.Description, &wf.TriggerType, &wf.CronSchedule, &enabledInt,
	); err != nil {
		return "", apperr.Wrap(apperr.CodeNotFound, "workflow not found", err)
	}
	wf.Enabled = enabledInt == 1
	runID := h.executeWorkflow(ctx, &wf, "manual")
	return runID, nil
}
