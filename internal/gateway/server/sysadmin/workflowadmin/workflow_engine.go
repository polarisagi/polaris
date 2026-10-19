package workflowadmin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/polarisagi/polaris/internal/automation/pipeline"
	"github.com/polarisagi/polaris/internal/execute/orchestrator"
	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin/cronadmin"
	"github.com/polarisagi/polaris/internal/gateway/session"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
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
	steps := h.loadWorkflowSteps(ctx, wf.ID)
	total := len(steps)
	runID := newWorkflowRunID()
	now := time.Now().UTC().Format(time.RFC3339)

	// 1. 后台资源准入判定（Admitter）
	var release func()
	if h.Admitter != nil {
		var ok bool
		release, ok = h.Admitter.AdmitBackground("workflow")
		if !ok {
			h.recordResourcePressureSkip(ctx, runID, wf.ID, trigger, now, total)
			return runID
		}
	}

	if err := h.WorkflowRepo.CreateWorkflowRun(ctx, runID, wf.ID, trigger, "running", 0, total, now); err != nil {
		slog.Warn("workflow: insert run failed", "run", runID, "err", err)
	}

	nextRun := cronadmin.CalcNextRun(wf.CronSchedule, now)
	if err := h.WorkflowRepo.UpdateWorkflowLastRun(ctx, wf.ID, now, nextRun, "running", now); err != nil {
		slog.Warn("workflow: update status failed", "id", wf.ID, "err", err)
	}

	date := time.Now().Format("2006-01-02")
	since, _ := h.WorkflowRepo.LastSuccessfulRunStart(ctx, wf.ID)

	h.runWorkflowGraph(wf, runID, steps, release, date, since)
	return runID
}

func (h *WorkflowAdmin) recordResourcePressureSkip(ctx context.Context, runID, wfID, trigger, now string, total int) {
	if err := h.WorkflowRepo.CreateWorkflowRun(ctx, runID, wfID, trigger, "ok", 0, total, now); err != nil {
		slog.Warn("workflow: insert run failed", "run", runID, "err", err)
	}
	if err := h.WorkflowRepo.SetRunSkipped(ctx, runID); err != nil {
		slog.Warn("workflow: set run skipped failed", "run", runID, "err", err)
	}
	if err := h.WorkflowRepo.UpdateWorkflowRunStatus(ctx, runID, "ok", now, "resource_pressure", "[]", 0); err != nil {
		slog.Warn("workflow: update run status failed", "run", runID, "err", err)
	}
	metrics.RecordWorkflowRunSkipped(ctx, "resource_pressure")
}

// runWorkflowGraph 后台执行状态图并收尾（run/workflows 状态与统计）。新建运行与
// 续跑（ResumeWorkflowRun，ADR-0104 决策二）共用：续跑以同一 runID 作为
// StateGraphExecutor 的 parentTaskID，task_checkpoints 中 done 节点被复用而不重投任务。
func (h *WorkflowAdmin) runWorkflowGraph(wf *workflow, runID string, steps []workflowStep, release func(), date, since string) {
	total := len(steps)
	concurrent.SafeGo(context.Background(), "gateway.sysadmin.execute_workflow", func(context.Context) {
		if release != nil {
			defer release()
		}
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

		// （后注册先执行，保证 run 终态对外可见时条目已清）任何路径退出都要清掉本 run 的去重状态；失败运行只删除不 Commit，
		// 避免未成功投递的条目被记为已见。
		defer h.dedupStates.Delete(runID)

		if total == 0 {
			status = "error"
			errMsg = "workflow has no steps"
			return
		}
		if h.Blackboard == nil {
			status = "error"
			errMsg = "workflow: blackboard not configured, cannot execute"
			slog.Error("workflow: Blackboard is nil, cannot execute state graph", "workflow", wf.ID, "run", runID)
			return
		}

		spec := buildGraphSpec(wf.Type, wf.ID, runID, date, since, steps)
		executor := orchestrator.NewStateGraphExecutor(h.Blackboard)
		if err := executor.Execute(bgCtx, runID, spec); err != nil {
			status = "error"
			errMsg = err.Error()
			slog.Warn("workflow: state graph execution failed", "workflow", wf.ID, "run", runID, "err", err)
			return
		}

		if failCount, firstErr := h.scanStepOutputErrors(context.Background(), runID); failCount > 0 {
			status = "error"
			errMsg = fmt.Sprintf("%d step(s) failed: %s", failCount, firstErr)
			return
		}

		// 1. 终端步骤全为 skipped 判定与结果投递
		terminalSteps := findTerminalSteps(wf.Type, steps)
		allTerminalSkipped := h.isAllTerminalSkipped(context.Background(), runID, terminalSteps)
		// 成功路径先取出用于 Commit；失败/提前 return 由上方 defer 兜底删除，防止泄漏。
		dedupVal, hasDedup := h.dedupStates.LoadAndDelete(runID)

		h.handleWorkflowDeliveryAndCleanup(context.Background(), wf, runID, steps, terminalSteps, allTerminalSkipped, dedupVal, hasDedup)

		// 2. 运行数据与去重记录清理
		keepRuns, seenTTLDays := pipelineRetentionFromConfig()
		if err := h.WorkflowRepo.PruneRunData(context.Background(), wf.ID, keepRuns); err != nil {
			slog.Debug("workflow: prune run data failed", "workflow_id", wf.ID, "err", err)
		}
		ttlCutoff := time.Now().AddDate(0, 0, -seenTTLDays).UTC().Format(time.RFC3339)
		if err := h.WorkflowRepo.PruneSeen(context.Background(), ttlCutoff); err != nil {
			slog.Debug("workflow: prune seen failed", "err", err)
		}
	})
}

