package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"

	"github.com/polarisagi/polaris/configs"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/gateway/channels"

	"gopkg.in/yaml.v3"
)

// ─── 数据模型 ─────────────────────────────────────────────────────────────────

type automation struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Prompt          string `json:"prompt"`
	TriggerType     string `json:"trigger_type"`
	CronSchedule    string `json:"cron_schedule"`
	ChannelID       string `json:"channel_id"`
	WorkingDir      string `json:"working_dir"`
	ReasoningEffort string `json:"reasoning_effort"`
	ResultAction    string `json:"result_action"`
	SandboxLevel    int    `json:"sandbox_level"`
	CedarRulesJSON  string `json:"cedar_rules_json"`
	Enabled         bool   `json:"enabled"`
	LastRunAt       string `json:"last_run_at"`
	NextRunAt       string `json:"next_run_at"`
	RunCount        int    `json:"run_count"`
	LastRunStatus   string `json:"last_run_status"`
	LastRunError    string `json:"last_run_error"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

type automationRun struct {
	ID             string `json:"id"`
	AutomationID   string `json:"automation_id"`
	Trigger        string `json:"trigger"`
	Status         string `json:"status"`
	SessionID      string `json:"session_id"`
	StartedAt      string `json:"started_at"`
	FinishedAt     string `json:"finished_at"`
	ErrorMsg       string `json:"error_msg"`
	PromptSnapshot string `json:"prompt_snapshot"`
}

func newAutoID() string {
	b := make([]byte, 8)
	rand.Read(b) //nolint:errcheck
	return "auto_" + hex.EncodeToString(b)
}

func newRunID() string {
	b := make([]byte, 8)
	rand.Read(b) //nolint:errcheck
	return "run_" + hex.EncodeToString(b)
}

// ─── GET /v1/automations ──────────────────────────────────────────────────────

func (s *Server) handleListAutomations(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id, name, prompt, trigger_type, cron_schedule, channel_id,
		       working_dir, reasoning_effort, result_action,
		       sandbox_level, cedar_rules_json, enabled,
		       last_run_at, next_run_at, run_count, last_run_status, last_run_error,
		       created_at, updated_at
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
			&a.WorkingDir, &a.ReasoningEffort, &a.ResultAction,
			&a.SandboxLevel, &a.CedarRulesJSON, &enabledInt,
			&a.LastRunAt, &a.NextRunAt, &a.RunCount, &a.LastRunStatus, &a.LastRunError,
			&a.CreatedAt, &a.UpdatedAt,
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

func (s *Server) handleCreateAutomation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name            string `json:"name"`
		Prompt          string `json:"prompt"`
		TriggerType     string `json:"trigger_type"`
		CronSchedule    string `json:"cron_schedule"`
		ChannelID       string `json:"channel_id"`
		WorkingDir      string `json:"working_dir"`
		ReasoningEffort string `json:"reasoning_effort"`
		ResultAction    string `json:"result_action"`
		CedarRulesJSON  string `json:"cedar_rules_json"`
		Enabled         *bool  `json:"enabled"`
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

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	now := time.Now().UTC().Format(time.RFC3339)
	id := newAutoID()
	nextRun := ""
	if req.CronSchedule != "" {
		nextRun = calcNextRun(req.CronSchedule, now)
	}

	_, err := s.db.ExecContext(r.Context(), `
		INSERT INTO automations(
			id, name, prompt, trigger_type, cron_schedule, channel_id,
			working_dir, reasoning_effort, result_action,
			sandbox_level, cedar_rules_json, enabled,
			next_run_at, created_at, updated_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, req.Name, req.Prompt, req.TriggerType, req.CronSchedule, req.ChannelID,
		req.WorkingDir, req.ReasoningEffort, req.ResultAction,
		2, req.CedarRulesJSON, boolToInt(enabled),
		nextRun, now, now,
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "created"}) //nolint:errcheck
}

