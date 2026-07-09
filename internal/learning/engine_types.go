package learning

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/prompt/optimizer"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// EvolutionLevel L0~L4 对应自我演化的五个层级，L4 需多签名审批。

// EvolutionLevel defines the scope of automated change. Higher = more gates, deeper rollback chain.
type EvolutionLevel int

const (
	L0ConfigAdjust       EvolutionLevel = iota // routing weights, timeout thresholds — auto
	L1PromptHeuristic                          // system prompt, routing criteria — auto
	L2SkillGeneration                          // Logic Collapse → new skill — semi-auto
	L3StrategyModify                           // agent behavior policy, LoRA adapter — approval req'd
	L4SourceArchitecture                       // system source code — multi-sig
)

// EvolutionGate verifies that a change at the given level passes all required checks.
type EvolutionGate interface {
	Approve(ctx context.Context, level EvolutionLevel, change *Change) error
}

// SimpleEvolutionGate 默认实现：L0/L1 自动批准，L2 记录日志，L3/L4 拒绝（需多签名）。
// 生产环境应替换为支持多签名审批的实现。
type SimpleEvolutionGate struct {
	// 可扩展：注入 HITL gateway、审批人列表等
}

func (g *SimpleEvolutionGate) Approve(ctx context.Context, level EvolutionLevel, change *Change) error {
	switch {
	case level <= L1PromptHeuristic:
		// L0/L1 自动批准
		slog.Info("evolution gate: auto-approved", "level", level, "desc", change.Description)
		return nil
	case level == L2SkillGeneration:
		// L2 半自动：记录日志并批准（未来可接 HITL）
		slog.Warn("evolution gate: L2 skill generation approved (semi-auto)", "desc", change.Description)
		return nil
	default:
		// L3/L4 拒绝（多签名未实现）
		return apperr.New(apperr.CodeForbidden,
			fmt.Sprintf("evolution gate: L%d change requires multi-signature approval (not yet implemented): %s", level, change.Description))
	}
}

type Change struct {
	Level       EvolutionLevel `json:"level"`
	Description string         `json:"description"`
	Patch       []byte         `json:"patch,omitempty"`
	Trajectory  []byte         `json:"trajectory,omitempty"`
	Signature   string         `json:"signature,omitempty"`
}

// FailureClass distinguishes uncontrollable infrastructure failures from logic errors.
// Values must match types.FailureClass: "logic", "controllable", "uncontrollable".
type FailureClass string

const (
	FailureLogic          FailureClass = "logic"          // incorrect reasoning, bad plan, skill error
	FailureControllable   FailureClass = "controllable"   // timeout, resource exhausted
	FailureUncontrollable FailureClass = "uncontrollable" // network offline, provider down, quota
)

// MEMF (Fallacy Memory Pool) stores failed trajectories for pruning.
type MEMF interface {
	Record(ctx context.Context, trajectory *FailureTrajectory) error
	Query(ctx context.Context, embedding []float64, threshold float64) ([]FailureTrajectory, error)
}

type FailureTrajectory struct {
	ID           string       `json:"id"`
	TaskType     string       `json:"task_type"`
	Embedding    []float64    `json:"embedding"`
	Error        string       `json:"error"`
	FailureClass FailureClass `json:"failure_class"`
	NodeQuality  float64      `json:"node_quality_score"`
}

// AutoCurriculum finds edge tasks during idle periods (Voyager style).
type AutoCurriculum interface {
	FindEdgeTask(ctx context.Context) (*Task, error)
	Execute(ctx context.Context, task *Task) (*TaskResult, error)
}

type Task struct {
	ID          string    `json:"id"`
	Description string    `json:"description"`
	Type        string    `json:"type"`
	Embedding   []float64 `json:"embedding,omitempty"`
	Difficulty  float64   `json:"difficulty"`
}

type TaskResult struct {
	TaskID       string       `json:"task_id"`
	Success      bool         `json:"success"`
	FailureClass FailureClass `json:"failure_class,omitempty"`
	Output       []byte       `json:"output,omitempty"`
}

// IsUncontrollable returns true if the failure was due to infrastructure faults.
func (r *TaskResult) IsUncontrollable() bool {
	return !r.Success && r.FailureClass == FailureUncontrollable
}

// =============================================================================
// PromptOptimizerAdapter — M9 内环通过此接口更新 optimizer.PromptOptimizer 状态。
// 由 pkg/swarm.optimizer.PromptOptimizer 实现；self_improve 包不直接引用 swarm 包
// （防止循环依赖：swarm → self_improve，self_improve 不可反向引用）。
// =============================================================================

// PromptOptimizerAdapter M9 内环与外部 optimizer.PromptOptimizer 的解耦接口。
type PromptOptimizerAdapter interface {
	// AddAvoidRule 将 Reflexion 生成的规避规则写入 optimizer.ErrorPatternMemory。
	AddAvoidRule(taskType, rule string)
}

// VersionStoreAdapter M9 外环与外部 optimizer.PromptVersionStore 的解耦接口。
type VersionStoreAdapter interface {
	// UpdateScore 更新候选版本的 Eval 评分。
	UpdateScore(ctx context.Context, id string, score float64) error
	// Activate 当候选评分超过基线时激活版本（原子 CAS）。
	Activate(ctx context.Context, taskType, id string, baselineScore float64) error
}

