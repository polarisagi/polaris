package reflexion

import (
	"github.com/polarisagi/polaris/internal/learning"

	"context"
	"fmt"
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
		return nil, fmt.Errorf("ReflexionBridge.Reflect: %w", err)
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

// RolloutBridge 将 *optimizer.ProgressiveRollout 适配为 learning.learning.RolloutAdvancer。
// AdvanceGate 检查硬停止条件，触发时返回错误阻止推进。
type RolloutBridge struct {
	rollout *optimizer.ProgressiveRollout
}

func NewRolloutBridge(r *optimizer.ProgressiveRollout) *RolloutBridge {
	return &RolloutBridge{rollout: r}
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
	if b.rollout.CheckHardStop(swarmStats) {
		slog.Warn("swarm: rollout hard stop triggered", "version", version,
			"error_rate", stats.ErrorRate, "safety_violations", stats.SafetyViolations, "err", apperr.New(apperr.CodeInternal, "log event"))
		return apperr.New(apperr.CodeInternal, "rollout hard stop: safety or metrics regression")
	}
	slog.Info("swarm: rollout gate advanced", "version", version)
	return nil
}