// ─── PUT /v1/automations/{id} ─────────────────────────────────────────────────

func (s *Server) handleUpdateAutomation(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")

	var req struct {
		Name            *string `json:"name"`
		Prompt          *string `json:"prompt"`
		TriggerType     *string `json:"trigger_type"`
		CronSchedule    *string `json:"cron_schedule"`
		ChannelID       *string `json:"channel_id"`
		WorkingDir      *string `json:"working_dir"`
		ReasoningEffort *string `json:"reasoning_effort"`
		ResultAction    *string `json:"result_action"`
		CedarRulesJSON  *string `json:"cedar_rules_json"`
		Enabled         *bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var j automation
	var enabledInt int
	err := s.db.QueryRowContext(r.Context(), `
		SELECT id, name, prompt, trigger_type, cron_schedule, channel_id,
		       working_dir, reasoning_effort, result_action,
		       sandbox_level, cedar_rules_json, enabled
		FROM automations WHERE id=?`, jobID).
		Scan(&j.ID, &j.Name, &j.Prompt, &j.TriggerType, &j.CronSchedule, &j.ChannelID,
			&j.WorkingDir, &j.ReasoningEffort, &j.ResultAction,
			&j.SandboxLevel, &j.CedarRulesJSON, &enabledInt)
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

	now := time.Now().UTC().Format(time.RFC3339)
	nextRun := ""
	if j.CronSchedule != "" {
		nextRun = calcNextRun(j.CronSchedule, now)
	}

	_, err = s.db.ExecContext(r.Context(), `
		UPDATE automations
		SET name=?, prompt=?, trigger_type=?, cron_schedule=?, channel_id=?,
		    working_dir=?, reasoning_effort=?, result_action=?,
		    cedar_rules_json=?, enabled=?, next_run_at=?, updated_at=?
		WHERE id=?`,
		j.Name, j.Prompt, j.TriggerType, j.CronSchedule, j.ChannelID,
		j.WorkingDir, j.ReasoningEffort, j.ResultAction,
		j.CedarRulesJSON, boolToInt(j.Enabled), nextRun, now, jobID,
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "updated"}) //nolint:errcheck
}

// ─── DELETE /v1/automations/{id} ──────────────────────────────────────────────

func (s *Server) handleDeleteAutomation(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")
	s.db.ExecContext(r.Context(), `DELETE FROM automation_runs WHERE automation_id=?`, jobID) //nolint:errcheck
	if _, err := s.db.ExecContext(r.Context(), `DELETE FROM automations WHERE id=?`, jobID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "deleted"}) //nolint:errcheck
}

// ─── GET /v1/automations/{id}/runs ────────────────────────────────────────────

