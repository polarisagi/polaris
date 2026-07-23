package cronadmin

import (
	"github.com/polarisagi/polaris/internal/protocol/repo"

	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/httputil"
)

func (ca *CronAdmin) HandleListAutomations(w http.ResponseWriter, r *http.Request) {
	rows, err := ca.AutomationRepo.ListAutomations(r.Context())
	if err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
		return
	}

	var list []automation
	for _, row := range rows {
		a := automation{
			ID:              row.ID,
			Name:            row.Name,
			Prompt:          row.Prompt,
			TriggerType:     row.TriggerType,
			CronSchedule:    row.CronSchedule,
			ChannelID:       row.ChannelID,
			WorkingDir:      row.WorkingDir,
			EnvType:         row.EnvType,
			ReasoningEffort: row.ReasoningEffort,
			ResultAction:    row.ResultAction,
			SandboxLevel:    row.SandboxLevel,
			CedarRulesJSON:  row.CedarRulesJSON,
			Enabled:         row.Enabled,
			RequiresHITL:    row.RequiresHITL,
			RiskLevel:       row.RiskLevel,
			LastRunAt:       row.LastRunAt,
			NextRunAt:       row.NextRunAt,
			RunCount:        row.RunCount,
			LastRunStatus:   row.LastRunStatus,
			LastRunError:    row.LastRunError,
			CreatedAt:       row.CreatedAt,
			UpdatedAt:       row.UpdatedAt,
			EventFilter:     row.EventFilter,
		}
		list = append(list, a)
	}
	if list == nil {
		list = []automation{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"automations": list}) //nolint:errcheck
}

// ─── POST /v1/automations ─────────────────────────────────────────────────────

