package self_improve

// EvolutionLevel L0~L4 对应自我演化的五个层级，L4 需多签名审批。

import (
	"context"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
)

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

type Change struct {
	Level       EvolutionLevel `json:"level"`
	Description string         `json:"description"`
	Patch       []byte         `json:"patch,omitempty"`
	Trajectory  []byte         `json:"trajectory,omitempty"`
	Signature   string         `json:"signature,omitempty"`
}

// FailureClass distinguishes uncontrollable infrastructure failures from logic errors.
// Values must match protocol.FailureClass: "logic", "controllable", "uncontrollable".
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
// PromptOptimizerAdapter — M9 内环通过此接口更新 PromptOptimizer 状态。
// 由 pkg/swarm.PromptOptimizer 实现；self_improve 包不直接引用 swarm 包
// （防止循环依赖：swarm → self_improve，self_improve 不可反向引用）。
// =============================================================================

// PromptOptimizerAdapter M9 内环与外部 PromptOptimizer 的解耦接口。
type PromptOptimizerAdapter interface {
	// AddAvoidRule 将 Reflexion 生成的规避规则写入 ErrorPatternMemory。
	AddAvoidRule(taskType, rule string)
}

// VersionStoreAdapter M9 外环与外部 PromptVersionStore 的解耦接口。
type VersionStoreAdapter interface {
	// UpdateScore 更新候选版本的 Eval 评分。
	UpdateScore(ctx context.Context, id string, score float64) error
	// Activate 当候选评分超过基线时激活版本（原子 CAS）。
	Activate(ctx context.Context, taskType, id string, baselineScore float64) error
}

// HeuristicsWriter M9 内环写入成功轨迹的接口（P1-4）。
// 由 pkg/swarm.HeuristicsMemory 实现；self_improve 包通过此接口解耦，
// 避免 swarm → self_improve → swarm 循环引用。
type HeuristicsWriter interface {
	// RecordSuccess 将成功任务的 taskType 写入 HeuristicsMemory，更新 success_rate。
	// heuristicID 为空时由实现方自行生成（以 taskID 作为种子）。
	RecordSuccess(taskID, taskType string)
}

// =============================================================================
// Engine — M9 Self-Improvement Engine 主入口
// 架构文档: docs/arch/M09-Self-Improvement-Engine.md §2
//
// 三环架构:
//   内环（实时/小时）: 订阅任务完成事件 → ReflexionEngine → MEMF + HeuristicsMemory
//                      + 订阅 HeuristicGeneratedEvent → 更新 PromptOptimizer.ErrorPatternMemory
//   中环（日/周）:    2min ticker → AutoCurriculumGenerator
//   外环（周/月）:    订阅版本变更 → ProgressiveRollout 门控推进
//                      + 订阅 EvalCompletedEvent → 更新 prompt_versions.score → 触发 Rollout
// =============================================================================

// TaskCompleteEvent 任务完成事件（由 Blackboard 事件总线推送）。
type TaskCompleteEvent struct {
	TaskID   string
	TaskType string
	Success  bool
	Failure  FailureClass
	Output   []byte
}

// VersionChangeEvent 版本变更事件（触发外环 Rollout 检查）。
type VersionChangeEvent struct {
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
	// BaselinePassRate Eval 基线通过率，低于此值触发 PromptOptimizer（默认 0.8）
	BaselinePassRate float64
}

