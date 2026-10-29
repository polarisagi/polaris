package workflowadmin

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/polarisagi/polaris/internal/automation/pipeline"
	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// ─── POST /v1/workflows/{id}/trigger ─────────────────────────────────────────

func (h *WorkflowAdmin) HandleTriggerWorkflow(w http.ResponseWriter, r *http.Request) {
	wfID := r.PathValue("id")

	var wf workflow
	var enabledInt int
	if err := h.DB.QueryRowContext(r.Context(), `
		SELECT id, type, name, description, trigger_type, cron_schedule, enabled
		FROM workflows WHERE id=?`, wfID).Scan(
		&wf.ID, &wf.Type, &wf.Name, &wf.Description, &wf.TriggerType, &wf.CronSchedule, &enabledInt,
	); err != nil {
		http.Error(w, "workflow not found", http.StatusNotFound)
		return
	}
	wf.Enabled = enabledInt == 1

	runID := h.executeWorkflow(r.Context(), &wf, "manual")

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"run_id": runID, "status": "triggered"}) //nolint:errcheck
}

// ─── GET /v1/workflows/{id}/runs ─────────────────────────────────────────────

func (h *WorkflowAdmin) HandleListWorkflowRuns(w http.ResponseWriter, r *http.Request) {
	wfID := r.PathValue("id")

	rows, err := h.DB.QueryContext(r.Context(), `
		SELECT id, workflow_id, trigger, status, current_step, total_steps,
		       started_at, finished_at, error_msg, step_outputs, COALESCE(skipped, 0)
		FROM workflow_runs WHERE workflow_id=?
		ORDER BY started_at DESC LIMIT 30`, wfID)
	if err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var list []workflowRun
	for rows.Next() {
		var run workflowRun
		var rawOutputs []byte
		if err := rows.Scan(
			&run.ID, &run.WorkflowID, &run.Trigger, &run.Status,
			&run.CurrentStep, &run.TotalSteps,
			&run.StartedAt, &run.FinishedAt, &run.ErrorMsg, &rawOutputs,
			&run.Skipped,
		); err != nil {
			continue
		}
		if len(rawOutputs) > 0 {
			run.StepOutputs = rawOutputs
		} else {
			run.StepOutputs = json.RawMessage("[]")
		}
		list = append(list, run)
	}
	if err := rows.Err(); err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}
	rows.Close()

	if h.WorkflowRepo != nil {
		for i := range list {
			sessionIDs := []string{list[i].ID}
			var outputs []stepOutput
			if err := json.Unmarshal(list[i].StepOutputs, &outputs); err == nil {
				for _, so := range outputs {
					if so.SessionID != "" {
						sessionIDs = append(sessionIDs, so.SessionID)
					}
				}
			}
			if inTok, outTok, cost, qErr := h.WorkflowRepo.RunLLMUsage(r.Context(), sessionIDs); qErr == nil {
				list[i].LLMTokensIn = inTok
				list[i].LLMTokensOut = outTok
				list[i].CostUSD = cost
			}
		}
	}
	if list == nil {
		list = []workflowRun{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"runs": list}) //nolint:errcheck
}

// ─── GET /v1/workflows/runs/{id}/data/{step_key} ─────────────────────────────

// HandleGetRunStepData 返回单次运行指定步骤的数据产物。
// 响应形如：{status: string, count: int, truncated: bool, items: []map[string]any, text: string}
func (h *WorkflowAdmin) HandleGetRunStepData(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	stepKey := r.PathValue("step_key")

	if h.WorkflowRepo == nil {
		httputil.RespondError(w, "", apperr.New(apperr.CodeInternal, "workflow repository is nil"), http.StatusInternalServerError)
		return
	}

	status, dataJSON, err := h.WorkflowRepo.GetRunData(r.Context(), runID, stepKey)
	if err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}
	if status == "" && len(dataJSON) == 0 {
		http.Error(w, "step data not found", http.StatusNotFound)
		return
	}

	var data pipeline.StepData
	if len(dataJSON) > 0 {
		if err := json.Unmarshal(dataJSON, &data); err != nil {
			data = pipeline.StepData{Text: string(dataJSON), Count: 1}
		}
	}

	items := data.Items
	truncated := false
	if len(items) > 50 {
		items = items[:50]
		truncated = true
	}
	if items == nil {
		items = []map[string]any{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
		"status":    status,
		"count":     data.Count,
		"truncated": truncated,
		"items":     items,
		"text":      data.Text,
	})
}

// ─── POST /v1/workflows/runs/{id}/resume ─────────────────────────────────────

// HandleResumeWorkflowRun 续跑 interrupted|error 的运行（ADR-0104 决策二）：以同一 runID 重入
// StateGraphExecutor，done 节点由 task_checkpoints 复用。只能显式触发、启动期不自动续跑：
// 步骤是完整 Agent 运行，executing 节点重跑可能重复不可逆副作用，须由人确认。
func (h *WorkflowAdmin) HandleResumeWorkflowRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	ctx := r.Context()

	var wfID, status string
	if err := h.DB.QueryRowContext(ctx, `SELECT workflow_id, status FROM workflow_runs WHERE id=?`, runID).Scan(&wfID, &status); err != nil {
		http.Error(w, "workflow run not found", http.StatusNotFound)
		return
	}
	if status != "interrupted" && status != "error" {
		http.Error(w, "run is not resumable (status="+status+")", http.StatusConflict)
		return
	}

	var wf workflow
	var enabledInt int
	if err := h.DB.QueryRowContext(ctx, `
		SELECT id, type, name, description, trigger_type, cron_schedule, enabled
		FROM workflows WHERE id=?`, wfID).Scan(
		&wf.ID, &wf.Type, &wf.Name, &wf.Description, &wf.TriggerType, &wf.CronSchedule, &enabledInt,
	); err != nil {
		http.Error(w, "workflow deleted", http.StatusConflict)
		return
	}
	wf.Enabled = enabledInt == 1
	if !wf.Enabled {
		http.Error(w, "workflow disabled", http.StatusConflict)
		return
	}

	// 条件 UPDATE 是并发闸门：两个并发续跑请求只有一个能把状态从 interrupted|error 翻到 running。
	resumed, err := h.WorkflowRepo.ResumeWorkflowRun(ctx, runID, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}
	if !resumed {
		http.Error(w, "run is not resumable", http.StatusConflict)
		return
	}

	// 进程重启续跑（ResumeWorkflowRun）丢失 pending 可接受（只会导致少记 seen → 下次可能重复一次，无安全性副作用）。
	date := time.Now().Format("2006-01-02")
	since, _ := h.WorkflowRepo.LastSuccessfulRunStart(ctx, wf.ID)
	h.runWorkflowGraph(&wf, runID, h.loadWorkflowSteps(ctx, wf.ID), nil, date, since)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"run_id": runID, "status": "resumed"}) //nolint:errcheck
}
