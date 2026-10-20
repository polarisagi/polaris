package cronadmin

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// HandleListAutomations 列出全部自动化任务。
func (ca *CronAdmin) HandleListAutomations(w http.ResponseWriter, r *http.Request) {
	rows, err := ca.AutomationRepo.ListAutomations(r.Context())
	if err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}

	var list []automation
	for _, row := range rows {
		list = append(list, automationFromRow(row))
	}
	if list == nil {
		list = []automation{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"automations": list}) //nolint:errcheck
}

// ─── POST /v1/automations ─────────────────────────────────────────────────────

// HandleCreateAutomation 创建新的自动化任务。
func (ca *CronAdmin) HandleCreateAutomation(w http.ResponseWriter, r *http.Request) {
	var in AutomationInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httputil.RespondError(w, "", err, http.StatusBadRequest)
		return
	}
	id, _, err := ca.SaveAutomation(r.Context(), in)
	if err != nil {
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "created"}) //nolint:errcheck
}

// ─── PUT /v1/automations/{id} ─────────────────────────────────────────────────

type updateAutomationReq struct {
	Name            *string `json:"name"`
	Prompt          *string `json:"prompt"`
	TriggerType     *string `json:"trigger_type"`
	CronSchedule    *string `json:"cron_schedule"`
	ChannelID       *string `json:"channel_id"`
	WorkingDir      *string `json:"working_dir"`
	EnvType         *string `json:"env_type"`
	ReasoningEffort *string `json:"reasoning_effort"`
	ResultAction    *string `json:"result_action"`
	CedarRulesJSON  *string `json:"cedar_rules_json"`
	Enabled         *bool   `json:"enabled"`
	RequiresHITL    *bool   `json:"requires_hitl"`
	RiskLevel       *int    `json:"risk_level"`
	EventFilter     *string `json:"event_filter"`
	ProjectID       *string `json:"project_id"`
}

// setIfPresent 在 PATCH 字段非 nil 时覆盖目标值。
func setIfPresent[T any](dst *T, src *T) {
	if src != nil {
		*dst = *src
	}
}

func applyAutomationPatch(in *AutomationInput, req updateAutomationReq) {
	setIfPresent(&in.Name, req.Name)
	setIfPresent(&in.Prompt, req.Prompt)
	setIfPresent(&in.TriggerType, req.TriggerType)
	setIfPresent(&in.CronSchedule, req.CronSchedule)
	setIfPresent(&in.ChannelID, req.ChannelID)
	setIfPresent(&in.WorkingDir, req.WorkingDir)
	setIfPresent(&in.EnvType, req.EnvType)
	setIfPresent(&in.ReasoningEffort, req.ReasoningEffort)
	setIfPresent(&in.ResultAction, req.ResultAction)
	setIfPresent(&in.CedarRulesJSON, req.CedarRulesJSON)
	if req.Enabled != nil {
		in.Enabled = req.Enabled
	}
	setIfPresent(&in.RequiresHITL, req.RequiresHITL)
	setIfPresent(&in.RiskLevel, req.RiskLevel)
	setIfPresent(&in.EventFilter, req.EventFilter)
	setIfPresent(&in.ProjectID, req.ProjectID)
}

// HandleUpdateAutomation 更新指定自动化任务。
func (ca *CronAdmin) HandleUpdateAutomation(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")

	var req updateAutomationReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "", err, http.StatusBadRequest)
		return
	}

	row, err := ca.AutomationRepo.GetAutomation(r.Context(), jobID)
	if err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}
	if row == nil {
		httputil.RespondError(w, "automation not found", apperr.New(apperr.CodeNotFound, "automation not found"), http.StatusNotFound)
		return
	}

	in := AutomationInput{
		ID:              jobID,
		Name:            row.Name,
		Prompt:          row.Prompt,
		TriggerType:     row.TriggerType,
		CronSchedule:    row.CronSchedule,
		ChannelID:       row.ChannelID,
		WorkingDir:      row.WorkingDir,
		EnvType:         row.EnvType,
		ReasoningEffort: row.ReasoningEffort,
		ResultAction:    row.ResultAction,
		CedarRulesJSON:  row.CedarRulesJSON,
		EventFilter:     row.EventFilter,
		Enabled:         &row.Enabled,
		RequiresHITL:    row.RequiresHITL,
		RiskLevel:       row.RiskLevel,
		Source:          row.Source,
		ProjectID:       row.ProjectID,
	}
	applyAutomationPatch(&in, req)

	_, _, err = ca.SaveAutomation(r.Context(), in)
	if err != nil {
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "updated"}) //nolint:errcheck
}

// ─── DELETE /v1/automations/{id} ──────────────────────────────────────────────

// HandleDeleteAutomation 删除指定自动化任务。
func (ca *CronAdmin) HandleDeleteAutomation(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")
	if err := ca.DeleteAutomation(r.Context(), jobID); err != nil {
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "deleted"}) //nolint:errcheck
}

// ─── GET /v1/automations/{id}/runs ────────────────────────────────────────────

// HandleListAutomationRuns 查询指定自动化任务的执行历史。
func (ca *CronAdmin) HandleListAutomationRuns(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")
	limitStr := r.URL.Query().Get("limit")
	limit := 20
	if v, err := strconv.Atoi(limitStr); err == nil && v > 0 && v <= 100 {
		limit = v
	}

	rows, err := ca.AutomationRepo.ListRunsByAutomationID(r.Context(), jobID, limit)
	if err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}

	var list []automationRun
	for _, row := range rows {
		run := automationRun{
			ID:             row.ID,
			AutomationID:   row.AutomationID,
			Trigger:        row.Trigger,
			Status:         row.Status,
			SessionID:      row.SessionID,
			StartedAt:      row.StartedAt,
			FinishedAt:     row.FinishedAt,
			ErrorMsg:       row.ErrorMsg,
			PromptSnapshot: row.PromptSnapshot,
		}
		list = append(list, run)
	}
	if list == nil {
		list = []automationRun{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"runs": list}) //nolint:errcheck
}

// ─── POST /v1/automations/{id}/trigger ────────────────────────────────────────

// HandleTriggerAutomation 手动触发自动化执行。
func (ca *CronAdmin) HandleTriggerAutomation(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")
	runID, err := ca.TriggerAutomation(r.Context(), jobID)
	if err != nil {
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"run_id": runID, "status": "triggered"}) //nolint:errcheck
}
