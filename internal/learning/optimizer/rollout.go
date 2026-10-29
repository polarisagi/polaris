package optimizer

import (
	"context"
	"math"

	"github.com/polarisagi/polaris/internal/config"
)

// M9 Staging 流水线发布骨架
// 架构文档: docs/arch/M09-Self-Improvement-Engine.md §2.3

type RolloutGate int

const (
	GateEvalRegression  RolloutGate = iota // Gate 1: Eval Harness 离线回归 (p<0.05)
	GateShadowExecution                    // Gate 2: Shadow Execution (影子执行) 强制隔离
	GateCanaryRollout                      // Gate 3: Canary Rollout (1%→10%→50%→100%，SSoT: state.yaml §m9 canary_steps)
	GateFullRollout                        // Gate 4: Full Rollout (保留7天 rollback)
)

type RolloutStatus string

const (
	RolloutStatusPending    RolloutStatus = "pending"
	RolloutStatusRunning    RolloutStatus = "running"
	RolloutStatusPaused     RolloutStatus = "paused"
	RolloutStatusRolledBack RolloutStatus = "rolled_back"
	RolloutStatusCommitted  RolloutStatus = "committed"
)

// RolloutState 存储在数据库中的流水线状态 (State-in-DB)。
type RolloutState struct {
	CandidateVersion string
	BaselineVersion  string
	CurrentGate      RolloutGate
	CanaryPercent    int // 1, 10, 50, 100（SSoT: state.yaml §m9 canary_steps）
	Status           RolloutStatus
	StartedAt        int64
	LastAdvancedAt   int64
}

// AgentVersionSnapshot 版本快照。
type AgentVersionSnapshot struct {
	Version         string
	ConfigRef       string
	PromptSetRef    string
	SkillSnapshotID string
	ModelID         string
	CreatedAt       int64

	// TaskType/BaselineScore：Gate 2(Shadow) 确认通过后用于回调
	// PromptVersionStore.Activate(ctx, TaskType, Version, BaselineScore)。
	// 随 SubmitCandidate 一并序列化进 rollout_states.metadata，ConfirmShadow 时读回。
	TaskType      string
	BaselineScore float64
}

// StagingPipeline 定义 M9 Staging 流水线行为。
// 实现该接口的存储层应通过事件或持久化队列来推进状态。
type StagingPipeline interface {
	SubmitCandidate(ctx context.Context, snapshot *AgentVersionSnapshot) error
	RecordEvalScore(ctx context.Context, version string, passRate float64, baselinePassRate float64) error
	ConfirmShadow(ctx context.Context, version string) error
	AdvanceGate(ctx context.Context, version string, stats RolloutStats) (*RolloutState, error)
	Rollback(ctx context.Context, version string, reason string) error
	GetState(ctx context.Context, version string) (*RolloutState, error)
	// ListPendingShadow 返回当前停留在 Gate 2(Shadow)、状态为 running 的候选版本号。
	// 供 ShadowExecutor 周期性触发器发现待回放的候选（M12 §8）。
	ListPendingShadow(ctx context.Context) ([]string, error)
}

// ProgressiveRollout 维护渐进式推进规则。
type ProgressiveRollout struct {
	canarySteps []int
}

// defaultCanarySteps 返回与 state.yaml §m9 canary_steps [0.01, 0.10, 0.50, 1.00] 一致。
func defaultCanarySteps() []int { return []int{1, 10, 50, 100} }

// NewProgressiveRollout 阶梯取自 config M9SelfImprove.CanarySteps（小数比例 → 百分比）；
// 配置缺失或非法（非递增 / 越界）时回落 defaultCanarySteps。
func NewProgressiveRollout() *ProgressiveRollout {
	steps := make([]int, 0, 4)
	prev := 0
	for _, f := range config.CurrentThresholds().M9SelfImprove.CanarySteps {
		pct := int(math.Round(f * 100))
		if pct <= prev || pct > 100 {
			steps = nil
			break
		}
		steps = append(steps, pct)
		prev = pct
	}
	if len(steps) < 2 || steps[len(steps)-1] != 100 {
		steps = defaultCanarySteps()
	}
	return &ProgressiveRollout{canarySteps: steps}
}

// CheckHardStop 检查是否触发硬停止条件。
// 硬停止条件: error>baseline×1.2 | P95 latency>baseline×1.4 | 安全违规>0 | SurpriseIndex退化
func (pr *ProgressiveRollout) CheckHardStop(stats RolloutStats) bool {
	if stats.ErrorRate > stats.BaselineErrorRate*1.2 {
		return true
	}
	if stats.P95Latency > stats.BaselineP95Latency*1.4 {
		return true
	}
	if stats.SafetyViolations > 0 {
		return true
	}
	if stats.SurpriseIndexDegrade {
		return true
	}
	return false
}

type RolloutStats struct {
	ErrorRate            float64
	BaselineErrorRate    float64
	P95Latency           float64
	BaselineP95Latency   float64
	SafetyViolations     int
	SurpriseIndexDegrade bool
}
