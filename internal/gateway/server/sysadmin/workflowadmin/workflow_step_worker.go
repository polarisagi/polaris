// workflow_step_worker.go：workflow 步骤的自订阅 Blackboard 执行器
// （2026-07-12 workflow DAG 并行 + 失败重试接入）。
//
// 设计动机：M8 Orchestrator 的中心化按类型下推机制（RegisterWorker/
// dispatchPendingTasks）在生产环境从未被激活（orchestrator.NewWorker 全仓库
// 零调用），故不依赖它，改用 orchestrator.Worker.ListenLoop 已验证过的"自订阅
// Blackboard + CAS 认领"模式：订阅 task_posted 事件，按 CapabilityType 过滤，
// 只认领本包投递的 workflow_step 任务。
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

// workflowStepWorkerAgentID 是本 Worker 在 Blackboard claimed_by 列中的固定标识。
const workflowStepWorkerAgentID = "workflow_step_worker"

// stepNodeIntent 对应 StateGraphExecutor.tryPostNode 编码的 intentData 顶层结构
// （pattern_state_graph.go）：state_graph_node_id 为触发的节点 ID（=本包的 step
// ID），template 为 buildGraphSpec 写入的 stepIntentEnvelope JSON 字符串，
// upstream_output 为上游节点（若有）完成时写回的 payload 原始字节（字符串形态）。
type stepNodeIntent struct {
	StateGraphNodeID string `json:"state_graph_node_id"`
	Template         string `json:"template"`
	UpstreamOutput   string `json:"upstream_output"`
}