// HeuristicsWriter M9 内环写入成功轨迹的接口（P1-4）。
// 由 pkg/swarm.optimizer.HeuristicsMemory 实现；self_improve 包通过此接口解耦，
// 避免 swarm → self_improve → swarm 循环引用。
type HeuristicsWriter interface {
	// RecordSuccess 将成功任务的 taskType 写入 optimizer.HeuristicsMemory，更新 success_rate。
	// heuristicID 为空时由实现方自行生成（以 taskID 作为种子）。
	RecordSuccess(taskID, taskType string)
}

// StagingPipelineAdapter L3/L4 审批通过后提交候选版本的解耦接口。
// 由 pkg/swarm.optimizer.StagingPipeline 实现；self_improve 包通过接口解耦，防循环引用。
// 注：optimizer.AgentVersionSnapshot 定义在 rollout.go，此接口签名与 optimizer.StagingPipeline.SubmitCandidate 对齐。
type StagingPipelineAdapter interface {
	SubmitCandidate(ctx context.Context, snap *optimizer.AgentVersionSnapshot) error
}

// =============================================================================
// Engine — M9 Self-Improvement Engine 主入口
// 架构文档: docs/arch/M09-Self-Improvement-Engine.md §2
//
// 三环架构:
//   内环（实时/小时）: 订阅任务完成事件 → ReflexionEngine → MEMF + optimizer.HeuristicsMemory
//                      + 订阅 HeuristicGeneratedEvent → 更新 optimizer.PromptOptimizer.optimizer.ErrorPatternMemory
//   中环（日/周）:    2min ticker → AutoCurriculumGenerator
//   外环（周/月）:    订阅版本变更 → optimizer.ProgressiveRollout 门控推进
//                      + 订阅 EvalCompletedEvent → 更新 prompt_versions.score → 触发 Rollout
// =============================================================================

// TaskCompleteEvent 任务完成事件（由 Blackboard 事件总线推送）。
type TaskCompleteEvent struct {
	Seq      int64
	TaskID   string
	TaskType string
	Success  bool
	Failure  FailureClass
	Output   []byte
}

// VersionChangeEvent 版本变更事件（触发外环 Rollout 检查）。
type VersionChangeEvent struct {
	Seq              int64
	CandidateVersion string
	Stats            RolloutStats
}

// RolloutStats 外环统计数据（与 pkg/swarm 包的 RolloutStats 保持对齐）。
type RolloutStats struct {
	ErrorRate            float64
	BaselineErrorRate    float64
	P95Latency           float64
	BaselineP95Latency   float64
	SafetyViolations     int
	SurpriseIndexDegrade bool
}

// EngineConfig Engine 配置。
type EngineConfig struct {
	// InnerLoopInterval 内环轮询间隔（订阅模式时忽略）
	InnerLoopInterval time.Duration
	// MidLoopInterval 中环课程生成轮询间隔（默认 2min）
	MidLoopInterval time.Duration
	// MaxConcurrentReflections 并发反思上限（防止在高负载时积压）
	MaxConcurrentReflections int
	// BaselinePassRate Eval 基线通过率，低于此值触发 optimizer.PromptOptimizer（默认 0.8）
	BaselinePassRate float64
	// L3CheckInterval L3 策略漂移检测周期（默认 10min）
	L3CheckInterval time.Duration
}

// DefaultEngineConfig 返回默认配置。
func DefaultEngineConfig() *EngineConfig {
	return &EngineConfig{
		InnerLoopInterval:        0, // 事件驱动，不使用 ticker
		MidLoopInterval:          2 * time.Minute,
		MaxConcurrentReflections: 3,
		BaselinePassRate:         0.8,
		L3CheckInterval:          10 * time.Minute,
	}
}

// Reflector 内环反思接口（由 pkg/swarm.ReflexionEngine 实现，通过接口解耦）。
type Reflector interface {
	Reflect(ctx context.Context, taskID, taskType string, result *TaskResult, trajectory []Step, replanCount int) (*Reflection, error)
}

// Reflection 反思结果（镜像 swarm.Reflection 以避免循环引用）。
type Reflection struct {
	TaskID             string `json:"task_id"`
	Cause              string `json:"cause"`
	Counterfactual     string `json:"counterfactual"`
	GeneratedHeuristic string `json:"generated_heuristic"`
	MEMFRecordID       string `json:"memf_record_id,omitempty"`
	CreatedAt          int64  `json:"created_at"`
}

// Step 任务轨迹步骤（镜像 swarm.Step）。
type Step struct {
	Index     int    `json:"index"`
	Action    string `json:"action"`
	Reasoning string `json:"reasoning"`
	Result    string `json:"result"`
	Success   bool   `json:"success"`
}

// CurriculumGenerator 中环课程生成接口。
type CurriculumGenerator interface {
	Generate(ctx context.Context, surpriseIndex float64) error
}

// RolloutAdvancer 外环门控推进接口。
type RolloutAdvancer interface {
	AdvanceGate(ctx context.Context, version string, stats RolloutStats) error
}