func (s *Server) handleListAutomationRuns(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")
	limitStr := r.URL.Query().Get("limit")
	limit := 20
	if v, err := strconv.Atoi(limitStr); err == nil && v > 0 && v <= 100 {
		limit = v
	}

	rows, err := s.db.QueryContext(r.Context(), `
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

func (s *Server) handleTriggerAutomation(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")

	var a automation
	var enabledInt int
	err := s.db.QueryRowContext(r.Context(), `
		SELECT id, name, prompt, working_dir, reasoning_effort,
		       result_action, sandbox_level, cedar_rules_json, enabled
		FROM automations WHERE id=?`, jobID).
		Scan(&a.ID, &a.Name, &a.Prompt, &a.WorkingDir,
			&a.ReasoningEffort, &a.ResultAction, &a.SandboxLevel, &a.CedarRulesJSON, &enabledInt)
	if err != nil {
		http.Error(w, "automation not found", http.StatusNotFound)
		return
	}
	a.Enabled = enabledInt == 1
	if !a.Enabled {
		http.Error(w, "automation is disabled", http.StatusConflict)
		return
	}

	runID := s.executeAutomation(r.Context(), &a, "manual")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"run_id": runID, "status": "triggered"}) //nolint:errcheck
}

// ─── Cron 后台运行器 ──────────────────────────────────────────────────────────

func (s *Server) startCronRunner(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.cronTick(ctx)
			}
		}
	}()
}

// cronTick 扫描 next_run_at <= NOW() 的任务并触发执行。
func (s *Server) cronTick(ctx context.Context) {
	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, prompt, trigger_type, cron_schedule,
		       working_dir, reasoning_effort, result_action, sandbox_level, cedar_rules_json
		FROM automations
		WHERE enabled=1
		  AND circuit_open=0
		  AND (trigger_type='cron' OR trigger_type='both')
		  AND cron_schedule != ''
		  AND (next_run_at = '' OR next_run_at <= ?)
		  AND last_run_status != 'running'`,
		now)
	if err != nil {
		slog.Warn("cronTick: query failed", "err", err)
		return
	}
	defer rows.Close()

	var due []automation
	for rows.Next() {
		var a automation
		if err := rows.Scan(
			&a.ID, &a.Name, &a.Prompt, &a.TriggerType, &a.CronSchedule,
			&a.WorkingDir, &a.ReasoningEffort, &a.ResultAction, &a.SandboxLevel, &a.CedarRulesJSON,
		); err != nil {
			continue
		}
		due = append(due, a)
	}
	rows.Close()

	for i := range due {
		a := &due[i]
		go s.executeAutomation(ctx, a, "cron")
	}

	// 同批次触发到期工作流
	s.cronTickWorkflows(ctx)
}

type cronCtxKey string

const (
	ctxKeySandboxLevel cronCtxKey = "sandbox_level"
	ctxKeyCedarRules   cronCtxKey = "cedar_rules_json"
)

