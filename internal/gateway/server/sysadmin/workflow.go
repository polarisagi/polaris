package sysadmin

import (
	"github.com/polarisagi/polaris/internal/protocol/repo"

	"context"
	"crypto/rand"

	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ─── 数据模型 ─────────────────────────────────────────────────────────────────

type workflow struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	TriggerType   string `json:"trigger_type"`
	CronSchedule  string `json:"cron_schedule"`
	Enabled       bool   `json:"enabled"`
	StepsCount    int    `json:"steps_count"`
	LastRunAt     string `json:"last_run_at"`
	NextRunAt     string `json:"next_run_at"`
	RunCount      int    `json:"run_count"`
	LastRunStatus string `json:"last_run_status"`
	LastRunError  string `json:"last_run_error"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

type workflowStep struct {
	ID              string `json:"id"`
	WorkflowID      string `json:"workflow_id"`
	Seq             int    `json:"seq"`
	Name            string `json:"name"`
	AutomationID    string `json:"automation_id"`
	Prompt          string `json:"prompt"`
	ReasoningEffort string `json:"reasoning_effort"`
	WorkingDir      string `json:"working_dir"`
	InputFromPrev   bool   `json:"input_from_prev"`
}

type workflowRun struct {
	ID          string          `json:"id"`
	WorkflowID  string          `json:"workflow_id"`
	Trigger     string          `json:"trigger"`
	Status      string          `json:"status"`
	CurrentStep int             `json:"current_step"`
	TotalSteps  int             `json:"total_steps"`
	StartedAt   string          `json:"started_at"`
	FinishedAt  string          `json:"finished_at"`
	ErrorMsg    string          `json:"error_msg"`
	StepOutputs json.RawMessage `json:"step_outputs"`
}

type stepOutput struct {
	Seq           int    `json:"seq"`
	SessionID     string `json:"session_id"`
	Status        string `json:"status"`
	OutputPreview string `json:"output_preview"`
}

func newWorkflowID() string {
	b := make([]byte, 8)
	rand.Read(b) //nolint:errcheck
	return "wf_" + hex.EncodeToString(b)
}

func newWorkflowStepID() string {
	b := make([]byte, 8)
	rand.Read(b) //nolint:errcheck
	return "ws_" + hex.EncodeToString(b)
}

func newWorkflowRunID() string {
	b := make([]byte, 8)
	rand.Read(b) //nolint:errcheck
	return "wfr_" + hex.EncodeToString(b)
}

// ─── GET /v1/workflows ────────────────────────────────────────────────────────

func (h *SysAdminHandler) HandleListWorkflows(w http.ResponseWriter, r *http.Request) {
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

func (h *SysAdminHandler) HandleGetWorkflow(w http.ResponseWriter, r *http.Request) {
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

func (h *SysAdminHandler) HandleCreateWorkflow(w http.ResponseWriter, r *http.Request) {
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
		nextRun = calcNextRun(req.CronSchedule, now)
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
func (h *SysAdminHandler) HandleUpdateWorkflow(w http.ResponseWriter, r *http.Request) {
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
		nextRun = calcNextRun(cur.CronSchedule, now)
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

func (h *SysAdminHandler) HandleDeleteWorkflow(w http.ResponseWriter, r *http.Request) {
	wfID := r.PathValue("id")
	if err := h.WorkflowRepo.DeleteWorkflow(r.Context(), wfID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "deleted"}) //nolint:errcheck
}

// ─── POST /v1/workflows/{id}/trigger ─────────────────────────────────────────

func (h *SysAdminHandler) HandleTriggerWorkflow(w http.ResponseWriter, r *http.Request) {
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

func (h *SysAdminHandler) HandleListWorkflowRuns(w http.ResponseWriter, r *http.Request) {
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

// ─── 执行引擎 ─────────────────────────────────────────────────────────────────

// executeWorkflow 顺序执行工作流各步骤，步骤间通过上一步 reply 文本交接数据。
// 异步启动，返回 runID。
//
//nolint:gocyclo,funlen
func (h *SysAdminHandler) executeWorkflow(ctx context.Context, wf *workflow, trigger string) string {
	runID := newWorkflowRunID()
	now := time.Now().UTC().Format(time.RFC3339)

	steps := h.loadWorkflowSteps(ctx, wf.ID)
	total := len(steps)

	if err := h.WorkflowRepo.CreateWorkflowRun(ctx, runID, wf.ID, trigger, "running", 0, total, now); err != nil {
		slog.Warn("workflow: insert run failed", "run", runID, "err", err)
	}

	nextRun := calcNextRun(wf.CronSchedule, now)
	if err := h.WorkflowRepo.UpdateWorkflowLastRun(ctx, wf.ID, now, nextRun, "running", now); err != nil {
		slog.Warn("workflow: update status failed", "id", wf.ID, "err", err)
	}

	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 120*time.Minute)
		defer cancel()

		status := "ok"
		errMsg := ""
		finishedAt := ""
		stepOutputs := make([]stepOutput, 0, total)

		defer func() {
			finishedAt = time.Now().UTC().Format(time.RFC3339)
			outJSON, _ := json.Marshal(stepOutputs)
			if err := h.WorkflowRepo.UpdateWorkflowRunStatus(context.Background(), runID, status, finishedAt, errMsg, string(outJSON), len(stepOutputs)-1); err != nil {
				slog.Warn("workflow: update run failed", "run", runID, "err", err)
			}
			h.updateWorkflowStats(wf.ID, status, errMsg, finishedAt)
		}()

		if total == 0 {
			status = "error"
			errMsg = "workflow has no steps"
			return
		}

		prevOutput := ""

		for i, step := range steps {
			if err := h.WorkflowRepo.UpdateWorkflowRunCurrentStep(bgCtx, runID, i); err != nil {
				slog.Warn("workflow: update current_step failed", "run", runID, "err", err)
			}

			effectivePrompt := step.Prompt
			effectiveWorkingDir := step.WorkingDir
			effectiveEffort := step.ReasoningEffort
			if effectiveEffort == "" {
				effectiveEffort = "medium"
			}

			// 绑定 automation 时以 automation 配置覆盖步骤字段
			if step.AutomationID != "" {
				h.applyAutomationOverride(bgCtx, step.AutomationID, &effectivePrompt, &effectiveWorkingDir, &effectiveEffort)
			}

			// 注入上一步输出（截断至 2000 字符）
			if step.InputFromPrev && prevOutput != "" {
				prefix := prevOutput
				runes := []rune(prefix)
				if len(runes) > 2000 {
					prefix = string(runes[:2000]) + "\n…（已截断）"
				}
				effectivePrompt = "[前一步骤输出]\n" + prefix + "\n[/前一步骤输出]\n\n" + effectivePrompt
			}

			sessionID := newSessionID()
			stepName := step.Name
			if stepName == "" {
				stepName = fmt.Sprintf("%s - 步骤%d", wf.Name, i+1)
			}

			reply, stepErr := h.runWorkflowStep(bgCtx, sessionID, effectivePrompt, effectiveWorkingDir, effectiveEffort, stepName)

			so := stepOutput{Seq: i, SessionID: sessionID, Status: "ok"}
			if stepErr != nil {
				so.Status = "error"
				so.OutputPreview = stepErr.Error()
				stepOutputs = append(stepOutputs, so)
				status = "error"
				errMsg = fmt.Sprintf("step %d (%s) failed: %s", i, step.Name, stepErr.Error())
				slog.Warn("workflow: step failed", "workflow", wf.ID, "step", i, "err", stepErr)
				return
			}

			preview := []rune(reply)
			if len(preview) > 500 {
				so.OutputPreview = string(preview[:500]) + "…"
			} else {
				so.OutputPreview = reply
			}
			stepOutputs = append(stepOutputs, so)
			prevOutput = reply
		}
	}()

	return runID
}

// runWorkflowStep 同步执行单步，返回 Agent 回复文本。
//
//nolint:gocyclo,funlen
func (h *SysAdminHandler) runWorkflowStep(ctx context.Context, sessionID, prompt, workingDir, reasoningEffort, name string) (string, error) {
	p := h.Registry.PickProvider("default")
	if p == nil {
		p = h.Registry.PickProvider("general")
	}
	if p == nil {
		return "", apperr.New(apperr.CodeInternal, "no provider available")
	}

	if err := h.Chat.EnsureSession(ctx, sessionID); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "ensure session", err)
	}

	var history []types.Message
	history = h.Chat.InjectSystemPrompt(history)

	userMessage := prompt
	if workingDir != "" {
		userMessage = "[工作目录: " + workingDir + "]\n\n" + prompt
	}
	history = append(history, types.Message{Role: "user", Content: userMessage})
	if err := h.Chat.SaveMessage(ctx, sessionID, "user", userMessage, "", 0); err != nil {
		slog.Warn("workflow step: saveMessage user failed", "err", err)
	}

	toolSchemas := h.BuildToolSchemas()
	req := &types.InferRequest{
		Messages:        history,
		MaxTokens:       4096,
		Temperature:     0.7,
		Tools:           toolSchemas,
		ReasoningEffort: parseReasoningEffort(reasoningEffort),
	}

	var sb strings.Builder
	const maxToolRounds = 10

	for range maxToolRounds {
		ch, err := p.StreamInfer(ctx, req.Messages)
		if err != nil {
			return "", apperr.Wrap(apperr.CodeInternal, "infer", err)
		}

		var roundText strings.Builder
		var toolCalls []map[string]json.RawMessage

		for ev := range ch {
			switch ev.Type {
			case types.StreamTextDelta:
				if ev.Content != "" {
					roundText.WriteString(ev.Content)
					sb.WriteString(ev.Content)
				}
			case types.StreamToolCall:
				var call map[string]json.RawMessage
				if json.Unmarshal([]byte(ev.Content), &call) == nil {
					toolCalls = append(toolCalls, call)
				}
			}
		}

		if len(toolCalls) == 0 || h.ToolExec == nil {
			break
		}

		assistantParts := make([]any, 0, 1+len(toolCalls))
		if roundText.Len() > 0 {
			assistantParts = append(assistantParts, map[string]any{"type": "text", "text": roundText.String()})
		}
		toolResultParts := make([]any, 0, len(toolCalls))

		for _, tc := range toolCalls {
			var toolID, toolName string
			var inputRaw json.RawMessage
			if b, ok := tc["id"]; ok {
				json.Unmarshal(b, &toolID) //nolint:errcheck
			}
			if b, ok := tc["name"]; ok {
				json.Unmarshal(b, &toolName) //nolint:errcheck
			}
			if b, ok := tc["input"]; ok {
				inputRaw = b
			}
			assistantParts = append(assistantParts, map[string]any{
				"type": "tool_use", "id": toolID, "name": toolName, "input": inputRaw,
			})
			result, execErr := h.ToolExec(ctx, toolName, inputRaw)
			var resultText string
			if execErr != nil {
				resultText = "error: " + execErr.Error()
			} else if result != nil {
				resultText = string(result.Output)
			}
			toolResultParts = append(toolResultParts, map[string]any{
				"type": "tool_result", "tool_use_id": toolID, "content": resultText,
			})
		}
		req.Messages = append(req.Messages,
			types.Message{Role: "assistant", Parts: assistantParts},
			types.Message{Role: "user", Parts: toolResultParts},
		)
	}

	reply := sb.String()
	if err := h.Chat.SaveMessage(ctx, sessionID, "assistant", reply, "", 0); err != nil {
		slog.Warn("workflow step: saveMessage assistant failed", "err", err)
	}
	_ = h.Chat.UpdateSessionTitle(ctx, sessionID, name)
	return reply, nil
}

// ─── Cron 触发（由 cronTick 调用）───────────────────────────────────────────

// cronTickWorkflows 扫描到期的 cron 工作流并触发。
//
//nolint:unused
func (h *SysAdminHandler) cronTickWorkflows(ctx context.Context) {
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
		slog.Warn("cronTickWorkflows: query failed", "err", err)
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
		go h.executeWorkflow(ctx, wf, "cron")
	}
}

// ─── 辅助函数 ─────────────────────────────────────────────────────────────────

// applyAutomationOverride 从已有 automation 加载配置覆盖步骤字段（非空值才覆盖）。
func (h *SysAdminHandler) applyAutomationOverride(ctx context.Context, automationID string, prompt, workingDir, effort *string) {
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

func (h *SysAdminHandler) loadWorkflowSteps(ctx context.Context, wfID string) []workflowStep {
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
func (h *SysAdminHandler) updateWorkflowStats(workflowID, status, errMsg, finishedAt string) {
	bg := context.Background()
	if err := h.WorkflowRepo.UpdateWorkflowStats(bg, workflowID, status, errMsg, finishedAt, circuitBreakThreshold); err != nil {
		slog.Warn("workflow: update stats failed", "id", workflowID, "err", err)
	}
}