// DefaultEngineConfig 返回默认配置。
func DefaultEngineConfig() *EngineConfig {
	return &EngineConfig{
		InnerLoopInterval:        0, // 事件驱动，不使用 ticker
		MidLoopInterval:          2 * time.Minute,
		MaxConcurrentReflections: 3,
		BaselinePassRate:         0.8,
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

// Engine M9 Self-Improvement Engine 主结构。
// 所有依赖通过构造器注入，无全局状态（R1.3）。
type Engine struct {
	cfg        *EngineConfig
	reflector  Reflector
	curriculum CurriculumGenerator
	rollout    RolloutAdvancer

	// 事件通道（由外部订阅者推入，Engine 消费）
	taskEvents    <-chan TaskCompleteEvent
	versionEvents <-chan VersionChangeEvent

	// 新增：M9 自改善闭环事件通道
	// heuristicEvents 由 swarm.ReflexionEngine 写入，Engine 内环消费更新 ErrorPatternMemory
	heuristicEvents <-chan protocol.HeuristicGeneratedPayload
	// evalEvents 由 governance/eval.RunnerImpl 写入，Engine 外环消费更新 prompt_versions.score
	evalEvents <-chan protocol.EvalCompletedPayload

	// 新增：外部适配器（接口解耦，防 swarm→self_improve 循环引用）
	optimizer        PromptOptimizerAdapter // 可 nil，nil 时跳过 AvoidRule 注入
	versionStore     VersionStoreAdapter    // 可 nil，nil 时跳过评分更新
	heuristicsWriter HeuristicsWriter       // 可 nil，nil 时跳过成功轨迹写入（P1-4）

	// 反思并发信号量（控制 goroutine 数量）
	sem chan struct{}

	// Tier1+：从 M3 Metrics 读取实时 SurpriseIndex 的函数（nil 时用 0.5 占位）
	surpriseIndexFn func() float64
}

// SetSurpriseIndexProvider 注入 SurpriseIndex 读取函数（Tier1+ 从 M3 Metrics 读取）。
func (e *Engine) SetSurpriseIndexProvider(fn func() float64) { e.surpriseIndexFn = fn }

// SetOptimizer 注入 PromptOptimizerAdapter（可选；nil 时内环跳过 AvoidRule 注入）。
func (e *Engine) SetOptimizer(opt PromptOptimizerAdapter) { e.optimizer = opt }

// SetVersionStore 注入 VersionStoreAdapter（可选；nil 时外环跳过评分更新）。
func (e *Engine) SetVersionStore(vs VersionStoreAdapter) { e.versionStore = vs }

// SetHeuristicsWriter 注入 HeuristicsWriter（可选；nil 时内环跳过成功轨迹写入，P1-4）。
func (e *Engine) SetHeuristicsWriter(hw HeuristicsWriter) { e.heuristicsWriter = hw }

// SetHeuristicEvents 注入 HeuristicGenerated 事件通道（read-only 端）。
func (e *Engine) SetHeuristicEvents(ch <-chan protocol.HeuristicGeneratedPayload) {
	e.heuristicEvents = ch
}

// SetEvalEvents 注入 EvalCompleted 事件通道（read-only 端）。
func (e *Engine) SetEvalEvents(ch <-chan protocol.EvalCompletedPayload) {
	e.evalEvents = ch
}

func (e *Engine) currentSurpriseIndex() float64 {
	if e.surpriseIndexFn != nil {
		return e.surpriseIndexFn()
	}
	return 0.5
}

// NewEngine 创建 Engine 实例，所有依赖必须非 nil（fail-fast）。
func NewEngine(
	cfg *EngineConfig,
	reflector Reflector,
	curriculum CurriculumGenerator,
	rollout RolloutAdvancer,
	taskEvents <-chan TaskCompleteEvent,
	versionEvents <-chan VersionChangeEvent,
) *Engine {
	if cfg == nil {
		cfg = DefaultEngineConfig()
	}
	maxConcurrent := cfg.MaxConcurrentReflections
	if maxConcurrent <= 0 {
		maxConcurrent = 3
	}
	return &Engine{
		cfg:           cfg,
		reflector:     reflector,
		curriculum:    curriculum,
		rollout:       rollout,
		taskEvents:    taskEvents,
		versionEvents: versionEvents,
		sem:           make(chan struct{}, maxConcurrent),
	}
}

// Run 启动三环主循环，阻塞直到 ctx 取消。
// 内环：消费 taskEvents + heuristicEvents，并发执行 Reflect（受信号量限制）
// 中环：2min ticker 触发 AutoCurriculumGenerator
// 外环：消费 versionEvents + evalEvents，触发 Rollout AdvanceGate
func (e *Engine) Run(ctx context.Context) error { //nolint:gocyclo
	midTicker := time.NewTicker(e.cfg.MidLoopInterval)
	defer midTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		// 内环：任务完成事件 → Reflexion（失败）或 HeuristicsWriter（成功）
		case ev, ok := <-e.taskEvents:
			if !ok {
				return nil
			}
			if !ev.Success {
				select {
				case e.sem <- struct{}{}:
					go func(event TaskCompleteEvent) {
						defer func() { <-e.sem }()
						result := &TaskResult{
							TaskID:       event.TaskID,
							Success:      event.Success,
							FailureClass: event.Failure,
							Output:       event.Output,
						}
						if e.reflector != nil {
							_, _ = e.reflector.Reflect(ctx, event.TaskID, event.TaskType, result, nil, 0)
						}
					}(ev)
				default:
					// 信号量满，丢弃（尽力而为原则）
				}
			} else {
				// 成功轨迹写入 HeuristicsMemory，驱动 success_rate 更新（P1-4）。
				// 原实现忽略成功任务，导致 skillGapAnalysis 永远读不到有效 success_rate。
				if e.heuristicsWriter != nil {
					e.heuristicsWriter.RecordSuccess(ev.TaskID, ev.TaskType)
				}
			}

		// 内环（新）：HeuristicGenerated → 更新 PromptOptimizer.ErrorPatternMemory
		case ev, ok := <-e.heuristicEvents:
			if !ok {
				e.heuristicEvents = nil
				continue
			}
			if e.optimizer != nil && ev.AvoidRule != "" {
				e.optimizer.AddAvoidRule(ev.TaskType, ev.AvoidRule)
			}

		// 中环：定时触发 AutoCurriculum
		case <-midTicker.C:
			if e.curriculum != nil {
				go func() {
					_ = e.curriculum.Generate(ctx, e.currentSurpriseIndex())
				}()
			}

		// 外环：版本变更 → Rollout 门控推进
		case ev, ok := <-e.versionEvents:
			if !ok {
				return nil
			}
			if e.rollout != nil {
				go func(event VersionChangeEvent) {
					_ = e.rollout.AdvanceGate(ctx, event.CandidateVersion, event.Stats)
				}(ev)
			}

		// 外环（新）：EvalCompleted → 更新评分 + 触发 Rollout
		case ev, ok := <-e.evalEvents:
			if !ok {
				e.evalEvents = nil
				continue
			}
			e.handleEvalCompleted(ctx, ev)
		}
	}
}

// handleEvalCompleted 处理 Eval 完成事件：更新评分，若达到激活条件则触发 Rollout。
// 设计：score ≥ baselinePassRate × 1.05 且 !BlockDeploy → 激活候选版本 → AdvanceGate。
func (e *Engine) handleEvalCompleted(ctx context.Context, ev protocol.EvalCompletedPayload) {
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
		go func() {
			_ = e.rollout.AdvanceGate(ctx, ev.CandidateID, RolloutStats{
				BaselineErrorRate: 1.0 - e.cfg.BaselinePassRate,
				ErrorRate:         1.0 - ev.PassRate,
			})
		}()
	}
}
