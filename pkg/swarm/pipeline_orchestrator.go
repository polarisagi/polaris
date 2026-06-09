package swarm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
)

// PipelineOrchestrator 按 PipelineDescriptor 顺序调度专家 Agent 流水线。
//
// 设计约束（Context Isolation Contract）：
//   - 编排器不做领域推理，仅加载 context、派生任务、收集结果、推进阶段。
//   - 每个阶段产出的 Result 作为下一阶段的 ContextPayload 精确传递，
//     不依赖全局记忆检索，确保每个 Agent 只看到它在当前角色中需要的信息。
//   - 当 VerificationPolicy.Adversarial == true 时，在意图字段注入对抗性前置假设，
//     强制验证 Agent 从"目标未达成"出发做目标反向分析。
//
// 架构文档: docs/arch/M08-Multi-Agent-Orchestrator-深度选型.md §5
type PipelineOrchestrator struct {
	blackboard   protocol.Blackboard
	pollInterval time.Duration
}

// NewPipelineOrchestrator 创建流水线编排器。
// pollInterval 控制阶段完成轮询频率（建议 200ms~1s）。
func NewPipelineOrchestrator(bb protocol.Blackboard, pollInterval time.Duration) *PipelineOrchestrator {
	if pollInterval <= 0 {
		pollInterval = 500 * time.Millisecond
	}
	return &PipelineOrchestrator{
		blackboard:   bb,
		pollInterval: pollInterval,
	}
}

// Run 执行一条完整流水线，返回最终 VerificationResult。
// 若未配置 VerificationPolicy，返回 VerdictPass（无验证）。
// 调用方应检查 VerificationResult.Verdict == VerdictBlocker 并决定是否重规划。
func (po *PipelineOrchestrator) Run(ctx context.Context, desc protocol.PipelineDescriptor) (*protocol.VerificationResult, error) {
	if len(desc.Stages) == 0 {
		return nil, perrors.New(perrors.CodeInvalidInput, "pipeline: at least one stage required")
	}

	// 生成流水线实例 ID（若调用方未指定）
	pipelineID := desc.ID
	if pipelineID == "" {
		pipelineID = "pipe-" + uuid.NewString()
	}

	slog.Info("pipeline: starting", "pipeline_id", pipelineID, "goal", desc.Goal, "stages", len(desc.Stages))

	var prevPayload []byte // 上一阶段的结构化产出，作为下一阶段的 ContextPayload

	for i, stage := range desc.Stages {
		stagePayload, err := po.runStage(ctx, pipelineID, desc.Goal, stage, prevPayload, i, desc.MaxRetries)
		if err != nil {
			return nil, perrors.Wrap(perrors.CodeInternal,
				fmt.Sprintf("pipeline %s: stage %s failed", pipelineID, stage.Name), err)
		}
		prevPayload = stagePayload
		slog.Info("pipeline: stage complete", "pipeline_id", pipelineID, "stage", stage.Name,
			"payload_bytes", len(stagePayload))
	}

	// 追加对抗验证阶段（可选）
	if desc.VerificationPolicy != nil {
		return po.runVerification(ctx, pipelineID, desc.Goal, prevPayload, desc.VerificationPolicy)
	}

	return &protocol.VerificationResult{Verdict: protocol.VerdictPass, Summary: "no verification policy configured"}, nil
}

// runStage 执行流水线中的单个阶段，带重试逻辑。
// 返回该阶段 Agent 的 Result 字节（作为下一阶段的 ContextPayload）。
func (po *PipelineOrchestrator) runStage(
	ctx context.Context,
	pipelineID, goal string,
	stage protocol.PipelineStageSpec,
	contextPayload []byte,
	stageIdx, maxRetries int,
) ([]byte, error) {
	retries := maxRetries
	if retries < 0 {
		retries = 0
	}

	// 合并目标与前序上下文作为 Intent：Agent 可从 ContextPayload 读取结构化产出，
	// Intent 携带原始目标，确保 S_PERCEIVE 理解这是流水线上下文而非独立任务。
	intentPayload, _ := json.Marshal(map[string]any{
		"pipeline_id":    pipelineID,
		"pipeline_stage": stage.Name,
		"stage_index":    stageIdx,
		"goal":           goal,
		"has_context":    len(contextPayload) > 0,
	})

	for attempt := 0; attempt <= retries; attempt++ {
		taskID := fmt.Sprintf("%s-%s-%d", pipelineID, stage.Name, attempt)

		priority := stage.Priority
		if priority == 0 {
			priority = 1
		}

		task := &protocol.TaskEntry{
			ID:             taskID,
			Type:           stage.TaskType,
			Priority:       priority,
			Status:         protocol.TaskPending,
			Intent:         intentPayload,
			IntentTaint:    protocol.TaintMedium, // 流水线内部意图，中等置信度
			PipelineID:     pipelineID,
			PipelineStage:  stage.Name,
			ContextPayload: contextPayload,
			CreatedAt:      time.Now().UnixMilli(),
			UpdatedAt:      time.Now().UnixMilli(),
		}

		if stage.TimeoutSec > 0 {
			task.ExpiresAt = time.Now().Add(time.Duration(stage.TimeoutSec) * time.Second).UnixMilli()
		}

		if err := po.blackboard.PostTask(ctx, task); err != nil {
			if attempt == retries {
				return nil, perrors.Wrap(perrors.CodeInternal, "pipeline: post task failed", err)
			}
			slog.Warn("pipeline: post task failed, retrying", "task_id", taskID, "attempt", attempt, "err", err)
			time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
			continue
		}

		result, err := po.waitForCompletion(ctx, taskID, stage.TimeoutSec)
		if err != nil {
			if attempt == retries {
				return nil, err
			}
			slog.Warn("pipeline: stage failed, retrying", "stage", stage.Name, "attempt", attempt, "err", err)
			time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
			continue
		}

		return result, nil
	}

	return nil, perrors.New(perrors.CodeInternal, fmt.Sprintf("pipeline: stage %s exhausted retries", stage.Name))
}