// executeAutomation 创建 run 记录、调用 Agent 执行、更新状态。
// 返回 runID，异步执行不阻塞调用方。
//
//nolint:gocyclo,funlen
func (s *Server) executeAutomation(ctx context.Context, a *automation, trigger string) string {
	runID := newRunID()
	now := time.Now().UTC().Format(time.RFC3339)

	// 1. 生成 session ID
	sessionID := newSessionID()

	// 2. 写 run 记录（running 状态）
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO automation_runs(id, automation_id, trigger, status, session_id, prompt_snapshot, started_at)
		VALUES(?,?,?,?,?,?,?)`,
		runID, a.ID, trigger, "running", sessionID, a.Prompt, now,
	); err != nil {
		slog.Warn("automation: insert run failed", "run", runID, "err", err)
	}

	// 3. 更新 automations 执行状态
	nextRun := calcNextRun(a.CronSchedule, now)
	if _, err := s.db.ExecContext(ctx, `
		UPDATE automations
		SET last_run_at=?, next_run_at=?, last_run_status='running', updated_at=?
		WHERE id=?`,
		now, nextRun, now, a.ID,
	); err != nil {
		slog.Warn("automation: update status failed", "id", a.ID, "err", err)
	}

	go func() {
		// 动态映射超时
		timeout := 15 * time.Minute
		switch a.ReasoningEffort {
		case "low":
			timeout = 5 * time.Minute
		case "medium":
			timeout = 15 * time.Minute
		case "high":
			timeout = 30 * time.Minute
		case "ultra":
			timeout = 60 * time.Minute
		}

		bgCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		// 注入上下文（Sandbox Level / Cedar Rules）
		bgCtx = context.WithValue(bgCtx, ctxKeySandboxLevel, a.SandboxLevel)
		bgCtx = context.WithValue(bgCtx, ctxKeyCedarRules, a.CedarRulesJSON)

		status := "ok"
		errMsg := ""
		finishedAt := ""

		defer func() {
			finishedAt = time.Now().UTC().Format(time.RFC3339)
			// 更新 run 记录
			if _, err := s.db.ExecContext(context.Background(), `
				UPDATE automation_runs
				SET status=?, finished_at=?, error_msg=?
				WHERE id=?`,
				status, finishedAt, errMsg, runID,
			); err != nil {
				slog.Warn("automation: update run failed", "run", runID, "err", err)
			}

			// 更新 automations 统计（含电路断路器，见 updateAutomationStats）
			s.updateAutomationStats(a.ID, status, errMsg, finishedAt)
		}()

		// 准备 Provider
		p := s.registry.PickProvider("default")
		if p == nil {
			p = s.registry.PickProvider("general")
		}
		if p == nil {
			status = "error"
			errMsg = "no provider available"
			slog.Warn("automation: no provider available", "id", a.ID)
			return
		}

		if err := s.ensureSession(bgCtx, sessionID); err != nil {
			status = "error"
			errMsg = "ensure session failed: " + err.Error()
			return
		}

		// 构建初始 history
		var history []protocol.Message
		history = s.injectSystemPrompt(history)

		// 追加用户消息，working_dir 非空时注入工作目录上下文
		userMessage := a.Prompt
		if a.WorkingDir != "" {
			userMessage = "[工作目录: " + a.WorkingDir + "]\n\n" + a.Prompt
		}
		history = append(history, protocol.Message{Role: "user", Content: userMessage})
		if err := s.saveMessage(bgCtx, sessionID, "user", userMessage, "", 0); err != nil {
			slog.Warn("automation: saveMessage user failed", "err", err)
		}

		// 获取可用工具
		toolSchemas := s.buildToolSchemas()

		// 执行推理
		req := &protocol.InferRequest{
			Messages:        history,
			MaxTokens:       4096,
			Temperature:     0.7,
			Tools:           toolSchemas,
			ReasoningEffort: parseReasoningEffort(a.ReasoningEffort),
		}

		startInfer := time.Now()
		var sb strings.Builder
		const maxToolRounds = 10

		for range maxToolRounds {
			ch, err := p.StreamInfer(bgCtx, req)
			if err != nil {
				status = "error"
				errMsg = "infer failed: " + err.Error()
				return
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

				result, execErr := s.toolExec(bgCtx, toolName, inputRaw)
				var resultText string
				if execErr != nil {
					resultText = "error: " + execErr.Error()
				} else if result != nil {
					resultText = string(result.Output)
				}
				slog.Info("automation: tool executed", "name", toolName, "ok", execErr == nil)
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
		latencyMs := time.Since(startInfer).Milliseconds()

		if err := s.saveMessage(bgCtx, sessionID, "assistant", reply, "", latencyMs); err != nil {
			slog.Warn("automation: saveMessage assistant failed", "err", err)
		}
		_ = s.updateSessionTitle(bgCtx, sessionID, a.Name)

		// 处理 result_action
		if chID, ok := strings.CutPrefix(a.ResultAction, "channel:"); ok {
			// 向 Channel 发送消息：原 channelType="" 导致 SendReply 走 default 分支静默丢弃。
			// 须先从 DB 读取 channel 的 type 和 config_json，才能正确分发。
			var chType, cfgJSON string
			if qErr := s.db.QueryRowContext(bgCtx,
				`SELECT type, config_json FROM channels WHERE id=?`, chID).
				Scan(&chType, &cfgJSON); qErr == nil {
				var cfg map[string]any
				_ = json.Unmarshal([]byte(cfgJSON), &cfg)
				s.channelMgr.SendReply(bgCtx, chType, chID, cfg, channels.Message{ChatID: ""}, reply)
			} else {
				slog.Warn("automation: channel not found for result_action",
					"automation_id", a.ID, "channel_id", chID, "err", qErr)
			}
		}
	}()

	return runID
}

func parseReasoningEffort(e string) protocol.ReasoningEffort {
	switch e {
	case "low":
		return protocol.ReasoningEffortLow
	case "medium":
		return protocol.ReasoningEffortMedium
	case "high":
		return protocol.ReasoningEffortHigh
	case "ultra":
		return protocol.ReasoningEffortHigh // ultra map to high
	default:
		return protocol.ReasoningEffortMedium
	}
}

// ─── Cron 表达式解析（简化版，支持标准5字段格式 + @daily/@weekly 别名）────────

// updateAutomationStats 更新 automations 表统计字段，含 Gap-C 电路断路器逻辑。
// 连续 circuitThreshold 次 error → 置 circuit_open=1，cronTick 跳过该任务。
// status=ok 时清零 failure_count 和 circuit_open（断路自愈）。
const circuitBreakThreshold = 5

func (s *Server) updateAutomationStats(automationID, status, errMsg, finishedAt string) {
	bg := context.Background()
	if status == "error" {
		if _, err := s.db.ExecContext(bg, `
			UPDATE automations
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
			finishedAt, automationID,
		); err != nil {
			slog.Warn("automation: update stats failed", "id", automationID, "err", err)
			return
		}
		var circuitOpen int
		_ = s.db.QueryRowContext(bg, `SELECT circuit_open FROM automations WHERE id=?`, automationID).Scan(&circuitOpen)
		if circuitOpen == 1 {
			slog.Warn("automation: circuit opened — consecutive failures exceeded threshold",
				"id", automationID, "threshold", circuitBreakThreshold)
		}
		return
	}
	// status=ok 或 running 结束：清零断路器
	if _, err := s.db.ExecContext(bg, `
		UPDATE automations
		SET last_run_status=?, last_run_error='',
		    run_count=run_count+1,
		    failure_count=0, circuit_open=0, circuit_opened_at='',
		    updated_at=?
		WHERE id=?`,
		status, finishedAt, automationID,
	); err != nil {
		slog.Warn("automation: update stats failed", "id", automationID, "err", err)
	}
}

