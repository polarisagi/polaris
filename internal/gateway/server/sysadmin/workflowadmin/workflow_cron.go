package workflowadmin

import (
	"context"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin/cronadmin"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// ─── Cron 触发（由 cronadmin.cronTick 通过 CronTickWorkflows 回调调用）───────

// CronTickWorkflows 扫描到期的 cron 工作流并触发。导出供
// cronadmin.NewCronAdmin 的 cronTickWorkflows 回调参数接入
// （2026-07-07：此前该回调在 SysAdminHandler 里硬编码 nil，见 P0 修复记录）。
func (h *WorkflowAdmin) CronTickWorkflows(ctx context.Context) {
	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := h.DB.QueryContext(ctx, `
		SELECT id, name, description, trigger_type, cron_schedule, enabled
		FROM workflows
		WHERE enabled=1
		  AND circuit_open=0
		  AND trigger_type='cron'
		  AND cron_schedule != ''
		  AND (next_run_at = '' OR next_run_at <= ?)
		  AND last_run_status != 'running'`,
		now)
	if err != nil {
		slog.Warn("CronTickWorkflows: query failed", "err", err)
		return
	}
	defer rows.Close()

	var due []workflow
	for rows.Next() {
		var wf workflow
		var enabledInt int
		if err := rows.Scan(
			&wf.ID, &wf.Name, &wf.Description, &wf.TriggerType, &wf.CronSchedule, &enabledInt,
		); err != nil {
			continue
		}
		wf.Enabled = enabledInt == 1
		due = append(due, wf)
	}
	rows.Close()

	for i := range due {
		wf := &due[i]
		concurrent.SafeGo(ctx, "gateway.sysadmin.workflow_cron_tick", func(ctx context.Context) {
			h.executeWorkflow(ctx, wf, "cron")
		})
	}
}

// ─── 辅助函数 ─────────────────────────────────────────────────────────────────

// applyAutomationOverride 从已有 automation 加载配置覆盖步骤字段（非空值才覆盖）。
func (h *WorkflowAdmin) applyAutomationOverride(ctx context.Context, automationID string, prompt, workingDir, effort *string) {
	var aPrompt, aDir, aEffort string
	err := h.DB.QueryRowContext(ctx, `
		SELECT prompt, working_dir, reasoning_effort FROM automations WHERE id=?`,
		automationID).Scan(&aPrompt, &aDir, &aEffort)
	if err != nil {
		return
	}
	*prompt = aPrompt
	if aDir != "" {
		*workingDir = aDir
	}
	if aEffort != "" {
		*effort = aEffort
	}
}

func (h *WorkflowAdmin) loadWorkflowSteps(ctx context.Context, wfID string) []workflowStep {
	rows, err := h.DB.QueryContext(ctx, `
		SELECT id, workflow_id, seq, name, automation_id, prompt,
		       reasoning_effort, working_dir, input_from_prev
		FROM workflow_steps WHERE workflow_id=? ORDER BY seq ASC`, wfID)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var steps []workflowStep
	for rows.Next() {
		var st workflowStep
		var inputInt int
		if err := rows.Scan(
			&st.ID, &st.WorkflowID, &st.Seq, &st.Name, &st.AutomationID, &st.Prompt,
			&st.ReasoningEffort, &st.WorkingDir, &inputInt,
		); err != nil {
			continue
		}
		st.InputFromPrev = inputInt == 1
		steps = append(steps, st)
	}
	return steps
}

// updateWorkflowStats 更新 workflows 统计字段 + 电路断路器。
func (h *WorkflowAdmin) updateWorkflowStats(workflowID, status, errMsg, finishedAt string) {
	bg := context.Background()
	if err := h.WorkflowRepo.UpdateWorkflowStats(bg, workflowID, status, errMsg, finishedAt, cronadmin.CircuitBreakThreshold); err != nil {
		slog.Warn("workflow: update stats failed", "id", workflowID, "err", err)
	}
}
