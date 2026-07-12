package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// compensate 倒序投递补偿任务（best-effort：日志记录失败，不阻断调用方错误路径）。
func (po *PipelineOrchestrator) compensate(
	ctx context.Context,
	pipelineID, goal string,
	completed []types.PipelineStageSpec,
	compensateMap map[string]string,
) {
	if len(compensateMap) == 0 {
		return
	}
	for i := len(completed) - 1; i >= 0; i-- {
		stage := completed[i]
		compType, ok := compensateMap[stage.Name]
		if !ok || compType == "" {
			continue
		}
		taskID := fmt.Sprintf("%s-%s-compensate", pipelineID, stage.Name)
		intentPayload, _ := json.Marshal(map[string]any{
			"pipeline_id":    pipelineID,
			"pipeline_stage": stage.Name,
			"goal":           goal,
			"compensating":   true,
		})
		task := &types.TaskEntry{
			ID:            taskID,
			Type:          compType,
			Priority:      1,
			Status:        types.TaskPending,
			Intent:        intentPayload,
			IntentTaint:   types.TaintMedium,
			PipelineID:    pipelineID,
			PipelineStage: stage.Name + "-compensate",
			CreatedAt:     time.Now().UnixMilli(),
			UpdatedAt:     time.Now().UnixMilli(),
		}
		if err := po.blackboard.PostTask(ctx, task); err != nil {
			slog.Warn("pipeline: compensate task post failed",
				"pipeline_id", pipelineID, "stage", stage.Name, "err", err)
		} else {
			slog.Info("pipeline: compensate task posted",
				"pipeline_id", pipelineID, "stage", stage.Name, "type", compType)

			// 启动独立监控 goroutine，追踪补偿任务结果 (FIX-009)
			concurrent.SafeGo(ctx, "swarm.pipeline_compensate_monitor", func(monCtx context.Context) {
				po.monitorCompensationTask(monCtx, taskID, stage.Name, pipelineID)
			})
		}
	}
}

// monitorCompensationTask 监控补偿任务的执行结果
func (po *PipelineOrchestrator) monitorCompensationTask(ctx context.Context, taskID, failedStage, pipelineID string) {
	ticker := time.NewTicker(po.compPoll)
	defer ticker.Stop()
	deadline := time.After(po.compTimeout)

	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			slog.Error("pipeline: compensation task timeout, manual intervention required",
				"task_id", taskID,
				"failed_stage", failedStage,
			)
			if metrics.InstrSwarmCompensationTimeoutTotal != nil {
				metrics.InstrSwarmCompensationTimeoutTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("stage", failedStage)))
			}
			po.escalateCompensationFailure(ctx, pipelineID, failedStage, taskID, "timeout")
			return
		case <-ticker.C:
			snapshot, err := po.blackboard.PeekTask(ctx, taskID)
			if err != nil {
				slog.Warn("pipeline: failed to peek compensation task status", "err", err)
				continue
			}
			switch snapshot.Status {
			case types.TaskDone:
				slog.Info("pipeline: compensation task completed", "task_id", taskID)
				return
			case types.TaskFailed:
				slog.Error("pipeline: compensation task itself failed, data may be inconsistent",
					"task_id", taskID,
					"failed_stage", failedStage,
				)
				if metrics.InstrSwarmCompensationFailedTotal != nil {
					metrics.InstrSwarmCompensationFailedTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("stage", failedStage)))
				}
				po.escalateCompensationFailure(ctx, pipelineID, failedStage, taskID, "failed")
				return
			}
			// 其他状态继续等待
		}
	}
}

func (po *PipelineOrchestrator) escalateCompensationFailure(ctx context.Context, pipelineID, failedStage, taskID, reason string) {
	if po.decisionLog != nil {
		decisionCtx, _ := json.Marshal(map[string]string{
			"pipeline_id":  pipelineID,
			"failed_stage": failedStage,
			"task_id":      taskID,
			"reason":       reason,
		})
		entry := &types.DecisionLogEntry{
			SessionID:    pipelineID,
			AgentID:      "orchestrator",
			DecisionType: "compensation_escalation",
			Context:      decisionCtx,
			Choice:       "ESCALATE",
			Reason:       fmt.Sprintf("compensation task %s", reason),
		}
		if err := po.decisionLog.AppendDecision(ctx, entry); err != nil {
			slog.Warn("pipeline: failed to append decision log", "err", err)
		}
	}

	if po.hitl != nil {
		promptText := fmt.Sprintf("Compensation task %s. PipelineID: %s, FailedStage: %s, TaskID: %s, Reason: %s", reason, pipelineID, failedStage, taskID, reason)
		prompt := types.HITLPrompt{
			ID:             "comp-esc-" + taskID,
			CheckpointType: "compensation_failed",
			PromptText:     promptText,
			RiskLevel:      3,
			DeadlineNs:     time.Now().Add(12 * time.Hour).UnixNano(),
		}
		_, err := po.hitl.Prompt(ctx, prompt)
		if err != nil {
			slog.Error("pipeline: failed to prompt HITL escalation", "err", err)
		}
	}
}
