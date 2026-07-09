package workflowadmin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
	// Provider selection is now handled internally by AgentPool.

	if err := h.Chat.EnsureSession(ctx, sessionID); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "ensure session", err)
	}

	userMessage := prompt
	if workingDir != "" {
		userMessage = "[工作目录: " + workingDir + "]\n\n" + prompt
	}
	if err := h.Chat.SaveMessage(ctx, sessionID, "user", userMessage, "", "", 0); err != nil {
		slog.Warn("workflow step: saveMessage user failed", "err", err)
	}

	intent := types.Intent{
		Query: prompt,
	}

	res, err := h.AgentPool.AcquireHeadless(ctx, intent)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "headless infer failed", err)
	}

	reply := res.Output
	if err := h.Chat.SaveMessage(ctx, sessionID, "assistant", reply, "", "", 0); err != nil {
		slog.Warn("workflow step: saveMessage assistant failed", "err", err)
	}
	_ = h.Chat.UpdateSessionTitle(ctx, sessionID, name)
	return reply, nil
}
