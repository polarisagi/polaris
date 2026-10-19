package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// buildLoopResumeSnapshot 序列化当前 Agentic Loop 执行期上下文快照（P1 / ADR-0104 / HE-6）。
func (a *Agent) buildLoopResumeSnapshot() (string, error) {
	a.sCtx.Mu.RLock()
	msgsCopy := make([]types.Message, len(a.sCtx.LoopMessages))
	copy(msgsCopy, a.sCtx.LoopMessages)
	snap := HandoffResumeContext{
		SchemaVersion:       HandoffResumeContextVersion,
		GlobalTaintLevel:    a.sCtx.GlobalTaintLevel,
		LoopRound:           a.sCtx.LoopRound,
		LoopMessages:        msgsCopy,
		LoopLastUsageTokens: a.sCtx.LoopLastUsageTokens,
		LoopLastUsageCost:   a.sCtx.LoopLastUsageCost,
		LoopLastWallSeconds: a.sCtx.LoopLastWallSeconds,
		SnapshotAt:          time.Now().Unix(),
	}
	a.sCtx.Mu.RUnlock()

	data, err := json.Marshal(snap)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "buildLoopResumeSnapshot: marshal failed", err)
	}
	if len(data) > handoffSnapshotMaxBytes {
		metrics.RecordAgentHandoffSnapshotOversized(context.Background())
		return "", apperr.New(apperr.CodeResourceExhausted,
			"buildLoopResumeSnapshot: snapshot exceeds handoffSnapshotMaxBytes")
	}
	return string(data), nil
}

// persistLoopCheckpoint 落盘 agentic 循环每轮增量快照（ADR-0104 / HE-6 / P1）。
// 每轮结束（LLM 响应已追加、工具结果已追加）时调用。
func (a *Agent) persistLoopCheckpoint(ctx context.Context) {
	if a.taskCheckpointRepo == nil || a.sCtx == nil {
		return
	}
	resumeJSON, err := a.buildLoopResumeSnapshot()
	if err != nil {
		slog.ErrorContext(ctx, "agentic_loop: build checkpoint snapshot failed",
			"session_id", a.sCtx.SessionID, "err", err)
		return
	}
	a.sCtx.Mu.RLock()
	round := a.sCtx.LoopRound
	sessionID := a.sCtx.SessionID
	globalTaint := a.sCtx.GlobalTaintLevel
	a.sCtx.Mu.RUnlock()

	nodeID := fmt.Sprintf("loop_round_%d", round)
	idemKey := string(types.BuildIdempotencyKey("loop", "checkpoint", sessionID, nodeID, 1))
	cpErr := a.taskCheckpointRepo.UpsertCheckpoint(ctx, types.TaskCheckpointRow{
		TaskID:         sessionID,
		NodeID:         nodeID,
		Attempt:        1,
		Status:         "running",
		Reason:         "loop_step_done",
		IdempotencyKey: idemKey,
		TaintLevel:     globalTaint,
		ResumeCtxJSON:  resumeJSON,
		StartedAt:      time.Now().Unix(),
	})
	if cpErr != nil {
		slog.ErrorContext(ctx, "agentic_loop: persist checkpoint failed",
			"session_id", sessionID, "err", cpErr)
	}
}

// markLoopCheckpointDone 标记 agentic 循环快照已完成（P1 / ADR-0104）。
func (a *Agent) markLoopCheckpointDone(ctx context.Context) {
	if a.taskCheckpointRepo == nil || a.sCtx == nil {
		return
	}
	a.sCtx.Mu.RLock()
	round := a.sCtx.LoopRound
	sessionID := a.sCtx.SessionID
	globalTaint := a.sCtx.GlobalTaintLevel
	a.sCtx.Mu.RUnlock()

	nodeID := fmt.Sprintf("loop_round_%d", round)
	if err := a.taskCheckpointRepo.UpsertCheckpoint(ctx, types.TaskCheckpointRow{
		TaskID:      sessionID,
		NodeID:      nodeID,
		Attempt:     1,
		Status:      "done",
		Reason:      "loop_done",
		TaintLevel:  globalTaint,
		CompletedAt: time.Now().Unix(),
	}); err != nil {
		slog.WarnContext(ctx, "agentic_loop: mark checkpoint done failed", "err", err)
	}
}

// ResumeAgenticLoop 在进程重启后根据快照续跑未完成的 agentic 回合（P1 / ADR-0104 / HE-6）。
// 将 Agent 就位到 S_LOOP 态，回填 LoopMessages、LoopRound、污点等级与累计计量，
// 已完成的工具结果在 LoopMessages 中保留，不会被重复执行。
func (a *Agent) ResumeAgenticLoop(resumeCtxJSON string) (restored bool) {
	if resumeCtxJSON == "" {
		return false
	}
	var snap HandoffResumeContext
	if err := json.Unmarshal([]byte(resumeCtxJSON), &snap); err != nil {
		slog.Warn("agent: ResumeAgenticLoop snapshot unmarshal failed",
			"session_id", a.sCtx.SessionID, "err", err)
		return false
	}
	if snap.SchemaVersion != HandoffResumeContextVersion {
		slog.Warn("agent: ResumeAgenticLoop schema version mismatch",
			"session_id", a.sCtx.SessionID, "got", snap.SchemaVersion, "want", HandoffResumeContextVersion)
		return false
	}
	if snap.LoopRound <= 0 || len(snap.LoopMessages) == 0 {
		return false
	}

	a.sCtx.Mu.Lock()
	a.sCtx.LoopRound = snap.LoopRound
	a.sCtx.LoopMessages = snap.LoopMessages
	if snap.GlobalTaintLevel > a.sCtx.GlobalTaintLevel {
		a.sCtx.GlobalTaintLevel = snap.GlobalTaintLevel
	}
	a.sCtx.LoopLastUsageTokens = snap.LoopLastUsageTokens
	a.sCtx.LoopLastUsageCost = snap.LoopLastUsageCost
	a.sCtx.LoopLastWallSeconds = snap.LoopLastWallSeconds
	a.sCtx.Mu.Unlock()

	a.sm.ForceState(types.AgentStateLoop)
	a.asyncIntent(types.TriggerResume)
	return true
}

// ResumeAgenticLoopFromCheckpoint 从 TaskCheckpointRepository 查找最近的 running 快照并续跑。
func (a *Agent) ResumeAgenticLoopFromCheckpoint(ctx context.Context) (restored bool) {
	if a.taskCheckpointRepo == nil || a.sCtx == nil {
		return false
	}
	cps, err := a.taskCheckpointRepo.ListCheckpointsByTask(ctx, a.sCtx.SessionID)
	if err != nil || len(cps) == 0 {
		return false
	}
	// 找到最近一个处于 running 状态且带有 ResumeCtxJSON 的 agentic loop 快照
	var latest *types.TaskCheckpointRow
	var latestRound int
	for i := range cps {
		row := &cps[i]
		if row.Status == "running" && strings.HasPrefix(row.NodeID, "loop_round_") && row.ResumeCtxJSON != "" {
			var snap HandoffResumeContext
			if err := json.Unmarshal([]byte(row.ResumeCtxJSON), &snap); err == nil && snap.LoopRound > 0 {
				if latest == nil || snap.LoopRound > latestRound || (snap.LoopRound == latestRound && row.StartedAt > latest.StartedAt) {
					latest = row
					latestRound = snap.LoopRound
				}
			}
		}
	}
	if latest == nil {
		return false
	}
	return a.ResumeAgenticLoop(latest.ResumeCtxJSON)
}
