package cronadmin

import (
	"github.com/polarisagi/polaris/internal/protocol/repo"

	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (ca *CronAdmin) HandleListAutomations(w http.ResponseWriter, r *http.Request) {
	rows, err := ca.DB.QueryContext(r.Context(), `
		SELECT id, name, prompt, trigger_type, cron_schedule, channel_id,
		       working_dir, env_type, reasoning_effort, result_action,
		       sandbox_level, cedar_rules_json, enabled, requires_hitl, risk_level,
		       last_run_at, next_run_at, run_count, last_run_status, last_run_error,
		       created_at, updated_at, event_filter
		FROM automations ORDER BY created_at DESC`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var list []automation
	for rows.Next() {
		var a automation
		var enabledInt int
		if err := rows.Scan(
			&a.ID, &a.Name, &a.Prompt, &a.TriggerType, &a.CronSchedule, &a.ChannelID,
			&a.WorkingDir, &a.EnvType, &a.ReasoningEffort, &a.ResultAction,
			&a.SandboxLevel, &a.CedarRulesJSON, &enabledInt, &a.RequiresHITL, &a.RiskLevel,
			&a.LastRunAt, &a.NextRunAt, &a.RunCount, &a.LastRunStatus, &a.LastRunError,
			&a.CreatedAt, &a.UpdatedAt, &a.EventFilter,
		); err != nil {
			continue
		}
		a.Enabled = enabledInt == 1
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
		http.Error(w, err.Error(), http.StatusBadRequest)
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
		RiskLevel:       fmt.Sprintf("%d", req.RiskLevel),
		NextRunAt:       nextRun,
		CreatedAt:       now,
		UpdatedAt:       now,
		EventFilter:     req.EventFilter,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var j automation
	var enabledInt int
	err := ca.DB.QueryRowContext(r.Context(), `
		SELECT id, name, prompt, trigger_type, cron_schedule, channel_id,
		       working_dir, env_type, reasoning_effort, result_action,
		       sandbox_level, cedar_rules_json, enabled, requires_hitl, risk_level, event_filter
		FROM automations WHERE id=?`, jobID).
		Scan(&j.ID, &j.Name, &j.Prompt, &j.TriggerType, &j.CronSchedule, &j.ChannelID,
			&j.WorkingDir, &j.EnvType, &j.ReasoningEffort, &j.ResultAction,
			&j.SandboxLevel, &j.CedarRulesJSON, &enabledInt, &j.RequiresHITL, &j.RiskLevel, &j.EventFilter)
	if err != nil {
		http.Error(w, "automation not found", http.StatusNotFound)
		return
	}
	j.Enabled = enabledInt == 1

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
		RiskLevel:       fmt.Sprintf("%d", j.RiskLevel),
		NextRunAt:       nextRun,
		UpdatedAt:       now,
		EventFilter:     j.EventFilter,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
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

	rows, err := ca.DB.QueryContext(r.Context(), `
		SELECT id, automation_id, trigger, status, session_id,
		       started_at, finished_at, error_msg, prompt_snapshot
		FROM automation_runs
		WHERE automation_id=?
		ORDER BY started_at DESC LIMIT ?`, jobID, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var list []automationRun
	for rows.Next() {
		var run automationRun
		if err := rows.Scan(
			&run.ID, &run.AutomationID, &run.Trigger, &run.Status, &run.SessionID,
			&run.StartedAt, &run.FinishedAt, &run.ErrorMsg, &run.PromptSnapshot,
		); err != nil {
			continue
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

	var a automation
	var enabledInt int
	if err := ca.DB.QueryRowContext(r.Context(), `
			SELECT id, name, prompt, working_dir, env_type, reasoning_effort,
			       result_action, sandbox_level, cedar_rules_json, enabled, requires_hitl, risk_level
			FROM automations WHERE id=?`, jobID).
		Scan(&a.ID, &a.Name, &a.Prompt, &a.WorkingDir, &a.EnvType, &a.ReasoningEffort,
			&a.ResultAction, &a.SandboxLevel, &a.CedarRulesJSON, &enabledInt, &a.RequiresHITL, &a.RiskLevel); err != nil {
		http.Error(w, "automation not found", http.StatusNotFound)
		return
	}
	a.Enabled = enabledInt == 1
	if !a.Enabled {
		http.Error(w, "automation is disabled", http.StatusConflict)
		return
	}

	runID := ca.executeAutomation(r.Context(), &a, "manual")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"run_id": runID, "status": "triggered"}) //nolint:errcheck
}