func (ca *CronAdmin) HandleCreateAutomation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name            string `json:"name"`
		Prompt          string `json:"prompt"`
		TriggerType     string `json:"trigger_type"`
		CronSchedule    string `json:"cron_schedule"`
		ChannelID       string `json:"channel_id"`
		WorkingDir      string `json:"working_dir"`
		EnvType         string `json:"env_type"`
		ReasoningEffort string `json:"reasoning_effort"`
		ResultAction    string `json:"result_action"`
		CedarRulesJSON  string `json:"cedar_rules_json"`
		Enabled         *bool  `json:"enabled"`
		RequiresHITL    bool   `json:"requires_hitl"`
		RiskLevel       int    `json:"risk_level"`
		EventFilter     string `json:"event_filter"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		http.Error(w, "prompt is required", http.StatusBadRequest)
		return
	}
	if req.TriggerType == "" {
		req.TriggerType = "cron"
	}
	if (req.TriggerType == "cron" || req.TriggerType == "both") && strings.TrimSpace(req.CronSchedule) == "" {
		http.Error(w, "cron_schedule is required for trigger_type=cron/both", http.StatusBadRequest)
		return
	}
	if req.ReasoningEffort == "" {
		req.ReasoningEffort = "medium"
	}
	if req.ResultAction == "" {
		req.ResultAction = "session"
	}
	if req.CedarRulesJSON == "" {
		req.CedarRulesJSON = "[]"
	}
	if req.EventFilter == "" {
		req.EventFilter = "{}"
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	now := time.Now().UTC().Format(time.RFC3339)
	id := newAutoID()
	nextRun := ""
	if req.CronSchedule != "" {
		nextRun = CalcNextRun(req.CronSchedule, now)
	}

	err := ca.AutomationRepo.CreateAutomation(r.Context(), repo.AutomationRow{
		ID:              id,
		Name:            req.Name,
		Prompt:          req.Prompt,
		TriggerType:     req.TriggerType,
		CronSchedule:    req.CronSchedule,
		ChannelID:       req.ChannelID,
		WorkingDir:      req.WorkingDir,
		EnvType:         req.EnvType,
		ReasoningEffort: req.ReasoningEffort,
		ResultAction:    req.ResultAction,
		SandboxLevel:    2,
		CedarRulesJSON:  req.CedarRulesJSON,
		Enabled:         enabled,
		RequiresHITL:    req.RequiresHITL,
		RiskLevel:       req.RiskLevel,
		NextRunAt:       nextRun,
		CreatedAt:       now,
		UpdatedAt:       now,
		EventFilter:     req.EventFilter,
	})
	if err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "created"}) //nolint:errcheck
}

// ─── PUT /v1/automations/{id} ─────────────────────────────────────────────────

//nolint:gocyclo
func (ca *CronAdmin) HandleUpdateAutomation(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")

	var req struct {
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
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusBadRequest)
		return
	}

	row, err := ca.AutomationRepo.GetAutomation(r.Context(), jobID)
	if err != nil || row == nil {
		http.Error(w, "automation not found", http.StatusNotFound)
		return
	}
	var j automation
	j.ID = row.ID
	j.Name = row.Name
	j.Prompt = row.Prompt
	j.TriggerType = row.TriggerType
	j.CronSchedule = row.CronSchedule
	j.ChannelID = row.ChannelID
	j.WorkingDir = row.WorkingDir
	j.EnvType = row.EnvType
	j.ReasoningEffort = row.ReasoningEffort
	j.ResultAction = row.ResultAction
	j.SandboxLevel = row.SandboxLevel
	j.CedarRulesJSON = row.CedarRulesJSON
	j.Enabled = row.Enabled
	j.RequiresHITL = row.RequiresHITL
	j.RiskLevel = row.RiskLevel
	j.EventFilter = row.EventFilter

	if req.Name != nil {
		j.Name = *req.Name
	}
	if req.Prompt != nil {
		j.Prompt = *req.Prompt
	}
	if req.TriggerType != nil {
		j.TriggerType = *req.TriggerType
	}
	if req.CronSchedule != nil {
		j.CronSchedule = *req.CronSchedule
	}
	if req.ChannelID != nil {
		j.ChannelID = *req.ChannelID
	}
	if req.WorkingDir != nil {
		j.WorkingDir = *req.WorkingDir
	}
	if req.EnvType != nil {
		j.EnvType = *req.EnvType
	}
	if req.ReasoningEffort != nil {
		j.ReasoningEffort = *req.ReasoningEffort
	}
	if req.ResultAction != nil {
		j.ResultAction = *req.ResultAction
	}
	if req.CedarRulesJSON != nil {
		j.CedarRulesJSON = *req.CedarRulesJSON
	}
	if req.Enabled != nil {
		j.Enabled = *req.Enabled
	}
	if req.RequiresHITL != nil {
		j.RequiresHITL = *req.RequiresHITL
	}
	if req.RiskLevel != nil {
		j.RiskLevel = *req.RiskLevel
	}
	if req.EventFilter != nil {
		j.EventFilter = *req.EventFilter
	}

	now := time.Now().UTC().Format(time.RFC3339)
	nextRun := ""
	if j.CronSchedule != "" {
		nextRun = CalcNextRun(j.CronSchedule, now)
	}

	err = ca.AutomationRepo.UpdateAutomation(r.Context(), repo.AutomationRow{
		ID:              jobID,
		Name:            j.Name,
		Prompt:          j.Prompt,
		TriggerType:     j.TriggerType,
		CronSchedule:    j.CronSchedule,
		ChannelID:       j.ChannelID,
		WorkingDir:      j.WorkingDir,
		EnvType:         j.EnvType,
		ReasoningEffort: j.ReasoningEffort,
		ResultAction:    j.ResultAction,
		SandboxLevel:    2,
		CedarRulesJSON:  j.CedarRulesJSON,
		Enabled:         j.Enabled,
		RequiresHITL:    j.RequiresHITL,
		RiskLevel:       j.RiskLevel,
		NextRunAt:       nextRun,
		UpdatedAt:       now,
		EventFilter:     j.EventFilter,
	})
	if err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "updated"}) //nolint:errcheck
}

// ─── DELETE /v1/automations/{id} ──────────────────────────────────────────────

func (ca *CronAdmin) HandleDeleteAutomation(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")
	ca.AutomationRepo.DeleteRunsByAutomationID(r.Context(), jobID) //nolint:errcheck
	if err := ca.AutomationRepo.DeleteAutomation(r.Context(), jobID); err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "deleted"}) //nolint:errcheck
}

// ─── GET /v1/automations/{id}/runs ────────────────────────────────────────────

func (ca *CronAdmin) HandleListAutomationRuns(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")
	limitStr := r.URL.Query().Get("limit")
	limit := 20
	if v, err := strconv.Atoi(limitStr); err == nil && v > 0 && v <= 100 {
		limit = v
	}

	rows, err := ca.AutomationRepo.ListRunsByAutomationID(r.Context(), jobID, limit)
	if err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
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

func (ca *CronAdmin) HandleTriggerAutomation(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")

	row, err := ca.AutomationRepo.GetAutomation(r.Context(), jobID)
	if err != nil || row == nil {
		http.Error(w, "automation not found", http.StatusNotFound)
		return
	}
	var a automation
	a.ID = row.ID
	a.Name = row.Name
	a.Prompt = row.Prompt
	a.WorkingDir = row.WorkingDir
	a.EnvType = row.EnvType
	a.ReasoningEffort = row.ReasoningEffort
	a.ResultAction = row.ResultAction
	a.SandboxLevel = row.SandboxLevel
	a.CedarRulesJSON = row.CedarRulesJSON
	a.Enabled = row.Enabled
	a.RequiresHITL = row.RequiresHITL
	a.RiskLevel = row.RiskLevel
	if !a.Enabled {
		http.Error(w, "automation is disabled", http.StatusConflict)
		return
	}

	runID := ca.executeAutomation(r.Context(), &a, "manual")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"run_id": runID, "status": "triggered"}) //nolint:errcheck
}
