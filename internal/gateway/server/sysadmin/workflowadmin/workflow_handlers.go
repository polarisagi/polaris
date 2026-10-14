package workflowadmin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/automation/pipeline"
	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin/cronadmin"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// ─── GET /v1/workflows ────────────────────────────────────────────────────────

func (h *WorkflowAdmin) populateWorkflowLastRunMetrics(ctx context.Context, wf *workflow, runID, stepOutputs string, skippedInt int) {
	wf.LastRunSkipped = skippedInt == 1
	if runID == "" || h.WorkflowRepo == nil {
		return
	}
	sessionIDs := []string{runID}
	var outputs []stepOutput
	if err := json.Unmarshal([]byte(stepOutputs), &outputs); err == nil {
		for _, so := range outputs {
			if so.SessionID != "" {
				sessionIDs = append(sessionIDs, so.SessionID)
			}
		}
	}
	if inTok, outTok, cost, err := h.WorkflowRepo.RunLLMUsage(ctx, sessionIDs); err == nil {
		wf.LastRunTokens = int(inTok + outTok)
		wf.LastRunCost = cost
	}
}

func (h *WorkflowAdmin) HandleListWorkflows(w http.ResponseWriter, r *http.Request) {
	rows, err := h.DB.QueryContext(r.Context(), `
		SELECT w.id, w.type, w.name, w.description, w.trigger_type, w.cron_schedule, w.enabled,
		       COALESCE(sc.cnt, 0),
		       w.last_run_at, w.next_run_at, w.run_count,
		       w.last_run_status, w.last_run_error, w.created_at, w.updated_at,
		       COALESCE(w.result_action, 'silent'), COALESCE(w.deliver_from, ''), COALESCE(w.source, 'user'),
		       COALESCE(lr.skipped, 0), COALESCE(lr.id, ''), COALESCE(lr.step_outputs, '[]')
		FROM workflows w
		LEFT JOIN (SELECT workflow_id, COUNT(*) cnt FROM workflow_steps GROUP BY workflow_id) sc
		       ON sc.workflow_id = w.id
		LEFT JOIN (
		    SELECT id, workflow_id, skipped, step_outputs
		    FROM (
		        SELECT id, workflow_id, skipped, step_outputs,
		               ROW_NUMBER() OVER (PARTITION BY workflow_id ORDER BY started_at DESC) as rn
		        FROM workflow_runs
		    ) WHERE rn = 1
		) lr ON lr.workflow_id = w.id
		ORDER BY w.created_at DESC`)
	if err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var list []workflow
	for rows.Next() {
		var wf workflow
		var enabledInt, skippedInt int
		var runID, stepOutputs string
		if err := rows.Scan(
			&wf.ID, &wf.Type, &wf.Name, &wf.Description, &wf.TriggerType, &wf.CronSchedule, &enabledInt,
			&wf.StepsCount,
			&wf.LastRunAt, &wf.NextRunAt, &wf.RunCount,
			&wf.LastRunStatus, &wf.LastRunError, &wf.CreatedAt, &wf.UpdatedAt,
			&wf.ResultAction, &wf.DeliverFrom, &wf.Source,
			&skippedInt, &runID, &stepOutputs,
		); err != nil {
			continue
		}
		wf.Enabled = enabledInt == 1
		h.populateWorkflowLastRunMetrics(r.Context(), &wf, runID, stepOutputs, skippedInt)
		list = append(list, wf)
	}
	if err := rows.Err(); err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
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
		SELECT id, type, name, description, trigger_type, cron_schedule, enabled,
		       last_run_at, next_run_at, run_count, last_run_status, last_run_error,
		       created_at, updated_at,
		       COALESCE(result_action, 'silent'), COALESCE(deliver_from, ''), COALESCE(source, 'user')
		FROM workflows WHERE id=?`, wfID).Scan(
		&wf.ID, &wf.Type, &wf.Name, &wf.Description, &wf.TriggerType, &wf.CronSchedule, &enabledInt,
		&wf.LastRunAt, &wf.NextRunAt, &wf.RunCount, &wf.LastRunStatus, &wf.LastRunError,
		&wf.CreatedAt, &wf.UpdatedAt,
		&wf.ResultAction, &wf.DeliverFrom, &wf.Source,
	)
	if err != nil {
		http.Error(w, "workflow not found", http.StatusNotFound)
		return
	}
	wf.Enabled = enabledInt == 1

	var skippedInt int
	var runID, stepOutputs string
	_ = h.DB.QueryRowContext(r.Context(), `
		SELECT COALESCE(skipped, 0), id, COALESCE(step_outputs, '[]')
		FROM workflow_runs WHERE workflow_id=? ORDER BY started_at DESC LIMIT 1`, wfID).Scan(&skippedInt, &runID, &stepOutputs)
	h.populateWorkflowLastRunMetrics(r.Context(), &wf, runID, stepOutputs, skippedInt)

	steps := h.loadWorkflowSteps(r.Context(), wfID)
	if steps == nil {
		steps = []workflowStep{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"workflow": wf, "steps": steps}) //nolint:errcheck
}

// ─── POST /v1/workflows ───────────────────────────────────────────────────────

// validateCreateWorkflowRequest 校验/归一化建表请求参数（type/trigger_type 默认值，
// name 必填，trigger_type=cron 时 cron_schedule 必填），失败时直接写入 400 响应。
// 返回 ok=false 时调用方应立即 return（从 HandleCreateWorkflow 拆出，gocyclo 治理，行为不变）。
func validateCreateWorkflowRequest(w http.ResponseWriter, reqType *string, name string, triggerType *string, cronSchedule string) bool {
	if *reqType == "" {
		*reqType = "chain"
	}
	if *reqType != "chain" && *reqType != "dag" {
		http.Error(w, "type must be 'chain' or 'dag'", http.StatusBadRequest)
		return false
	}
	if strings.TrimSpace(name) == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return false
	}
	if *triggerType == "" {
		*triggerType = "manual"
	}
	if *triggerType == "cron" && strings.TrimSpace(cronSchedule) == "" {
		http.Error(w, "cron_schedule required for trigger_type=cron", http.StatusBadRequest)
		return false
	}
	return true
}

func (h *WorkflowAdmin) handlePipelineCreate(w http.ResponseWriter, r *http.Request, bodyBytes []byte) bool {
	var probe struct {
		Kind string `json:"kind"`
	}
	_ = json.Unmarshal(bodyBytes, &probe)
	if probe.Kind != "pipeline" {
		return false
	}
	var spec pipeline.Spec
	if err := json.Unmarshal(bodyBytes, &spec); err != nil {
		httputil.RespondError(w, "", err, http.StatusBadRequest)
		return true
	}
	wfID, _, fieldErrs, err := h.SavePipeline(r.Context(), spec, "", "user")
	if len(fieldErrs) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"errors": fieldErrs}) //nolint:errcheck
		return true
	}
	if err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"id": wfID, "status": "created"}) //nolint:errcheck
	return true
}

func (h *WorkflowAdmin) handlePipelineUpdate(w http.ResponseWriter, r *http.Request, wfID string, bodyBytes []byte) bool {
	var probe struct {
		Kind string `json:"kind"`
	}
	_ = json.Unmarshal(bodyBytes, &probe)
	if probe.Kind != "pipeline" {
		return false
	}
	var spec pipeline.Spec
	if err := json.Unmarshal(bodyBytes, &spec); err != nil {
		httputil.RespondError(w, "", err, http.StatusBadRequest)
		return true
	}
	_, _, fieldErrs, err := h.SavePipeline(r.Context(), spec, wfID, "user")
	if len(fieldErrs) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"errors": fieldErrs}) //nolint:errcheck
		return true
	}
	if err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "updated"}) //nolint:errcheck
	return true
}

func hasNonAgentStep(steps []workflowStep) bool {
	for _, st := range steps {
		if st.Kind != "" && st.Kind != "agent" {
			return true
		}
	}
	return false
}

func resolveStepNeeds(stepSpec *pipeline.StepSpec, dependsOn []string, steps []workflowStep) {
	if len(stepSpec.Needs) > 0 || len(dependsOn) == 0 {
		return
	}
	for _, d := range dependsOn {
		if idx, err := strconv.Atoi(d); err == nil && idx >= 0 && idx < len(steps) {
			upstreamKey := steps[idx].StepKey
			if upstreamKey == "" {
				upstreamKey = fmt.Sprintf("step_%d", idx)
			}
			stepSpec.Needs = append(stepSpec.Needs, upstreamKey)
		}
	}
}

func convertWorkflowStepToPipelineStep(st workflowStep, i int, steps []workflowStep) pipeline.StepSpec {
	var stepSpec pipeline.StepSpec
	if st.ConfigJSON != "" && st.ConfigJSON != "{}" {
		_ = json.Unmarshal([]byte(st.ConfigJSON), &stepSpec)
	}
	if st.StepKey != "" {
		stepSpec.Key = st.StepKey
	}
	if stepSpec.Key == "" {
		stepSpec.Key = fmt.Sprintf("step_%d", i)
	}
	if st.Kind != "" {
		stepSpec.Kind = st.Kind
	}
	if st.Prompt != "" && stepSpec.Prompt == "" {
		stepSpec.Prompt = st.Prompt
	}
	if st.ReasoningEffort != "" && stepSpec.ReasoningEffort == "" {
		stepSpec.ReasoningEffort = st.ReasoningEffort
	}
	if st.MaxRetries > 0 && stepSpec.MaxRetries == 0 {
		stepSpec.MaxRetries = st.MaxRetries
	}
	resolveStepNeeds(&stepSpec, st.DependsOn, steps)
	return stepSpec
}

func (h *WorkflowAdmin) validatePipelineSteps(name, desc, triggerType, cronSchedule, resultAction, deliverFrom string, steps []workflowStep) []pipeline.FieldError {
	if !hasNonAgentStep(steps) {
		return nil
	}

	spec := pipeline.Spec{
		Kind:         "pipeline",
		Name:         name,
		Description:  desc,
		Trigger:      pipeline.Trigger{Type: triggerType, Cron: cronSchedule},
		ResultAction: resultAction,
		DeliverFrom:  deliverFrom,
		Steps:        make([]pipeline.StepSpec, 0, len(steps)),
	}

	for i, st := range steps {
		spec.Steps = append(spec.Steps, convertWorkflowStepToPipelineStep(st, i, steps))
	}

	return h.validateSpec(spec)
}

func (h *WorkflowAdmin) validateSpec(spec pipeline.Spec) []pipeline.FieldError {
	valDeps := toolMetaAdapter{exec: h.ToolExecutor}
	limits := pipeline.Limits{
		MaxSteps:           20,
		MinIntervalMin:     15,
		LLMMaxInputChars:   24000,
		LLMMaxOutputTokens: 2000,
		StepDataMaxBytes:   1048576,
	}
	minIntervalFn := func(cronExpr string) (int, error) {
		return cronadmin.MinIntervalMinutes(cronExpr, time.Now().UTC())
	}
	return pipeline.Validate(spec, valDeps, limits, minIntervalFn)
}

func buildWorkflowStepRows(wfID string, steps []workflowStep) []repo.WorkflowStepRow {
	stepRows := make([]repo.WorkflowStepRow, 0, len(steps))
	for i, st := range steps {
		effort := st.ReasoningEffort
		if effort == "" {
			effort = "medium"
		}
		kind := st.Kind
		if kind == "" {
			kind = "agent"
		}
		stepKey := st.StepKey
		if stepKey == "" {
			stepKey = fmt.Sprintf("step_%d", i)
		}
		configJSON := st.ConfigJSON
		if configJSON == "" {
			configJSON = "{}"
		}
		stepRows = append(stepRows, repo.WorkflowStepRow{
			ID:               newWorkflowStepID(),
			WorkflowID:       wfID,
			Seq:              i,
			Name:             st.Name,
			AutomationID:     st.AutomationID,
			Prompt:           st.Prompt,
			ReasoningEffort:  effort,
			WorkingDir:       st.WorkingDir,
			InputFromPrev:    st.InputFromPrev,
			DependsOn:        st.DependsOn,
			CapabilityType:   st.CapabilityType,
			CompensationTool: st.CompensationTool,
			CompensationArgs: st.CompensationArgs,
			MaxRetries:       st.MaxRetries,
			StepKey:          stepKey,
			Kind:             kind,
			ConfigJSON:       configJSON,
		})
	}
	return stepRows
}

func (h *WorkflowAdmin) HandleCreateWorkflow(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		httputil.RespondError(w, "", err, http.StatusBadRequest)
		return
	}
	if h.handlePipelineCreate(w, r, bodyBytes) {
		return
	}

	var req struct {
		Type         string         `json:"type"`
		Name         string         `json:"name"`
		Description  string         `json:"description"`
		TriggerType  string         `json:"trigger_type"`
		CronSchedule string         `json:"cron_schedule"`
		Enabled      *bool          `json:"enabled"`
		ResultAction string         `json:"result_action"`
		DeliverFrom  string         `json:"deliver_from"`
		Steps        []workflowStep `json:"steps"`
	}
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		httputil.RespondError(w, "", err, http.StatusBadRequest)
		return
	}
	if !validateCreateWorkflowRequest(w, &req.Type, req.Name, &req.TriggerType, req.CronSchedule) {
		return
	}

	if fieldErrs := h.validatePipelineSteps(req.Name, req.Description, req.TriggerType, req.CronSchedule, req.ResultAction, req.DeliverFrom, req.Steps); len(fieldErrs) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"errors": fieldErrs}) //nolint:errcheck
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

	resultAction := req.ResultAction
	if resultAction == "" {
		resultAction = "silent"
	}

	wfRow := repo.WorkflowRow{
		ID:           id,
		Type:         req.Type,
		Name:         req.Name,
		Description:  req.Description,
		TriggerType:  req.TriggerType,
		CronSchedule: req.CronSchedule,
		Enabled:      enabled,
		NextRunAt:    nextRun,
		CreatedAt:    now,
		UpdatedAt:    now,
		ResultAction: resultAction,
		DeliverFrom:  req.DeliverFrom,
		Source:       "user",
	}

	if err := validateStepRetryCompensation(req.Steps); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	stepRows := buildWorkflowStepRows(id, req.Steps)

	if err := h.WorkflowRepo.CreateWorkflowWithSteps(r.Context(), wfRow, stepRows); err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
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

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		httputil.RespondError(w, "", err, http.StatusBadRequest)
		return
	}
	if h.handlePipelineUpdate(w, r, wfID, bodyBytes) {
		return
	}

	var req struct {
		Type         *string        `json:"type"`
		Name         *string        `json:"name"`
		Description  *string        `json:"description"`
		TriggerType  *string        `json:"trigger_type"`
		CronSchedule *string        `json:"cron_schedule"`
		Enabled      *bool          `json:"enabled"`
		ResultAction *string        `json:"result_action"`
		DeliverFrom  *string        `json:"deliver_from"`
		Steps        []workflowStep `json:"steps"`
	}
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		httputil.RespondError(w, "", err, http.StatusBadRequest)
		return
	}

	var cur workflow
	var enabledInt int
	if err := h.DB.QueryRowContext(r.Context(), `
		SELECT id, type, name, description, trigger_type, cron_schedule, enabled,
		       COALESCE(result_action, 'silent'), COALESCE(deliver_from, ''), COALESCE(source, 'user')
		FROM workflows WHERE id=?`, wfID).Scan(
		&cur.ID, &cur.Type, &cur.Name, &cur.Description, &cur.TriggerType, &cur.CronSchedule, &enabledInt,
		&cur.ResultAction, &cur.DeliverFrom, &cur.Source,
	); err != nil {
		http.Error(w, "workflow not found: "+err.Error(), http.StatusNotFound)
		return
	}
	cur.Enabled = enabledInt == 1

	if req.Type != nil {
		if *req.Type != "chain" && *req.Type != "dag" {
			http.Error(w, "type must be 'chain' or 'dag'", http.StatusBadRequest)
			return
		}
		cur.Type = *req.Type
	}
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
	if req.ResultAction != nil {
		cur.ResultAction = *req.ResultAction
	}
	if req.DeliverFrom != nil {
		cur.DeliverFrom = *req.DeliverFrom
	}

	now := time.Now().UTC().Format(time.RFC3339)
	nextRun := ""
	if cur.TriggerType == "cron" && cur.CronSchedule != "" {
		nextRun = cronadmin.CalcNextRun(cur.CronSchedule, now)
	}

	wfRow := repo.WorkflowRow{
		ID:           wfID,
		Type:         cur.Type,
		Name:         cur.Name,
		Description:  cur.Description,
		TriggerType:  cur.TriggerType,
		CronSchedule: cur.CronSchedule,
		Enabled:      cur.Enabled,
		NextRunAt:    nextRun,
		UpdatedAt:    now,
		ResultAction: cur.ResultAction,
		DeliverFrom:  cur.DeliverFrom,
		Source:       cur.Source,
	}

	if req.Steps != nil {
		if err := validateStepRetryCompensation(req.Steps); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if fieldErrs := h.validatePipelineSteps(cur.Name, cur.Description, cur.TriggerType, cur.CronSchedule, cur.ResultAction, cur.DeliverFrom, req.Steps); len(fieldErrs) > 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{"errors": fieldErrs}) //nolint:errcheck
			return
		}
	}

	var stepRows []repo.WorkflowStepRow
	updateSteps := req.Steps != nil
	if updateSteps {
		stepRows = buildWorkflowStepRows(wfID, req.Steps)
	}

	if err := h.WorkflowRepo.UpdateWorkflowWithSteps(r.Context(), wfRow, stepRows, updateSteps); err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "updated"}) //nolint:errcheck
}

// ─── DELETE /v1/workflows/{id} ────────────────────────────────────────────────

func (h *WorkflowAdmin) HandleDeleteWorkflow(w http.ResponseWriter, r *http.Request) {
	wfID := r.PathValue("id")
	if err := h.WorkflowRepo.DeleteWorkflow(r.Context(), wfID); err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
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

	status, dataJSON, err := h.WorkflowRepo.LoadRunData(r.Context(), runID, stepKey)
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

func convertPipelineStepsToRepoRows(steps []pipeline.StepSpec) []repo.WorkflowStepRow {
	keyToSeq := make(map[string]int, len(steps))
	for i, st := range steps {
		keyToSeq[st.Key] = i
	}

	stepRows := make([]repo.WorkflowStepRow, 0, len(steps))
	for i, st := range steps {
		stepRows = append(stepRows, buildWorkflowStepRow(st, i, keyToSeq))
	}
	return stepRows
}

func buildWorkflowStepRow(st pipeline.StepSpec, i int, keyToSeq map[string]int) repo.WorkflowStepRow {
	dependsOn := make([]string, 0, len(st.Needs))
	for _, need := range st.Needs {
		if seq, ok := keyToSeq[need]; ok {
			dependsOn = append(dependsOn, strconv.Itoa(seq))
		}
	}

	configBytes, _ := json.Marshal(st)
	effort := st.ReasoningEffort
	if effort == "" {
		effort = "medium"
	}
	stepName := st.Key
	if stepName == "" {
		stepName = fmt.Sprintf("step_%d", i)
	}

	return repo.WorkflowStepRow{
		ID:              newWorkflowStepID(),
		Seq:             i,
		Name:            stepName,
		Prompt:          st.Prompt,
		ReasoningEffort: effort,
		DependsOn:       dependsOn,
		MaxRetries:      st.MaxRetries,
		StepKey:         st.Key,
		Kind:            st.Kind,
		ConfigJSON:      string(configBytes),
	}
}

func (h *WorkflowAdmin) checkWorkflowExists(ctx context.Context, id string) bool {
	if id == "" {
		return false
	}
	var existsID string
	return h.DB.QueryRowContext(ctx, `SELECT id FROM workflows WHERE id=?`, id).Scan(&existsID) == nil
}

// SavePipeline 提供管线保存入口（供 HTTP 接口及 T6 automation_save 复用）。
// needs 自动转换为 depends_on（seq 索引字符串）；type 固定为 'dag'；
// agent 步骤 prompt/reasoning_effort 存入对应列，其余步骤配置存入 config_json。
func (h *WorkflowAdmin) SavePipeline(ctx context.Context, spec pipeline.Spec, id, source string) (wfID, nextRun string, errs []pipeline.FieldError, err error) {
	errs = h.validateSpec(spec)
	if len(errs) > 0 {
		return "", "", errs, nil
	}

	if source == "" {
		source = "user"
	}
	resultAction := spec.ResultAction
	if resultAction == "" {
		resultAction = "session"
	}

	stepRows := convertPipelineStepsToRepoRows(spec.Steps)

	now := time.Now().UTC().Format(time.RFC3339)
	if spec.Trigger.Type == "cron" && spec.Trigger.Cron != "" {
		nextRun = cronadmin.CalcNextRun(spec.Trigger.Cron, now)
	}

	triggerType := spec.Trigger.Type
	if triggerType == "" {
		triggerType = "manual"
	}

	isUpdate := h.checkWorkflowExists(ctx, id)
	if id == "" {
		id = newWorkflowID()
	}

	for i := range stepRows {
		stepRows[i].WorkflowID = id
	}

	wfRow := repo.WorkflowRow{
		ID:           id,
		Type:         "dag",
		Name:         spec.Name,
		Description:  spec.Description,
		TriggerType:  triggerType,
		CronSchedule: spec.Trigger.Cron,
		Enabled:      true,
		NextRunAt:    nextRun,
		CreatedAt:    now,
		UpdatedAt:    now,
		ResultAction: resultAction,
		DeliverFrom:  spec.DeliverFrom,
		Source:       source,
	}

	if isUpdate {
		if err := h.WorkflowRepo.UpdateWorkflowWithSteps(ctx, wfRow, stepRows, true); err != nil {
			return "", "", nil, apperr.Wrap(apperr.CodeInternal, "update workflow with steps", err)
		}
	} else {
		if err := h.WorkflowRepo.CreateWorkflowWithSteps(ctx, wfRow, stepRows); err != nil {
			return "", "", nil, apperr.Wrap(apperr.CodeInternal, "create workflow with steps", err)
		}
	}

	return id, nextRun, nil, nil
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
