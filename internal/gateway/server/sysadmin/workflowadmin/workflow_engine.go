package workflowadmin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin/cronadmin"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// ─── 执行引擎 ─────────────────────────────────────────────────────────────────

// executeWorkflow 顺序执行工作流各步骤，步骤间通过上一步 reply 文本交接数据。
// 异步启动，返回 runID。
//
//nolint:gocyclo,funlen
func (h *WorkflowAdmin) executeWorkflow(ctx context.Context, wf *workflow, trigger string) string {
	runID := newWorkflowRunID()
	now := time.Now().UTC().Format(time.RFC3339)

	steps := h.loadWorkflowSteps(ctx, wf.ID)
	total := len(steps)

	if err := h.WorkflowRepo.CreateWorkflowRun(ctx, runID, wf.ID, trigger, "running", 0, total, now); err != nil {
		slog.Warn("workflow: insert run failed", "run", runID, "err", err)
	}

	nextRun := cronadmin.CalcNextRun(wf.CronSchedule, now)
	if err := h.WorkflowRepo.UpdateWorkflowLastRun(ctx, wf.ID, now, nextRun, "running", now); err != nil {
		slog.Warn("workflow: update status failed", "id", wf.ID, "err", err)
	}

	concurrent.SafeGo(context.Background(), "gateway.sysadmin.execute_workflow", func(context.Context) {
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

			sessionID := cronadmin.NewSessionID()
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
	})

	return runID
}

// runWorkflowStep 同步执行单步，返回 Agent 回复文本。
//
//nolint:gocyclo,funlen
func (h *WorkflowAdmin) runWorkflowStep(ctx context.Context, sessionID, prompt, workingDir, reasoningEffort, name string) (string, error) {
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
	history = h.Chat.InjectSystemPrompt(ctx, h.Agent, history, prompt)

	userMessage := prompt
	if workingDir != "" {
		userMessage = "[工作目录: " + workingDir + "]\n\n" + prompt
	}
	history = append(history, types.Message{Role: "user", Content: userMessage})
	if err := h.Chat.SaveMessage(ctx, sessionID, "user", userMessage, "", "", 0); err != nil {
		slog.Warn("workflow step: saveMessage user failed", "err", err)
	}

	toolSchemas := h.BuildToolSchemas()
	req := &types.InferRequest{
		Messages:        history,
		MaxTokens:       4096,
		Temperature:     0.7,
		Tools:           toolSchemas,
		ReasoningEffort: cronadmin.ParseReasoningEffort(reasoningEffort),
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
	if err := h.Chat.SaveMessage(ctx, sessionID, "assistant", reply, "", "", 0); err != nil {
		slog.Warn("workflow step: saveMessage assistant failed", "err", err)
	}
	_ = h.Chat.UpdateSessionTitle(ctx, sessionID, name)
	return reply, nil
}
