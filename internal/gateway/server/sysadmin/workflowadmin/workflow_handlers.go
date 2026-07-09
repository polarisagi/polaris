package workflowadmin

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin/cronadmin"
	"github.com/polarisagi/polaris/internal/protocol/repo"
)

// ─── GET /v1/workflows ────────────────────────────────────────────────────────

func (h *WorkflowAdmin) HandleListWorkflows(w http.ResponseWriter, r *http.Request) {
	rows, err := h.DB.QueryContext(r.Context(), `
		SELECT w.id, w.name, w.description, w.trigger_type, w.cron_schedule, w.enabled,
		       COALESCE(sc.cnt, 0),
		       w.last_run_at, w.next_run_at, w.run_count,
		       w.last_run_status, w.last_run_error, w.created_at, w.updated_at
		FROM workflows w
		LEFT JOIN (SELECT workflow_id, COUNT(*) cnt FROM workflow_steps GROUP BY workflow_id) sc
		       ON sc.workflow_id = w.id
		ORDER BY w.created_at DESC`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var list []workflow
	for rows.Next() {
		var wf workflow
		var enabledInt int
		if err := rows.Scan(
			&wf.ID, &wf.Name, &wf.Description, &wf.TriggerType, &wf.CronSchedule, &enabledInt,
			&wf.StepsCount,
			&wf.LastRunAt, &wf.NextRunAt, &wf.RunCount,
			&wf.LastRunStatus, &wf.LastRunError, &wf.CreatedAt, &wf.UpdatedAt,
		); err != nil {
			continue
		}
		wf.Enabled = enabledInt == 1
		list = append(list, wf)
	}
	if list == nil {
		list = []workflow{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"workflows": list}) //nolint:errcheck
}

// ─── GET /v1/workflows/{id} ───────────────────────────────────────────────────

func (h *WorkflowAdmin) HandleGetWorkflow(w http.ResponseWriter, r *http.Request) {
	wfID := r.PathValue("id")

	var wf workflow
	var enabledInt int
	err := h.DB.QueryRowContext(r.Context(), `
		SELECT id, name, description, trigger_type, cron_schedule, enabled,
		       last_run_at, next_run_at, run_count, last_run_status, last_run_error,
		       created_at, updated_at
		FROM workflows WHERE id=?`, wfID).Scan(
		&wf.ID, &wf.Name, &wf.Description, &wf.TriggerType, &wf.CronSchedule, &enabledInt,
		&wf.LastRunAt, &wf.NextRunAt, &wf.RunCount, &wf.LastRunStatus, &wf.LastRunError,
		&wf.CreatedAt, &wf.UpdatedAt,
	)
	if err != nil {
		http.Error(w, "workflow not found", http.StatusNotFound)
		return
	}
	wf.Enabled = enabledInt == 1

	steps := h.loadWorkflowSteps(r.Context(), wfID)
	if steps == nil {
		steps = []workflowStep{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"workflow": wf, "steps": steps}) //nolint:errcheck
}

// ─── POST /v1/workflows ───────────────────────────────────────────────────────

func (h *WorkflowAdmin) HandleCreateWorkflow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name         string         `json:"name"`
		Description  string         `json:"description"`
		TriggerType  string         `json:"trigger_type"`
		CronSchedule string         `json:"cron_schedule"`
		Enabled      *bool          `json:"enabled"`
		Steps        []workflowStep `json:"steps"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if req.TriggerType == "" {
		req.TriggerType = "manual"
	}
	if req.TriggerType == "cron" && strings.TrimSpace(req.CronSchedule) == "" {
		http.Error(w, "cron_schedule required for trigger_type=cron", http.StatusBadRequest)
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	now := time.Now().UTC().Format(time.RFC3339)
	id := newWorkflowID()
	nextRun := ""
	if req.TriggerType == "cron" && req.CronSchedule != "" {
		nextRun = cronadmin.CalcNextRun(req.CronSchedule, now)
	}

	wfRow := repo.WorkflowRow{
		ID:           id,
		Name:         req.Name,
		Description:  req.Description,
		TriggerType:  req.TriggerType,
		CronSchedule: req.CronSchedule,
		Enabled:      enabled,
		NextRunAt:    nextRun,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	stepRows := make([]repo.WorkflowStepRow, 0, len(req.Steps))
	for i, st := range req.Steps {
		effort := st.ReasoningEffort
		if effort == "" {
			effort = "medium"
		}
		stepRows = append(stepRows, repo.WorkflowStepRow{
			ID:              newWorkflowStepID(),
			WorkflowID:      id,
			Seq:             i,
			Name:            st.Name,
			AutomationID:    st.AutomationID,
			Prompt:          st.Prompt,
			ReasoningEffort: effort,
			WorkingDir:      st.WorkingDir,
			InputFromPrev:   st.InputFromPrev,
		})
	}

	if err := h.WorkflowRepo.CreateWorkflowWithSteps(r.Context(), wfRow, stepRows); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "created"}) //nolint:errcheck
}

// ─── PUT /v1/workflows/{id} ───────────────────────────────────────────────────

//nolint:gocyclo
func (h *WorkflowAdmin) HandleUpdateWorkflow(w http.ResponseWriter, r *http.Request) {
	wfID := r.PathValue("id")

	var req struct {
		Name         *string        `json:"name"`
		Description  *string        `json:"description"`
		TriggerType  *string        `json:"trigger_type"`
		CronSchedule *string        `json:"cron_schedule"`
		Enabled      *bool          `json:"enabled"`
		Steps        []workflowStep `json:"steps"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var cur workflow
	var enabledInt int
	if err := h.DB.QueryRowContext(r.Context(), `
		SELECT id, name, description, trigger_type, cron_schedule, enabled
		FROM workflows WHERE id=?`, wfID).Scan(
		&cur.ID, &cur.Name, &cur.Description, &cur.TriggerType, &cur.CronSchedule, &enabledInt,
	); err != nil {
		http.Error(w, "workflow not found: "+err.Error(), http.StatusNotFound)
		return
	}
	cur.Enabled = enabledInt == 1

	if req.Name != nil {
		cur.Name = *req.Name
	}
	if req.Description != nil {
		cur.Description = *req.Description
	}
	if req.TriggerType != nil {
		cur.TriggerType = *req.TriggerType
	}
	if req.CronSchedule != nil {
		cur.CronSchedule = *req.CronSchedule
	}
	if req.Enabled != nil {
		cur.Enabled = *req.Enabled
	}

	now := time.Now().UTC().Format(time.RFC3339)
	nextRun := ""
	if cur.TriggerType == "cron" && cur.CronSchedule != "" {
		nextRun = cronadmin.CalcNextRun(cur.CronSchedule, now)
	}

	wfRow := repo.WorkflowRow{
		ID:           wfID,
		Name:         cur.Name,
		Description:  cur.Description,
		TriggerType:  cur.TriggerType,
		CronSchedule: cur.CronSchedule,
		Enabled:      cur.Enabled,
		NextRunAt:    nextRun,
		UpdatedAt:    now,
	}

	var stepRows []repo.WorkflowStepRow
	updateSteps := req.Steps != nil
	if updateSteps {
		for i, st := range req.Steps {
			effort := st.ReasoningEffort
			if effort == "" {
				effort = "medium"
			}
			stepRows = append(stepRows, repo.WorkflowStepRow{
				ID:              newWorkflowStepID(),
				WorkflowID:      wfID,
				Seq:             i,
				Name:            st.Name,
				AutomationID:    st.AutomationID,
				Prompt:          st.Prompt,
				ReasoningEffort: effort,
				WorkingDir:      st.WorkingDir,
				InputFromPrev:   st.InputFromPrev,
			})
		}
	}

	if err := h.WorkflowRepo.UpdateWorkflowWithSteps(r.Context(), wfRow, stepRows, updateSteps); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "updated"}) //nolint:errcheck
}

// ─── DELETE /v1/workflows/{id} ────────────────────────────────────────────────

func (h *WorkflowAdmin) HandleDeleteWorkflow(w http.ResponseWriter, r *http.Request) {
	wfID := r.PathValue("id")
	if err := h.WorkflowRepo.DeleteWorkflow(r.Context(), wfID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "deleted"}) //nolint:errcheck
}

// ─── POST /v1/workflows/{id}/trigger ─────────────────────────────────────────

func (h *WorkflowAdmin) HandleTriggerWorkflow(w http.ResponseWriter, r *http.Request) {
	wfID := r.PathValue("id")

	var wf workflow
	var enabledInt int
	if err := h.DB.QueryRowContext(r.Context(), `
		SELECT id, name, description, trigger_type, cron_schedule, enabled
		FROM workflows WHERE id=?`, wfID).Scan(
		&wf.ID, &wf.Name, &wf.Description, &wf.TriggerType, &wf.CronSchedule, &enabledInt,
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
		       started_at, finished_at, error_msg, step_outputs
		FROM workflow_runs WHERE workflow_id=?
		ORDER BY started_at DESC LIMIT 30`, wfID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
	if list == nil {
		list = []workflowRun{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"runs": list}) //nolint:errcheck
}
