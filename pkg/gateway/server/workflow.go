package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
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

func (s *Server) handleListWorkflows(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), `
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

func (s *Server) handleGetWorkflow(w http.ResponseWriter, r *http.Request) {
	wfID := r.PathValue("id")

	var wf workflow
	var enabledInt int
	err := s.db.QueryRowContext(r.Context(), `
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

	steps := s.loadWorkflowSteps(r.Context(), wfID)
	if steps == nil {
		steps = []workflowStep{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"workflow": wf, "steps": steps}) //nolint:errcheck
}

// ─── POST /v1/workflows ───────────────────────────────────────────────────────

func (s *Server) handleCreateWorkflow(w http.ResponseWriter, r *http.Request) {
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

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(r.Context(), `
		INSERT INTO workflows(id, name, description, trigger_type, cron_schedule, enabled,
		                      next_run_at, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?)`,
		id, req.Name, req.Description, req.TriggerType, req.CronSchedule,
		boolToInt(enabled), nextRun, now, now,
	); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	for i, st := range req.Steps {
		if err := insertWorkflowStep(r.Context(), tx, id, i, st); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "created"}) //nolint:errcheck
}

// ─── PUT /v1/workflows/{id} ───────────────────────────────────────────────────

//nolint:gocyclo
func (s *Server) handleUpdateWorkflow(w http.ResponseWriter, r *http.Request) {
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
	if err := s.db.QueryRowContext(r.Context(), `
		SELECT id, name, description, trigger_type, cron_schedule, enabled
		FROM workflows WHERE id=?`, wfID).Scan(
		&cur.ID, &cur.Name, &cur.Description, &cur.TriggerType, &cur.CronSchedule, &enabledInt,
	); err != nil {
		http.Error(w, "workflow not found", http.StatusNotFound)
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

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(r.Context(), `
		UPDATE workflows SET name=?, description=?, trigger_type=?, cron_schedule=?,
		       enabled=?, next_run_at=?, updated_at=?
		WHERE id=?`,
		cur.Name, cur.Description, cur.TriggerType, cur.CronSchedule,
		boolToInt(cur.Enabled), nextRun, now, wfID,
	); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if req.Steps != nil {
		if _, err := tx.ExecContext(r.Context(),
			`DELETE FROM workflow_steps WHERE workflow_id=?`, wfID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for i, st := range req.Steps {
			if err := insertWorkflowStep(r.Context(), tx, wfID, i, st); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
	}

	if err := tx.Commit(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "updated"}) //nolint:errcheck
}

// ─── DELETE /v1/workflows/{id} ────────────────────────────────────────────────

func (s *Server) handleDeleteWorkflow(w http.ResponseWriter, r *http.Request) {
	wfID := r.PathValue("id")
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback() //nolint:errcheck

	for _, q := range []string{
		`DELETE FROM workflow_runs WHERE workflow_id=?`,
		`DELETE FROM workflow_steps WHERE workflow_id=?`,
		`DELETE FROM workflows WHERE id=?`,
	} {
		if _, err := tx.ExecContext(r.Context(), q, wfID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "deleted"}) //nolint:errcheck
}

// ─── POST /v1/workflows/{id}/trigger ─────────────────────────────────────────

func (s *Server) handleTriggerWorkflow(w http.ResponseWriter, r *http.Request) {
	wfID := r.PathValue("id")

	var wf workflow
	var enabledInt int
	if err := s.db.QueryRowContext(r.Context(), `
		SELECT id, name, description, trigger_type, cron_schedule, enabled
		FROM workflows WHERE id=?`, wfID).Scan(
		&wf.ID, &wf.Name, &wf.Description, &wf.TriggerType, &wf.CronSchedule, &enabledInt,
	); err != nil {
		http.Error(w, "workflow not found", http.StatusNotFound)
		return
	}
	wf.Enabled = enabledInt == 1

	runID := s.executeWorkflow(r.Context(), &wf, "manual")

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"run_id": runID, "status": "triggered"}) //nolint:errcheck
}

