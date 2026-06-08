package swarm

import "context"

// M9 Staging 流水线发布骨架
// 架构文档: docs/arch/M09-Self-Improvement-Engine.md §2.3

type RolloutGate int

const (
	GateEvalRegression  RolloutGate = iota // Gate 1: Eval Harness 离线回归 (p<0.05)
	GateShadowExecution                    // Gate 2: Shadow Execution (影子执行) 强制隔离
	GateCanaryRollout                      // Gate 3: Canary Rollout (1%→5%→25%→50%→100%)
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
	CanaryPercent    int // 1, 5, 25, 50, 100
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
}

type RolloutStats struct {
	ErrorRate            float64
	BaselineErrorRate    float64
	P95Latency           float64
	BaselineP95Latency   float64
	SafetyViolations     int
	SurpriseIndexDegrade bool
}

// StagingPipeline 定义 M9 Staging 流水线行为。
// 实现该接口的存储层应通过事件或持久化队列来推进状态。
type StagingPipeline interface {
	SubmitCandidate(ctx context.Context, snapshot *AgentVersionSnapshot) error
	AdvanceGate(ctx context.Context, version string, stats RolloutStats) (*RolloutState, error)
	Rollback(ctx context.Context, version string, reason string) error
	GetState(ctx context.Context, version string) (*RolloutState, error)
}

// ProgressiveRollout 维护渐进式推进规则。
type ProgressiveRollout struct {
	canarySteps []int
}

func NewProgressiveRollout() *ProgressiveRollout {
	return &ProgressiveRollout{
		canarySteps: []int{1, 5, 25, 50, 100},
	}
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
