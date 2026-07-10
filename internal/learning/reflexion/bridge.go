package reflexion

import (
	"github.com/polarisagi/polaris/internal/learning"

	"context"
	"log/slog"

	"github.com/polarisagi/polaris/internal/learning/curriculum"
	"github.com/polarisagi/polaris/internal/prompt/optimizer"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// ReflexionBridge 将 *ReflexionEngine 适配为 learning.learning.Reflector。
// 负责 swarm 与 self_improve 包之间的类型转换，避免循环引用。
type ReflexionBridge struct {
	engine *ReflexionEngine
}

func NewReflexionBridge(e *ReflexionEngine) *ReflexionBridge {
	return &ReflexionBridge{engine: e}
}

var _ learning.Reflector = (*ReflexionBridge)(nil)

func (b *ReflexionBridge) Reflect(ctx context.Context, taskID, taskType string, result *learning.TaskResult, trajectory []learning.Step, replanCount int) (*learning.Reflection, error) {
	swarmResult := &learning.TaskResult{
		TaskID:       result.TaskID,
		Success:      result.Success,
		FailureClass: result.FailureClass,
		Output:       result.Output,
	}
	swarmSteps := make([]learning.Step, len(trajectory))
	for i, s := range trajectory {
		swarmSteps[i] = learning.Step{
			Index:     s.Index,
			Action:    s.Action,
			Reasoning: s.Reasoning,
			Result:    s.Result,
			Success:   s.Success,
		}
	}
	ref, err := b.engine.Reflect(ctx, taskID, taskType, swarmResult, swarmSteps, replanCount)
	if err != nil || ref == nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "ReflexionBridge.Reflect", err)
	}
	return &learning.Reflection{
		TaskID:             ref.TaskID,
		Cause:              ref.Cause,
		Counterfactual:     ref.Counterfactual,
		GeneratedHeuristic: ref.GeneratedHeuristic,
		MEMFRecordID:       ref.MEMFRecordID,
		CreatedAt:          ref.CreatedAt,
	}, nil
}

// CurriculumBridge 将 *learning.AutoCurriculumGenerator 适配为 learning.learning.CurriculumGenerator。
// 预绑定 Blackboard，将 M9 中环的接口签名统一为 Generate(ctx, surpriseIndex) error。
type CurriculumBridge struct {
	gen *curriculum.AutoCurriculumGenerator
	bb  protocol.Blackboard
}

func NewCurriculumBridge(gen *curriculum.AutoCurriculumGenerator, bb protocol.Blackboard) *CurriculumBridge {
	return &CurriculumBridge{gen: gen, bb: bb}
}

var _ learning.CurriculumGenerator = (*CurriculumBridge)(nil)

func (b *CurriculumBridge) Generate(ctx context.Context, surpriseIndex float64) error {
	samples := b.gen.Generate(ctx, b.bb, surpriseIndex)
	slog.Debug("swarm: curriculum generated", "samples", len(samples), "surprise_index", surpriseIndex)
	return nil
}

// RolloutBridge 将 *optimizer.SQLiteRolloutStore 适配为 learning.learning.RolloutAdvancer。
// 2026-07-10 前曾包装无 DB 持久化的 *optimizer.ProgressiveRollout，AdvanceGate 只做
// 内存硬停止检查、不持久化任何 Gate 状态，导致 M9 版本推进与 rollout_states 表完全脱节
// （ShadowExecutor/ConfirmShadow 读到的状态永远不会被这条路径更新）。现改为包装真实的
// SQLiteRolloutStore，AdvanceGate 落库，与 L3/L4 SubmitCandidate 共用同一份状态。
type RolloutBridge struct {
	store *optimizer.SQLiteRolloutStore
}

func NewRolloutBridge(s *optimizer.SQLiteRolloutStore) *RolloutBridge {
	return &RolloutBridge{store: s}
}

var _ learning.RolloutAdvancer = (*RolloutBridge)(nil)

func (b *RolloutBridge) AdvanceGate(ctx context.Context, version string, stats learning.RolloutStats) error {
	swarmStats := optimizer.RolloutStats{
		ErrorRate:            stats.ErrorRate,
		BaselineErrorRate:    stats.BaselineErrorRate,
		P95Latency:           stats.P95Latency,
		BaselineP95Latency:   stats.BaselineP95Latency,
		SafetyViolations:     stats.SafetyViolations,
		SurpriseIndexDegrade: stats.SurpriseIndexDegrade,
	}
	state, err := b.store.AdvanceGate(ctx, version, swarmStats)
	if err != nil {
		slog.Warn("swarm: rollout advance failed", "version", version, "err", err)
		return apperr.Wrap(apperr.CodeInternal, "RolloutBridge.AdvanceGate", err)
	}
	if state != nil && state.Status == optimizer.RolloutStatusRolledBack {
		slog.Warn("swarm: rollout hard stop triggered", "version", version,
			"error_rate", stats.ErrorRate, "safety_violations", stats.SafetyViolations)
		return apperr.New(apperr.CodeInternal, "rollout hard stop: safety or metrics regression")
	}
	slog.Info("swarm: rollout gate advanced", "version", version)
	return nil
}
