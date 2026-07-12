package workflowadmin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/execute/orchestrator"
	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin/cronadmin"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// ─── 执行引擎 ─────────────────────────────────────────────────────────────────

// executeWorkflow 经 StateGraphExecutor（编排模式10）执行工作流（2026-07-12 由
// 顺序 for 循环改造，见 workflow_graph.go buildGraphSpec）：
//   - wf.Type=="chain"（默认）：忽略 depends_on，按 Seq 合成顺序链，与旧实现逐字节
//     等价（单前驱、无自环时 AND-Join 记账退化为既有"立即触发"语义）；
//   - wf.Type=="dag"：如实按 depends_on 构造并行边，多前驱 AND-Join 等待全部完成；
//   - max_retries>0 的步骤经自环条件边失败自动重试。
//
// 实际步骤执行（含 automation 覆盖/input_from_prev 注入/AgentPool 调用）已下沉到
// RunStepWorkerLoop（workflow_step_worker.go）——StateGraphExecutor 只通过
// Blackboard 投递/订阅任务，不直接调用 Agent。step_outputs/current_step 由该
// Worker 以原子 SQL（json_insert/自增）增量写入，兼容 DAG 并行下的并发完成，此处
// 只在 Execute 返回后读回判定整体成功/失败并推进 status/finished_at。
// 异步启动，返回 runID。
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

		defer func() {
			finishedAt := time.Now().UTC().Format(time.RFC3339)
			// stepOutputs 必须读回已由 RunStepWorkerLoop 增量落盘的当前值再原样传回：
			// UpdateWorkflowRunStatus 的 SQL 是整列覆盖式 UPDATE（非 json_insert 追加），
			// 传空字符串会把 Worker 已写入的执行历史整体清空为 "[]"。
			if err := h.WorkflowRepo.UpdateWorkflowRunStatus(context.Background(), runID, status, finishedAt, errMsg, h.readRunStepOutputsJSON(context.Background(), runID), h.readRunCurrentStep(context.Background(), runID)); err != nil {
				slog.Warn("workflow: update run failed", "run", runID, "err", err)
			}
			h.updateWorkflowStats(wf.ID, status, errMsg, finishedAt)
		}()

		if total == 0 {
			status = "error"
			errMsg = "workflow has no steps"
			return
		}
		if h.Blackboard == nil {
			// fail-closed：宁可显式报错，也不让 StateGraphExecutor 在 nil Blackboard
			// 上调用 Subscribe 触发 nil pointer panic（虽会被 SafeGo 兜底恢复，但
			// 会把这次 run 误标记为进行中/无明确错误原因）。
			status = "error"
			errMsg = "workflow: blackboard not configured, cannot execute"
			slog.Error("workflow: Blackboard is nil, cannot execute state graph", "workflow", wf.ID, "run", runID)
			return
		}

		spec := buildGraphSpec(wf.Type, wf.ID, runID, steps)
		executor := orchestrator.NewStateGraphExecutor(h.Blackboard)
		if err := executor.Execute(bgCtx, runID, spec); err != nil {
			status = "error"
			errMsg = err.Error()
			slog.Warn("workflow: state graph execution failed", "workflow", wf.ID, "run", runID, "err", err)
			return
		}

		// Execute 返回 nil 不等于全部成功：重试自环耗尽 MaxVisits 后仍可能停在
		// status=error 的终态节点而不产生任何"错误"（见 pattern_state_graph_test.go
		// TestStateGraphExecutor_LoopExhaustsMaxVisitsWithoutPassing）。读回 Worker
		// 已增量落盘的 step_outputs 判定整体结果。
		if failCount, firstErr := h.scanStepOutputErrors(context.Background(), runID); failCount > 0 {
			status = "error"
			errMsg = fmt.Sprintf("%d step(s) failed: %s", failCount, firstErr)
		}
	})

	return runID
}

// readRunCurrentStep 读回 RunStepWorkerLoop 已增量自增的 current_step，避免
// executeWorkflow 收尾时用旧的"顺序位置"语义覆盖 DAG 并行下的真实完成计数。
func (h *WorkflowAdmin) readRunCurrentStep(ctx context.Context, runID string) int {
	var n int
	if err := h.DB.QueryRowContext(ctx, `SELECT current_step FROM workflow_runs WHERE id=?`, runID).Scan(&n); err != nil {
		return 0
	}
	return n
}

// readRunStepOutputsJSON 读回 RunStepWorkerLoop 已增量落盘的 step_outputs 原始
// JSON（供 defer 收尾时原样传回 UpdateWorkflowRunStatus，避免整列覆盖式 UPDATE
// 把已写入的执行历史清空）。查询失败或空值均降级为 "[]"，不中断收尾流程。
func (h *WorkflowAdmin) readRunStepOutputsJSON(ctx context.Context, runID string) string {
	var raw sql.NullString
	if err := h.DB.QueryRowContext(ctx, `SELECT step_outputs FROM workflow_runs WHERE id=?`, runID).Scan(&raw); err != nil || !raw.Valid || raw.String == "" {
		return "[]"
	}
	return raw.String
}

// scanStepOutputErrors 读回 workflow_runs.step_outputs 判定是否存在失败步骤。
func (h *WorkflowAdmin) scanStepOutputErrors(ctx context.Context, runID string) (int, string) {
	var raw []byte
	if err := h.DB.QueryRowContext(ctx, `SELECT step_outputs FROM workflow_runs WHERE id=?`, runID).Scan(&raw); err != nil {
		return 0, ""
	}
	var outputs []stepOutput
	if err := json.Unmarshal(raw, &outputs); err != nil {
		return 0, ""
	}
	failCount := 0
	firstErr := ""
	for _, o := range outputs {
		if o.Status == "error" {
			failCount++
			if firstErr == "" {
				firstErr = fmt.Sprintf("step %d: %s", o.Seq, o.OutputPreview)
			}
		}
	}
	return failCount, firstErr
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