func (h *WorkflowAdmin) isAllTerminalSkipped(ctx context.Context, runID string, terminalSteps []workflowStep) bool {
	for _, ts := range terminalSteps {
		if h.getStepStatus(ctx, runID, ts) != "skipped" {
			return false
		}
	}
	return true
}

func (h *WorkflowAdmin) handleWorkflowDeliveryAndCleanup(ctx context.Context, wf *workflow, runID string, steps, terminalSteps []workflowStep, allTerminalSkipped bool, dedupVal any, hasDedup bool) {
	if allTerminalSkipped && len(terminalSteps) > 0 {
		if err := h.WorkflowRepo.SetRunSkipped(ctx, runID); err != nil {
			slog.Warn("workflow: set run skipped failed", "run", runID, "err", err)
		}
		metrics.RecordWorkflowRunSkipped(ctx, "empty")
		return
	}

	h.deliverWorkflowResult(ctx, wf, runID, steps)

	if hasDedup && dedupVal != nil {
		if err := dedupVal.(*dedupState).Commit(ctx); err != nil {
			slog.Warn("workflow: commit dedup seen failed", "workflow", wf.ID, "run", runID, "err", err)
		}
	}
}

func (h *WorkflowAdmin) deliverWorkflowResult(ctx context.Context, wf *workflow, runID string, steps []workflowStep) {
	if h.Deliverer == nil || wf.ResultAction == "" || wf.ResultAction == "silent" || wf.ResultAction == "none" {
		return
	}
	deliverText := h.findDeliverText(ctx, runID, wf.DeliverFrom, steps)
	if err := h.Deliverer.DeliverResult(ctx, wf.ResultAction, "", wf.Name, deliverText); err != nil {
		slog.Warn("workflow: deliver result failed", "workflow", wf.ID, "run", runID, "err", err)
	}
}

// findTerminalSteps 找出没有下游后继的终端步骤。
func findTerminalSteps(wfType string, steps []workflowStep) []workflowStep {
	if len(steps) == 0 {
		return nil
	}
	if wfType != "dag" {
		return []workflowStep{steps[len(steps)-1]}
	}
	depSet := make(map[int]bool)
	for _, st := range steps {
		for _, d := range st.DependsOn {
			if idx, err := strconv.Atoi(d); err == nil && idx >= 0 && idx < len(steps) {
				depSet[idx] = true
			}
		}
	}
	var terminal []workflowStep
	for i, st := range steps {
		if !depSet[i] {
			terminal = append(terminal, st)
		}
	}
	if len(terminal) == 0 {
		return []workflowStep{steps[len(steps)-1]}
	}
	return terminal
}