// calcNextRun 基于当前时间计算下次触发时间（RFC3339）。
//
//nolint:gocyclo
func calcNextRun(expr, fromRFC3339 string) string {
	from, err := time.Parse(time.RFC3339, fromRFC3339)
	if err != nil {
		from = time.Now().UTC()
	}

	// 语义别名展开
	switch strings.TrimSpace(expr) {
	case "@hourly":
		expr = "0 * * * *"
	case "@daily", "@midnight":
		expr = "0 0 * * *"
	case "@weekly":
		expr = "0 0 * * 0"
	case "@monthly":
		expr = "0 0 1 * *"
	}

	// 去掉秒字段（6字段 → 5字段）
	parts := strings.Fields(expr)
	if len(parts) == 6 {
		parts = parts[1:]
	}
	if len(parts) != 5 {
		return ""
	}

	minuteMatch := false
	hourMatch := false
	domMatch := false
	monthMatch := false
	dowMatch := false

	// parse
	minStep, minFixed := parseCronField(parts[0])
	hourStep, hourFixed := parseCronField(parts[1])
	domStep, domFixed := parseCronField(parts[2])
	monthStep, monthFixed := parseCronField(parts[3])
	dowStep, dowFixed := parseCronField(parts[4])

	t := from.Add(time.Minute).Truncate(time.Minute)
	// 从 from+1min 开始向前推，找下一个匹配时刻（最多搜索 1 年）
	for range 525600 { // 最多 365 天 × 1440 分钟
		minuteMatch = (minFixed == -1 && t.Minute()%minStep == 0) || (minFixed != -1 && t.Minute() == minFixed)
		hourMatch = (hourFixed == -1 && t.Hour()%hourStep == 0) || (hourFixed != -1 && t.Hour() == hourFixed)
		domMatch = (domFixed == -1 && t.Day()%domStep == 0) || (domFixed != -1 && t.Day() == domFixed)
		monthMatch = (monthFixed == -1 && int(t.Month())%monthStep == 0) || (monthFixed != -1 && int(t.Month()) == monthFixed)
		dowMatch = (dowFixed == -1 && int(t.Weekday())%dowStep == 0) || (dowFixed != -1 && int(t.Weekday()) == dowFixed)

		if minuteMatch && hourMatch && domMatch && monthMatch && dowMatch {
			return t.UTC().Format(time.RFC3339)
		}
		t = t.Add(time.Minute)
	}
	return ""
}