// runVerification 执行对抗性验证阶段。
func (po *PipelineOrchestrator) runVerification(
	ctx context.Context,
	pipelineID, goal string,
	executionPayload []byte,
	policy *protocol.VerificationPolicy,
) (*protocol.VerificationResult, error) {
	cap := policy.Capability
	if cap == "" {
		cap = "verify"
	}

	// 构造对抗性意图：注入"假设目标未达成"的初始前置
	adversarialPrefix := ""
	if policy.Adversarial {
		adversarialPrefix = "[ADVERSARIAL_STANCE] Assume the goal has NOT been achieved. " +
			"Start from what SHOULD be true, verify what ACTUALLY exists. " +
			"SUMMARY claims are not evidence — only codebase artifacts are. "
	}

	verifyIntentPayload, _ := json.Marshal(map[string]any{
		"pipeline_id":        pipelineID,
		"pipeline_stage":     "verify",
		"goal":               goal,
		"adversarial_stance": policy.Adversarial,
		"instruction":        adversarialPrefix + "Verify that the goal has been achieved. Return a VerificationResult JSON.",
	})

	taskID := pipelineID + "-verify"
	task := &protocol.TaskEntry{
		ID:             taskID,
		Type:           cap,
		Priority:       1,
		Status:         protocol.TaskPending,
		Intent:         verifyIntentPayload,
		IntentTaint:    protocol.TaintMedium,
		PipelineID:     pipelineID,
		PipelineStage:  "verify",
		ContextPayload: executionPayload, // 执行阶段的完整产出
		CreatedAt:      time.Now().UnixMilli(),
		UpdatedAt:      time.Now().UnixMilli(),
	}

	if err := po.blackboard.PostTask(ctx, task); err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "pipeline: post verify task failed", err)
	}

	resultBytes, err := po.waitForCompletion(ctx, taskID, 0)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "pipeline: verify stage failed", err)
	}

	// 尝试将结果解析为 VerificationResult。
	// parse 失败视为 VerdictWarning（不阻断）：使用 bool 模式而非具名 error，
	// 避免 nilerr linter 误报（"error not nil but returns nil"）。
	var vr protocol.VerificationResult
	if parsed := json.Unmarshal(resultBytes, &vr) == nil; !parsed {
		// Agent 未产出结构化 JSON — 视为警告（不阻断），记录日志
		slog.Warn("pipeline: verifier output not parseable as VerificationResult, treating as WARNING",
			"pipeline_id", pipelineID, "raw", string(resultBytes))
		return &protocol.VerificationResult{
			Verdict:  protocol.VerdictWarning,
			Summary:  "verifier output unparseable: " + string(resultBytes),
			Findings: []protocol.VerificationFinding{{Verdict: protocol.VerdictWarning, Description: "unparseable verifier output"}},
		}, nil
	}

	if policy.BlockOnFail && vr.Verdict == protocol.VerdictBlocker {
		slog.Error("pipeline: verification BLOCKER — goal not achieved",
			"pipeline_id", pipelineID, "summary", vr.Summary)
	}

	return &vr, nil
}

// waitForCompletion 轮询黑板直到任务 Done/Failed 或 context 取消。
// timeoutSec <= 0 时跟随 ctx 超时。
func (po *PipelineOrchestrator) waitForCompletion(ctx context.Context, taskID string, timeoutSec int) ([]byte, error) {
	pollCtx := ctx
	var cancel context.CancelFunc
	if timeoutSec > 0 {
		pollCtx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
		defer cancel()
	}

	ticker := time.NewTicker(po.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-pollCtx.Done():
			return nil, perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("pipeline: wait for task %s timed out", taskID), pollCtx.Err())
		case <-ticker.C:
			snapshot, err := po.blackboard.PeekTask(pollCtx, taskID)
			if err != nil {
				// 黑板查询临时失败，继续轮询（网络抖动等）
				slog.Debug("pipeline: snapshot query failed, retrying", "task_id", taskID, "err", err)
				continue
			}

			switch snapshot.Status {
			case protocol.TaskDone:
				return snapshot.Result, nil
			case protocol.TaskFailed:
				return nil, perrors.New(perrors.CodeInternal,
					fmt.Sprintf("pipeline: task %s failed", taskID))
			}
			// TaskPending/TaskClaimed/TaskExecuting/TaskSuspended → 继续等待
		}
	}
}