// getStepStatus 获取步骤执行状态（优先查 run_data，回退查 step_outputs）。
func (h *WorkflowAdmin) getStepStatus(ctx context.Context, runID string, step workflowStep) string {
	stepKey := step.StepKey
	if stepKey == "" {
		stepKey = fmt.Sprintf("step_%d", step.Seq)
	}
	if st, _, err := h.WorkflowRepo.GetRunData(ctx, runID, stepKey); err == nil && st != "" {
		return st
	}
	outputs := h.loadRunStepOutputs(ctx, runID)
	for _, o := range outputs {
		if o.Seq == step.Seq {
			return o.Status
		}
	}
	return ""
}

// loadRunStepOutputs 读取当前 run 的步骤输出列表。
func (h *WorkflowAdmin) loadRunStepOutputs(ctx context.Context, runID string) []stepOutput {
	var raw []byte
	if err := h.DB.QueryRowContext(ctx, `SELECT step_outputs FROM workflow_runs WHERE id=?`, runID).Scan(&raw); err != nil {
		return nil
	}
	var outputs []stepOutput
	if err := json.Unmarshal(raw, &outputs); err != nil {
		return nil
	}
	return outputs
}

func (h *WorkflowAdmin) loadStepText(ctx context.Context, runID, stepKey string) string {
	status, dataJSON, err := h.WorkflowRepo.GetRunData(ctx, runID, stepKey)
	if err != nil || len(dataJSON) == 0 {
		return ""
	}
	if status != "" && status != "ok" {
		return ""
	}
	var data pipeline.StepData
	if err := json.Unmarshal(dataJSON, &data); err == nil && data.Text != "" {
		return data.Text
	}
	return string(dataJSON)
}

// findDeliverText 寻找待投递的文本结果。
func (h *WorkflowAdmin) findDeliverText(ctx context.Context, runID, deliverFrom string, steps []workflowStep) string {
	if deliverFrom != "" {
		if text := h.loadStepText(ctx, runID, deliverFrom); text != "" {
			return text
		}
	}
	for i := len(steps) - 1; i >= 0; i-- {
		st := steps[i]
		if st.Kind != "llm" && st.Kind != "agent" && st.Kind != "" {
			continue
		}
		stepKey := st.StepKey
		if stepKey == "" {
			stepKey = fmt.Sprintf("step_%d", st.Seq)
		}
		if text := h.loadStepText(ctx, runID, stepKey); text != "" {
			return text
		}
	}
	return ""
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
// [A-03 Step5] 原内联 EnsureSession/SaveMessage(user)/AcquireHeadless/
// SaveMessage(assistant)/SampleAndScoreReply/UpdateSessionTitle 六步序列，与
// cronadmin.executeAutomation、channelsadmin.dispatchChannelMessage 的对应
// 序列几乎相同又不完全一致（本函数此前从不触发 UserPromptSubmit（原 message.before）hook 拦截、也
// 不 TouchSession），收敛至 session.Orchestrator.RunTurn(Headless:true) 统一
// 实现，见 internal/gateway/session/orchestrator_headless.go 顶部注释。
func (h *WorkflowAdmin) runWorkflowStep(ctx context.Context, sessionID, prompt, workingDir, reasoningEffort, name string) (string, error) {
	// 未注入时返回步骤错误走正常失败路径，而非 nil panic 中断整条工作流。
	if h.SessionOrch == nil {
		return "", apperr.New(apperr.CodeInternal, "workflow step: session orchestrator not configured")
	}
	req := session.Request{
		SessionID:       sessionID,
		Input:           prompt,
		Channel:         "workflow",
		Headless:        true,
		WorkingDir:      workingDir,
		TitleHint:       name,
		ReasoningEffort: reasoningEffort,
		LoopCompletion:  "explicit",
	}
	res, err := h.SessionOrch.RunTurn(ctx, req, session.NewBufferSink())
	if err != nil {
		// [2026-08-02 S-06 抽样复查] RunTurn 内部 resolveSessionID 对非法
		// session_id 返回 CodeInvalidInput，此前恒被 CodeInternal 覆盖。
		return "", apperr.Wrap(apperr.CodeOf(err), "session.RunTurn(workflow step) failed", err)
	}
	if res.Aborted {
		return "", apperr.New(apperr.CodeInternal, "workflow step blocked (hook rejection or session error)")
	}
	return res.Reply, nil
}