// parseCronField 解析字段，返回 step 和 fixed (-1 为通配)。
// 对于 "*" 返回 1, -1。对于 "*/n" 返回 n, -1。对于 "n" 返回 1, n。
func parseCronField(part string) (int, int) {
	if part == "*" {
		return 1, -1
	}
	if strings.HasPrefix(part, "*/") {
		step, err := strconv.Atoi(part[2:])
		if err == nil && step > 0 {
			return step, -1
		}
		return 1, -1 // fallback
	}
	if fixed, err := strconv.Atoi(part); err == nil {
		return 1, fixed
	}
	return 1, -1 // fallback
}

// ─── 自动化模板市场 ───────────────────────────────────────────────────────────

// automationTemplate 对应 configs/automations/templates/*.yaml 或远程 index.json 中的单条记录。
type automationTemplate struct {
	Icon            string   `yaml:"icon"             json:"icon"`
	Name            string   `yaml:"name"             json:"name"`
	Description     string   `yaml:"description"      json:"description"`
	Prompt          string   `yaml:"prompt"           json:"prompt"`
	TriggerType     string   `yaml:"trigger_type"     json:"trigger_type"`
	CronSchedule    string   `yaml:"cron_schedule"    json:"cron_schedule"`
	ReasoningEffort string   `yaml:"reasoning_effort" json:"reasoning_effort"`
	Tags            []string `yaml:"tags"             json:"tags,omitempty"`
	Source          string   `yaml:"source"           json:"source,omitempty"`
	Author          string   `yaml:"author"           json:"author,omitempty"`
}

// automationSource 对应 configs/automation_sources.yaml 中的单条来源配置。
type automationSource struct {
	ID          string `yaml:"id"`
	Name        string `yaml:"name"`
	Type        string `yaml:"type"` // local | remote
	Path        string `yaml:"path"` // type=local 时有效
	URL         string `yaml:"url"`  // type=remote 时有效
	Description string `yaml:"description"`
	Enabled     bool   `yaml:"enabled"`
	TrustTier   int    `yaml:"trust_tier"`
}

// remoteIndex 是远程 index.json 的顶层结构。
type remoteIndex struct {
	Templates []automationTemplate `json:"templates"`
}

// templateCache 存放远程拉取结果，避免每次请求都走网络。
type templateCache struct {
	templates []automationTemplate
	fetchedAt time.Time
}

// Server 侧的模板缓存（按来源 ID 存储）。
// 用 sync.Map 保证并发安全；TTL 1h，超时则重新拉取。
var templateCacheMap sync.Map // map[string]*templateCache

const templateCacheTTL = time.Hour

// loadEmbeddedTemplates 从 embed.FS 读取内置模板目录（二进制完全自包含，不依赖工作目录）。
func loadEmbeddedTemplates(dir string) []automationTemplate {
	entries, err := configs.FS.ReadDir(dir)
	if err != nil {
		return nil
	}
	var all []automationTemplate
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		b, err := configs.FS.ReadFile(dir + "/" + e.Name())
		if err != nil {
			slog.Warn("automation-templates: embedded read failed", "file", e.Name(), "err", err)
			continue
		}
		var tpls []automationTemplate
		if err := yaml.Unmarshal(b, &tpls); err != nil {
			slog.Warn("automation-templates: embedded parse failed", "file", e.Name(), "err", err)
			continue
		}
		all = append(all, tpls...)
	}
	return all
}

