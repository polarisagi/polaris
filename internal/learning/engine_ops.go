package learning

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/prompt/optimizer"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// detectL3Trigger 检测策略漂移并通过 HITL 请求人工审批。
// 触发条件：SurpriseIndex > 0.8（持续策略失效信号）。
// 审批通过后调用 stagingPipeline.SubmitCandidate 进入 Staging。
// 在 Run() 的 l3Ticker 中周期调用；hitlGateway 为 nil 时跳过。
func (e *Engine) detectL3Trigger(ctx context.Context) {
	if e.hitlGateway == nil || e.surpriseIndexFn == nil {
		return
	}
	si := e.surpriseIndexFn()
	if si <= 0.8 {
		return
	}
	slog.Warn("L3 strategy drift detected, requesting HITL approval", "surprise_index", si)
	concurrent.SafeGo(ctx, "learning-detect-l3", func(ctx context.Context) {
		resp, err := e.hitlGateway.Prompt(ctx, types.HITLPrompt{
			ID:             fmt.Sprintf("l3-%d", time.Now().UnixNano()),
			CheckpointType: "l3_strategy_modify",
			PromptText:     fmt.Sprintf("策略漂移检测（SurpriseIndex=%.3f > 0.8），请审批策略修改候选。", si),
			RiskLevel:      2,
			TaintLevel:     types.TaintMedium,
			DeadlineNs:     time.Now().Add(24 * time.Hour).UnixNano(),
		})
		if err != nil || resp == nil || !resp.Approved {
			slog.Warn("L3 strategy modify declined or timed out", "err", err)
			return
		}
		if e.stagingPipeline != nil {
			if err := e.stagingPipeline.SubmitCandidate(ctx, &optimizer.AgentVersionSnapshot{
				Version:   fmt.Sprintf("l3-%d", time.Now().Unix()),
				ConfigRef: fmt.Sprintf("strategy drift remediation: si=%.3f", si),
				CreatedAt: time.Now().Unix(),
			}); err != nil {
				slog.Error("L3 staging submit failed", "err", err)
			}
		}
	})
}

// detectL4Trigger 处理管理员主动触发的 L4 源码级架构变更审批。
// L4 不做自动检测，由外部通过 l4TriggerCh 推入 Change 信号触发。
// 需 HITL 审批（CheckpointType="l4_multi_sig"）通过后方可进入 Staging。
// 注：当前 HITLGateway 为单审批模型，多重签名需在 HITLGateway 扩展实现。
func (e *Engine) detectL4Trigger(ctx context.Context, change Change) {
	if e.hitlGateway == nil {
		slog.Warn("L4 trigger skipped: HITL gateway not configured")
		return
	}
	slog.Info("L4 source architecture change requested", "description", change.Description)
	concurrent.SafeGo(ctx, "learning-detect-l4", func(ctx context.Context) {
		resp, err := e.hitlGateway.Prompt(ctx, types.HITLPrompt{
			ID:             fmt.Sprintf("l4-%d", time.Now().UnixNano()),
			CheckpointType: "l4_multi_sig",
			PromptText:     fmt.Sprintf("L4 源码级架构变更申请：%s。请审批。", change.Description),
			RiskLevel:      3,
			TaintLevel:     types.TaintHigh,
			DeadlineNs:     time.Now().Add(72 * time.Hour).UnixNano(),
		})
		if err != nil || resp == nil || !resp.Approved {
			slog.Warn("L4 architecture change declined or timed out", "err", err)
			return
		}
		if e.stagingPipeline != nil {
			if err := e.stagingPipeline.SubmitCandidate(ctx, &optimizer.AgentVersionSnapshot{
				Version:   fmt.Sprintf("l4-%d", time.Now().Unix()),
				ConfigRef: change.Description,
				CreatedAt: time.Now().Unix(),
			}); err != nil {
				slog.Error("L4 staging submit failed", "err", err)
			}
		}
	})
}