// stepCompletionPayload 是本 Worker 写回 Blackboard CompleteTask 的载荷形状。
// Status 字段供 buildGraphSpec 构造的失败重试自环条件边
// （Field="status",Op=eq,Value="error"）求值——业务失败（工具/LLM 报错）一律走
// CompleteTask 而非 FailTask，把重试判定完全交给声明式条件边，FailTask 保留给
// 基础设施级故障（Blackboard 断连、intent 解析失败等，见 tryClaimAndExecuteStep
// 的 fail-fast 分支）。
type stepCompletionPayload struct {
	Status string `json:"status"` // "ok" | "error"
	StepID string `json:"step_id"`
	// Reply 截断至 2000 字符，供下游节点 input_from_prev 注入（与旧顺序执行引擎
	// 截断长度一致，见 workflow_engine.go 原实现）。
	Reply     string `json:"reply,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Error     string `json:"error,omitempty"`
}

// RunStepWorkerLoop 自订阅 Blackboard 并执行本包投递的 workflow_step 任务。
// 应在 boot 阶段以 concurrent.SafeGo 启动为长驻 goroutine（见 cmd/polaris 接入）。
// ctx 取消时返回 ctx.Err()。
func (h *WorkflowAdmin) RunStepWorkerLoop(ctx context.Context) error {
	if h.Blackboard == nil {
		return apperr.New(apperr.CodeInternal, "workflow step worker: Blackboard 未注入")
	}
	events, err := h.Blackboard.Subscribe(ctx)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "workflow step worker: subscribe failed", err)
	}
	slog.Info("workflow step worker: started listening on blackboard")

	for {
		select {
		case <-ctx.Done():
			return apperr.Wrap(apperr.CodeInternal, "workflow step worker: context canceled", ctx.Err())
		case ev, ok := <-events:
			if !ok {
				return apperr.New(apperr.CodeInternal, "workflow step worker: blackboard event channel closed")
			}
			if ev.Type != "task_posted" {
				continue
			}
			taskID := ev.TaskID
			concurrent.SafeGo(ctx, "workflowadmin.step_worker.claim", func(ctx context.Context) {
				h.tryClaimAndExecuteStep(ctx, taskID)
			})
		}
	}
}

// tryClaimAndExecuteStep 校验任务属于本能力类型后 CAS 认领并执行；非本类型/已被
// 抢占/未处于 Pending 状态均静默跳过（与 orchestrator.Worker.tryClaimAndExecute
// 的"被抢占则无视"语义一致）。
func (h *WorkflowAdmin) tryClaimAndExecuteStep(ctx context.Context, taskID string) {
	snap, err := h.Blackboard.PeekTask(ctx, taskID)
	if err != nil || snap == nil {
		return
	}
	if snap.Type != workflowStepCapabilityType {
		return // 不属于本 Worker 的能力类型，让其他 Worker（如有）处理
	}
	if snap.Status != types.TaskPending {
		return
	}

	claimed, err := h.Blackboard.ClaimTask(ctx, taskID, workflowStepWorkerAgentID)
	if err != nil || !claimed {
		return
	}

	var nodeIntent stepNodeIntent
	if err := json.Unmarshal(snap.Intent, &nodeIntent); err != nil {
		h.failStepTaskInfra(ctx, taskID, fmt.Sprintf("parse task intent failed: %v", err))
		return
	}
	var envelope stepIntentEnvelope
	if err := json.Unmarshal([]byte(nodeIntent.Template), &envelope); err != nil {
		h.failStepTaskInfra(ctx, taskID, fmt.Sprintf("parse step envelope failed: %v", err))
		return
	}

	step, err := h.loadWorkflowStepByID(ctx, nodeIntent.StateGraphNodeID)
	if err != nil || step == nil {
		h.failStepTaskInfra(ctx, taskID, fmt.Sprintf("load workflow_step %s failed: %v", nodeIntent.StateGraphNodeID, err))
		return
	}

	h.executeStepTask(ctx, taskID, envelope, *step, nodeIntent.UpstreamOutput)
}

// failStepTaskInfra 处理基础设施级故障（intent 解析失败/步骤配置缺失等）：这类
// 故障不是"业务重试"能修复的 bug，走 FailTask 触发 StateGraphExecutor Fail-Fast
// 中止，而非静默吞掉或伪装成 status=error 让重试自环徒劳空转。
func (h *WorkflowAdmin) failStepTaskInfra(ctx context.Context, taskID, msg string) {
	slog.Error("workflow step worker: infra failure", "task_id", taskID, "err", msg)
	_ = h.Blackboard.FailTask(context.Background(), taskID, workflowStepWorkerAgentID, []byte(msg))
}

// executeStepTask 执行单个步骤：应用 automation 覆盖 + input_from_prev 注入 +
// 复用既有 runWorkflowStep，成功/业务失败均以 CompleteTask 写回（见
// stepCompletionPayload 文档注释）。
func (h *WorkflowAdmin) executeStepTask(ctx context.Context, taskID string, envelope stepIntentEnvelope, step workflowStep, upstreamOutput string) {
	bgCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	effectivePrompt := step.Prompt
	effectiveWorkingDir := step.WorkingDir
	effectiveEffort := step.ReasoningEffort
	if effectiveEffort == "" {
		effectiveEffort = "medium"
	}
	if step.AutomationID != "" {
		h.applyAutomationOverride(bgCtx, step.AutomationID, &effectivePrompt, &effectiveWorkingDir, &effectiveEffort)
	}

	if step.InputFromPrev && upstreamOutput != "" {
		if prefix := extractUpstreamReply(upstreamOutput); prefix != "" {
			effectivePrompt = "[前一步骤输出]\n" + prefix + "\n[/前一步骤输出]\n\n" + effectivePrompt
		}
	}

	sessionID := cronadmin.NewSessionID()
	stepName := stepLabel(step)

	reply, stepErr := h.runWorkflowStep(bgCtx, sessionID, effectivePrompt, effectiveWorkingDir, effectiveEffort, stepName)

	payload := stepCompletionPayload{StepID: step.ID, SessionID: sessionID}
	var recordStatus, recordPreview string
	if stepErr != nil {
		payload.Status = "error"
		payload.Error = stepErr.Error()
		recordStatus = "error"
		recordPreview = stepErr.Error()
		slog.Warn("workflow step worker: step failed", "step", step.ID, "err", stepErr)
	} else {
		payload.Status = "ok"
		payload.Reply = truncateRunes(reply, 2000)
		recordStatus = "ok"
		recordPreview = truncateRunes(reply, 500)
	}

	h.recordStepOutput(bgCtx, envelope.RunID, step, sessionID, recordStatus, recordPreview)

	payloadJSON, _ := json.Marshal(payload)
	if err := h.Blackboard.CompleteTask(context.Background(), taskID, workflowStepWorkerAgentID, payloadJSON); err != nil {
		slog.Warn("workflow step worker: CompleteTask failed", "task_id", taskID, "err", err)
	}
}

// recordStepOutput 落盘执行历史（workflow_runs.step_outputs/current_step），
// 原子追加/自增以兼容 DAG 并行下多个步骤并发完成（见 repo 层 json_insert 实现），
// 记账失败不影响任务结果（与 worker.go UpdateTaskTokens 同一原则）。
func (h *WorkflowAdmin) recordStepOutput(ctx context.Context, runID string, step workflowStep, sessionID, status, preview string) {
	so := stepOutput{Seq: step.Seq, SessionID: sessionID, Status: status, OutputPreview: preview}
	soJSON, _ := json.Marshal(so)
	if err := h.WorkflowRepo.AppendWorkflowRunStepOutput(ctx, runID, soJSON); err != nil {
		slog.Warn("workflow step worker: append step output failed", "run", runID, "step", step.ID, "err", err)
	}
	if err := h.WorkflowRepo.IncrementWorkflowRunCurrentStep(ctx, runID); err != nil {
		slog.Warn("workflow step worker: increment current_step failed", "run", runID, "err", err)
	}
}

// extractUpstreamReply 从上游节点的 stepCompletionPayload JSON 中提取 Reply 字段，
// 解析失败（如上游是非本包节点、payload 形状不同）时返回空串——input_from_prev
// 注入是增强能力，不是执行前置条件，静默降级优于中止整条工作流。
func extractUpstreamReply(upstreamOutput string) string {
	var p stepCompletionPayload
	if err := json.Unmarshal([]byte(upstreamOutput), &p); err != nil {
		return ""
	}
	return p.Reply
}

func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "\n…（已截断）"
}