// loadLocalTemplates 扫描用户配置的本地目录（automation_sources.yaml type=local 路径）。
// 仅用于 Operator 自定义模板；内置模板统一走 loadEmbeddedTemplates。
func loadLocalTemplates(dir string) []automationTemplate {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var all []automationTemplate
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			slog.Warn("automation-templates: read failed", "file", e.Name(), "err", err)
			continue
		}
		var tpls []automationTemplate
		if err := yaml.Unmarshal(b, &tpls); err != nil {
			slog.Warn("automation-templates: parse failed", "file", e.Name(), "err", err)
			continue
		}
		all = append(all, tpls...)
	}
	return all
}

// fetchRemoteTemplates 拉取远程 index.json，命中缓存则直接返回。
func (s *Server) fetchRemoteTemplates(src automationSource) []automationTemplate {
	if val, ok := templateCacheMap.Load(src.ID); ok {
		if c, isType := val.(*templateCache); isType && time.Since(c.fetchedAt) < templateCacheTTL {
			return c.templates
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.URL, nil)
	if err != nil {
		slog.Warn("automation-templates: bad remote url", "id", src.ID, "err", err)
		return nil
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "polaris/1.0")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		slog.Warn("automation-templates: fetch failed", "id", src.ID, "err", err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		slog.Warn("automation-templates: remote returned non-200", "id", src.ID, "status", resp.StatusCode, "err", perrors.New(perrors.CodeInternal, "log event"))
		return nil
	}

	var idx remoteIndex
	if err := json.NewDecoder(resp.Body).Decode(&idx); err != nil {
		slog.Warn("automation-templates: decode failed", "id", src.ID, "err", err)
		return nil
	}

	// 注入来源标识（覆盖远程可能缺失的 source 字段）
	for i := range idx.Templates {
		if idx.Templates[i].Source == "" {
			idx.Templates[i].Source = src.ID
		}
	}

	templateCacheMap.Store(src.ID, &templateCache{templates: idx.Templates, fetchedAt: time.Now()})
	slog.Info("automation-templates: remote fetched", "id", src.ID, "count", len(idx.Templates))
	return idx.Templates
}

// loadSources 从 embed.FS 读取 extensions/automation_sources.yaml（二进制自包含，不依赖工作目录）。
func loadSources() []automationSource {
	b, err := configs.FS.ReadFile("extensions/automation_sources.yaml")
	if err != nil {
		return nil
	}
	var srcs []automationSource
	if err := yaml.Unmarshal(b, &srcs); err != nil {
		slog.Warn("automation-sources: parse failed", "err", err)
		return nil
	}
	return srcs
}

// GET /v1/automation-templates
// 合并所有已启用来源（local YAML + 远程 index）返回模板列表。
// 查询参数：?source=<id> 可过滤单一来源；?tag=<tag> 过滤标签。
func (s *Server) handleListAutomationTemplates(w http.ResponseWriter, r *http.Request) {
	filterSource := r.URL.Query().Get("source")
	filterTag := r.URL.Query().Get("tag")

	srcs := loadSources()
	var all []automationTemplate

	for _, src := range srcs {
		if !src.Enabled {
			continue
		}
		if filterSource != "" && src.ID != filterSource {
			continue
		}
		var tpls []automationTemplate
		switch src.Type {
		case "local":
			tpls = loadLocalTemplates(src.Path)
		case "remote":
			if src.URL != "" {
				tpls = s.fetchRemoteTemplates(src)
			}
		}
		all = append(all, tpls...)
	}

	// 无有效来源时 fallback 到内置模板（从 embed.FS 读取，不依赖工作目录）
	if len(all) == 0 && filterSource == "" {
		all = loadEmbeddedTemplates("automations/templates")
	}

	// 标签过滤
	if filterTag != "" {
		var filtered []automationTemplate
		for _, t := range all {
			if slices.Contains(t.Tags, filterTag) {
				filtered = append(filtered, t)
			}
		}
		all = filtered
	}

	if all == nil {
		all = []automationTemplate{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"templates": all}) //nolint:errcheck
}