// handleEvalCompleted 处理 Eval 完成事件：更新评分，若达到激活条件则触发 Rollout。
// 设计：score ≥ baselinePassRate × 1.05 且 !BlockDeploy → 激活候选版本 → AdvanceGate。
func (e *Engine) handleEvalCompleted(ctx context.Context, ev types.EvalCompletedPayload) {
	if ev.CandidateID == "" || ev.BlockDeploy {
		return // 基线评测或安全否决，不激活
	}
	if e.versionStore == nil {
		return
	}
	// 更新候选版本的 Eval 分数
	if err := e.versionStore.UpdateScore(ctx, ev.CandidateID, ev.PassRate); err != nil {
		return
	}
	// 超过激活阈值（基线 × 1.05）才触发激活与 Rollout
	threshold := e.cfg.BaselinePassRate * 1.05
	if ev.PassRate < threshold {
		return
	}
	// 激活候选（taskType 暂从 suite 字段近似，实际应由上层传入）
	if err := e.versionStore.Activate(ctx, ev.Suite, ev.CandidateID, e.cfg.BaselinePassRate); err != nil {
		return
	}
	// 通知外环推进 Rollout
	if e.rollout != nil {
		concurrent.SafeGo(ctx, "learning-rollout-advance", func(ctx context.Context) {
			_ = e.rollout.AdvanceGate(ctx, ev.CandidateID, RolloutStats{
				BaselineErrorRate:  1.0 - e.cfg.BaselinePassRate,
				ErrorRate:          1.0 - ev.PassRate,
				SafetyViolations:   ev.SafetyViolations,
				P95Latency:         ev.P95LatencyMs,
				BaselineP95Latency: ev.BaselineP95Ms,
			})
		})
	}
}

// 编译期验证 Engine 实现 LearningFacade（对外统一门面，P1-2）。
var _ LearningFacade = (*Engine)(nil)

// ReportOutcome 上报任务结果到内环事件通道（非阻塞：通道满时丢弃，后台尽力而为）。
func (e *Engine) ReportOutcome(_ context.Context, taskID string, result *TaskResult) error {
	if result == nil {
		return nil
	}
	ev := TaskCompleteEvent{
		Seq:      e.taskSeqCounter.Add(1),
		TaskID:   taskID,
		TaskType: "general", // 调用方暂无任务类型上下文，与 Agent 终态回调口径一致
		Success:  result.FailureClass == "",
		Failure:  result.FailureClass,
		Output:   result.Output,
	}
	select {
	case e.taskEvents <- ev:
	default:
	}
	return nil
}

func (e *Engine) SurpriseIndex() float64 {
	return e.currentSurpriseIndex()
}

func (e *Engine) TriggerCurriculum(ctx context.Context) error {
	if e.curriculum != nil {
		if err := e.curriculum.Generate(ctx, e.currentSurpriseIndex()); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "Engine.TriggerCurriculum", err)
		}
	}
	return nil
}

func (e *Engine) Stop(ctx context.Context) error {
	// Engine shuts down when ctx passed to Start is canceled
	return nil
}

func (e *Engine) loadCursors(ctx context.Context) map[string]int64 {
	cursors := make(map[string]int64)
	if e.db == nil {
		return cursors
	}
	rows, err := e.db.QueryContext(ctx, "SELECT stream_name, last_seq FROM learning_cursors")
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var name string
			var seq int64
			_ = rows.Scan(&name, &seq)
			cursors[name] = seq
		}
	} else {
		slog.Warn("failed to load learning cursors", "err", err)
	}
	return cursors
}

func (e *Engine) saveCursorAsync(ctx context.Context, stream string, seq int64) {
	if e.db == nil {
		return
	}
	concurrent.SafeGo(ctx, "learning-cursor-save", func(ctx context.Context) {
		now := time.Now().Unix()
		_, err := e.db.ExecContext(ctx, "INSERT INTO learning_cursors(stream_name, last_seq, updated_at) VALUES(?, ?, ?) ON CONFLICT(stream_name) DO UPDATE SET last_seq=excluded.last_seq, updated_at=excluded.updated_at", stream, seq, now)
		if err != nil {
			slog.Error("failed to save learning cursor", "stream", stream, "seq", seq, "err", err)
		}
	})
}
