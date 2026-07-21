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

// handleEvalCompleted 处理 Eval 完成事件：更新评分，达标后提交 Staging 进入 Gate 2(Shadow)。
// 设计：score ≥ baselinePassRate × 1.05 且 !BlockDeploy → SubmitCandidate + RecordEvalScore。
//
// 注意：此处不再直接调用 versionStore.Activate。候选 Prompt 的真正激活推迟到
// ShadowExecutor 完成影子回放并调用 SQLiteRolloutStore.ConfirmShadow 之后，由
// ConfirmShadow 内部回调 promptActivator.Activate 触发（见 rollout_store.go）。
// 旧实现在此处 Eval 一过就同步 Activate，Gate 2/3 形同虚设，见 ADR-0029 §K。
func (e *Engine) handleEvalCompleted(ctx context.Context, ev types.EvalCompletedPayload) {
	if ev.CandidateID == "" || ev.BlockDeploy {
		return // 基线评测或安全否决，不提交
	}
	if e.versionStore == nil {
		return
	}
	// 更新候选版本的 Eval 分数（prompt_versions.score，Activate 时读取校验）
	if err := e.versionStore.UpdateScore(ctx, ev.CandidateID, ev.PassRate); err != nil {
		return
	}
	// 超过激活阈值（基线 × 1.05）才提交 Staging
	threshold := e.cfg.BaselinePassRate * 1.05
	if ev.PassRate < threshold {
		return
	}
	if e.stagingPipeline == nil {
		return
	}
	// 提交候选进入 Gate 1（Eval，随即被 SubmitCandidate 置为 Gate 2/Shadow 等待回放）。
	// taskType 暂从 suite 字段近似，实际应由上层传入；随 metadata 落盘供 ConfirmShadow 读回。
	if err := e.stagingPipeline.SubmitCandidate(ctx, &optimizer.AgentVersionSnapshot{
		Version:       ev.CandidateID,
		TaskType:      ev.Suite,
		BaselineScore: e.cfg.BaselinePassRate,
		CreatedAt:     time.Now().Unix(),
	}); err != nil {
		slog.Error("M9 staging submit failed", "candidate", ev.CandidateID, "err", err)
		return
	}
	if err := e.stagingPipeline.RecordEvalScore(ctx, ev.CandidateID, ev.PassRate, e.cfg.BaselinePassRate); err != nil {
		slog.Error("M9 staging record eval score failed", "candidate", ev.CandidateID, "err", err)
	}
}

func (e *Engine) TriggerCurriculum(ctx context.Context) error {
	if e.curriculum != nil {
		if err := e.curriculum.Generate(ctx, e.currentSurpriseIndex()); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "Engine.TriggerCurriculum", err)
		}
	}
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