// ─── GET /v1/workflows/{id}/runs ─────────────────────────────────────────────

func (s *Server) handleListWorkflowRuns(w http.ResponseWriter, r *http.Request) {
	wfID := r.PathValue("id")

	rows, err := s.db.QueryContext(r.Context(), `
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
func (s *Server) executeWorkflow(ctx context.Context, wf *workflow, trigger string) string {
	runID := newWorkflowRunID()
	now := time.Now().UTC().Format(time.RFC3339)

	steps := s.loadWorkflowSteps(ctx, wf.ID)
	total := len(steps)

	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO workflow_runs(id, workflow_id, trigger, status, current_step, total_steps, started_at)
		VALUES(?,?,?,?,?,?,?)`,
		runID, wf.ID, trigger, "running", 0, total, now,
	); err != nil {
		slog.Warn("workflow: insert run failed", "run", runID, "err", err)
	}

	nextRun := calcNextRun(wf.CronSchedule, now)
	if _, err := s.db.ExecContext(ctx, `
		UPDATE workflows SET last_run_at=?, next_run_at=?, last_run_status='running', updated_at=?
		WHERE id=?`,
		now, nextRun, now, wf.ID,
	); err != nil {
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
			if _, err := s.db.ExecContext(context.Background(), `
				UPDATE workflow_runs
				SET status=?, finished_at=?, error_msg=?, step_outputs=?, current_step=?
				WHERE id=?`,
				status, finishedAt, errMsg, string(outJSON), len(stepOutputs)-1, runID,
			); err != nil {
				slog.Warn("workflow: update run failed", "run", runID, "err", err)
			}
			s.updateWorkflowStats(wf.ID, status, errMsg, finishedAt)
		}()

		if total == 0 {
			status = "error"
			errMsg = "workflow has no steps"
			return
		}

		prevOutput := ""

		for i, step := range steps {
			if _, err := s.db.ExecContext(bgCtx,
				`UPDATE workflow_runs SET current_step=? WHERE id=?`, i, runID,
			); err != nil {
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
				s.applyAutomationOverride(bgCtx, step.AutomationID, &effectivePrompt, &effectiveWorkingDir, &effectiveEffort)
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

			reply, stepErr := s.runWorkflowStep(bgCtx, sessionID, effectivePrompt, effectiveWorkingDir, effectiveEffort, stepName)

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
func (s *Server) runWorkflowStep(ctx context.Context, sessionID, prompt, workingDir, reasoningEffort, name string) (string, error) {
	p := s.registry.PickProvider("default")
	if p == nil {
		p = s.registry.PickProvider("general")
	}
	if p == nil {
		return "", fmt.Errorf("no provider available")
	}

	if err := s.ensureSession(ctx, sessionID); err != nil {
		return "", fmt.Errorf("ensure session: %w", err)
	}

	var history []protocol.Message
	history = s.injectSystemPrompt(history)

	userMessage := prompt
	if workingDir != "" {
		userMessage = "[工作目录: " + workingDir + "]\n\n" + prompt
	}
	history = append(history, protocol.Message{Role: "user", Content: userMessage})
	if err := s.saveMessage(ctx, sessionID, "user", userMessage, "", 0); err != nil {
		slog.Warn("workflow step: saveMessage user failed", "err", err)
	}

	toolSchemas := s.buildToolSchemas()
	req := &protocol.InferRequest{
		Messages:        history,
		MaxTokens:       4096,
		Temperature:     0.7,
		Tools:           toolSchemas,
		ReasoningEffort: parseReasoningEffort(reasoningEffort),
	}

	var sb strings.Builder
	const maxToolRounds = 10

	for range maxToolRounds {
		ch, err := p.StreamInfer(ctx, req)
		if err != nil {
			return "", fmt.Errorf("infer: %w", err)
		}

		var roundText strings.Builder
		var toolCalls []map[string]json.RawMessage

		for ev := range ch {
			switch ev.Type {
			case protocol.StreamTextDelta:
				if ev.Content != "" {
					roundText.WriteString(ev.Content)
					sb.WriteString(ev.Content)
				}
			case protocol.StreamToolCall:
				var call map[string]json.RawMessage
				if json.Unmarshal([]byte(ev.Content), &call) == nil {
					toolCalls = append(toolCalls, call)
				}
			}
		}

		if len(toolCalls) == 0 || s.toolExec == nil {
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
			result, execErr := s.toolExec(ctx, toolName, inputRaw)
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
			protocol.Message{Role: "assistant", Parts: assistantParts},
			protocol.Message{Role: "user", Parts: toolResultParts},
		)
	}

	reply := sb.String()
	if err := s.saveMessage(ctx, sessionID, "assistant", reply, "", 0); err != nil {
		slog.Warn("workflow step: saveMessage assistant failed", "err", err)
	}
	_ = s.updateSessionTitle(ctx, sessionID, name)
	return reply, nil
}

// ─── Cron 触发（由 cronTick 调用）───────────────────────────────────────────

// cronTickWorkflows 扫描到期的 cron 工作流并触发。
func (s *Server) cronTickWorkflows(ctx context.Context) {
	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := s.db.QueryContext(ctx, `
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
		go s.executeWorkflow(ctx, wf, "cron")
	}
}

// ─── 辅助函数 ─────────────────────────────────────────────────────────────────

// applyAutomationOverride 从已有 automation 加载配置覆盖步骤字段（非空值才覆盖）。
func (s *Server) applyAutomationOverride(ctx context.Context, automationID string, prompt, workingDir, effort *string) {
	var aPrompt, aDir, aEffort string
	err := s.db.QueryRowContext(ctx, `
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

func (s *Server) loadWorkflowSteps(ctx context.Context, wfID string) []workflowStep {
	rows, err := s.db.QueryContext(ctx, `
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

func insertWorkflowStep(ctx context.Context, tx *sql.Tx, wfID string, idx int, st workflowStep) error {
	if st.ReasoningEffort == "" {
		st.ReasoningEffort = "medium"
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO workflow_steps(id, workflow_id, seq, name, automation_id, prompt,
		                           reasoning_effort, working_dir, input_from_prev)
		VALUES(?,?,?,?,?,?,?,?,?)`,
		newWorkflowStepID(), wfID, idx, st.Name, st.AutomationID, st.Prompt,
		st.ReasoningEffort, st.WorkingDir, boolToInt(st.InputFromPrev),
	)
	return err
}

// updateWorkflowStats 更新 workflows 统计字段 + 电路断路器。
func (s *Server) updateWorkflowStats(workflowID, status, errMsg, finishedAt string) {
	bg := context.Background()
	if status == "error" {
		if _, err := s.db.ExecContext(bg, `
			UPDATE workflows
			SET last_run_status=?, last_run_error=?,
			    run_count=run_count+1,
			    failure_count=failure_count+1,
			    circuit_open=CASE WHEN failure_count+1 >= ? THEN 1 ELSE circuit_open END,
			    circuit_opened_at=CASE WHEN failure_count+1 >= ? AND circuit_open=0
			                          THEN ? ELSE circuit_opened_at END,
			    updated_at=?
			WHERE id=?`,
			status, errMsg,
			circuitBreakThreshold, circuitBreakThreshold, finishedAt,
			finishedAt, workflowID,
		); err != nil {
			slog.Warn("workflow: update stats failed", "id", workflowID, "err", err)
		}
		return
	}
	if _, err := s.db.ExecContext(bg, `
		UPDATE workflows
		SET last_run_status=?, last_run_error='',
		    run_count=run_count+1,
		    failure_count=0, circuit_open=0, circuit_opened_at='',
		    updated_at=?
		WHERE id=?`,
		status, finishedAt, workflowID,
	); err != nil {
		slog.Warn("workflow: update stats failed", "id", workflowID, "err", err)
	}
}
