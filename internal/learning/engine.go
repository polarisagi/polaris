package learning

import (
	"context"
	"database/sql"
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// Engine M9 Self-Improvement Engine 主结构。
// 所有依赖通过构造器注入，无全局状态（R1.3）。
type Engine struct {
	cfg        *EngineConfig
	reflector  Reflector
	curriculum CurriculumGenerator
	rollout    RolloutAdvancer

	// 事件通道（由外部订阅者推入，Engine 消费）
	taskEvents    chan TaskCompleteEvent
	versionEvents <-chan VersionChangeEvent

	// 新增：M9 自改善闭环事件通道
	// heuristicEvents 由 swarm.ReflexionEngine 写入，Engine 内环消费更新 optimizer.ErrorPatternMemory
	heuristicEvents <-chan types.HeuristicGeneratedPayload
	// evalEvents 由 governance/eval.RunnerImpl 写入，Engine 外环消费更新 prompt_versions.score
	evalEvents <-chan types.EvalCompletedPayload

	// 新增：外部适配器（接口解耦，防 swarm→self_improve 循环引用）
	optimizer        PromptOptimizerAdapter // 可 nil，nil 时跳过 AvoidRule 注入
	versionStore     VersionStoreAdapter    // 可 nil，nil 时跳过评分更新
	heuristicsWriter HeuristicsWriter       // 可 nil，nil 时跳过成功轨迹写入（P1-4）

	// 反思并发信号量（控制 goroutine 数量）
	sem chan struct{}

	// Tier1+：从 M3 Metrics 读取实时 SurpriseIndex 的函数（nil 时用 0.5 占位）
	surpriseIndexFn func() float64

	// L3/L4 进化网关依赖（可为 nil）
	hitlGateway     protocol.HITL
	stagingPipeline StagingPipelineAdapter
	l4TriggerCh     <-chan Change // admin 主动触发 L4，非自动检测
	evolutionGate   EvolutionGate // M12: EvolutionGate instance
	gate            backgroundGate

	db *sql.DB // DB for cursor persistence

	// taskSeqCounter：ReportOutcome 内部构造的 TaskCompleteEvent 单调递增序号
	// （2026-07-04 审计补齐，供 learning_cursors 幂等去重使用）。atomic 而非裸
	// int64，因为 ReportOutcome 可能被多个调用方并发调用。
	taskSeqCounter atomic.Int64
}

type backgroundGate interface {
	BackgroundPermit(priority int) bool
}

func (e *Engine) WithBackgroundGate(g backgroundGate) { e.gate = g }

// SetSurpriseIndexProvider 注入 SurpriseIndex 读取函数（Tier1+ 从 M3 Metrics 读取）。
func (e *Engine) SetSurpriseIndexProvider(fn func() float64) { e.surpriseIndexFn = fn }

// SetDB 注入数据库连接。
func (e *Engine) SetDB(db *sql.DB) { e.db = db }

// SetHITLGateway 注入 HITL 网关（L3/L4 审批路径；nil 时跳过通知）。
func (e *Engine) SetHITLGateway(h protocol.HITL) { e.hitlGateway = h }

// SetStagingPipeline 注入 Staging 流水线（审批通过后提交候选版本）。
func (e *Engine) SetStagingPipeline(s StagingPipelineAdapter) { e.stagingPipeline = s }

// SetL4TriggerChannel 注入 L4 管理员信号通道（admin 主动触发，非自动）。
func (e *Engine) SetL4TriggerChannel(ch <-chan Change) { e.l4TriggerCh = ch }

// SetOptimizer 注入 PromptOptimizerAdapter（可选；nil 时内环跳过 AvoidRule 注入）。
func (e *Engine) SetOptimizer(opt PromptOptimizerAdapter) { e.optimizer = opt }

// SetVersionStore 注入 VersionStoreAdapter（可选；nil 时外环跳过评分更新）。
func (e *Engine) SetVersionStore(vs VersionStoreAdapter) { e.versionStore = vs }

// SetHeuristicsWriter 注入 HeuristicsWriter（可选；nil 时内环跳过成功轨迹写入，P1-4）。
func (e *Engine) SetHeuristicsWriter(hw HeuristicsWriter) { e.heuristicsWriter = hw }

// SetHeuristicEvents 注入 HeuristicGenerated 事件通道（read-only 端）。
func (e *Engine) SetHeuristicEvents(ch <-chan types.HeuristicGeneratedPayload) {
	e.heuristicEvents = ch
}

// SetEvalEvents 注入 EvalCompleted 事件通道（read-only 端）。
func (e *Engine) SetEvalEvents(ch <-chan types.EvalCompletedPayload) {
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
	taskEvents chan TaskCompleteEvent,
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
		evolutionGate: &SimpleEvolutionGate{},
	}
}

// Run 启动三环主循环，阻塞直到 ctx 取消。
// 内环：消费 taskEvents + heuristicEvents，并发执行 Reflect（受信号量限制）
// 中环：2min ticker 触发 AutoCurriculumGenerator
// 外环：消费 versionEvents + evalEvents，触发 Rollout AdvanceGate
//
// L2 (SkillGeneration) 由 LogicCollapseMonitor 在 RecordSuccess 时异步触发。
// L3 (StrategyModify)  策略漂移检测 → HITLGateway.Prompt → 人工审批 → SubmitCandidate
// L4 (SourceArchitecture) 多签名审批门控 → SubmitCandidate
func (e *Engine) Start(ctx context.Context) error { //nolint:gocyclo
	cursors := e.loadCursors(ctx)
	midTicker := time.NewTicker(e.cfg.MidLoopInterval)
	defer midTicker.Stop()

	l3Interval := e.cfg.L3CheckInterval
	if l3Interval <= 0 {
		l3Interval = 10 * time.Minute
	}
	l3Ticker := time.NewTicker(l3Interval)
	defer l3Ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		// 内环：任务完成事件 → Reflexion（失败）或 HeuristicsWriter（成功）
		case ev, ok := <-e.taskEvents:
			if !ok {
				return nil
			}
			if ev.Seq > 0 && ev.Seq <= cursors["task"] {
				continue // 幂等跳过
			}
			if ev.Seq > 0 {
				cursors["task"] = ev.Seq
				e.saveCursorAsync(ctx, "task", ev.Seq)
			}
			if !ev.Success {
				select {
				case e.sem <- struct{}{}:
					event := ev
					concurrent.SafeGo(ctx, "learning.engine.reflect", func(sgCtx context.Context) {
						defer func() { <-e.sem }()
						result := &TaskResult{
							TaskID:       event.TaskID,
							Success:      event.Success,
							FailureClass: event.Failure,
							Output:       event.Output,
						}
						if e.reflector != nil {
							_, _ = e.reflector.Reflect(sgCtx, event.TaskID, event.TaskType, result, nil, 0)
						}
					})
				default:
					// 信号量满，丢弃（尽力而为原则）
				}
			} else {
				// 成功轨迹写入 optimizer.HeuristicsMemory，驱动 success_rate 更新（P1-4）。
				// 原实现忽略成功任务，导致 skillGapAnalysis 永远读不到有效 success_rate。
				if e.heuristicsWriter != nil {
					e.heuristicsWriter.RecordSuccess(ev.TaskID, ev.TaskType)
				}
			}

		// 内环（新）：HeuristicGenerated → 更新 optimizer.PromptOptimizer.optimizer.ErrorPatternMemory
		case ev, ok := <-e.heuristicEvents:
			if !ok {
				e.heuristicEvents = nil
				continue
			}
			if ev.Seq > 0 && ev.Seq <= cursors["heuristic"] {
				continue
			}
			if ev.Seq > 0 {
				cursors["heuristic"] = ev.Seq
				e.saveCursorAsync(ctx, "heuristic", ev.Seq)
			}
			if e.optimizer != nil && ev.AvoidRule != "" {
				e.optimizer.AddAvoidRule(ev.TaskType, ev.AvoidRule)
			}

		// 中环：定时触发 AutoCurriculum
		case <-midTicker.C:
			if e.gate != nil && !e.gate.BackgroundPermit(2) {
				continue // skip 本轮
			}
			if e.curriculum != nil {
				concurrent.SafeGo(ctx, "learning-curriculum-generate", func(ctx context.Context) {
					_ = e.curriculum.Generate(ctx, e.currentSurpriseIndex())
				})
			}

		// L3 策略漂移检测（周期性）
		case <-l3Ticker.C:
			e.detectL3Trigger(ctx)

		// L4 管理员主动触发信号
		case change, ok := <-e.l4TriggerCh:
			if !ok {
				e.l4TriggerCh = nil
				continue
			}
			e.detectL4Trigger(ctx, change)

		// 外环：版本变更 → Rollout 门控推进
		case ev, ok := <-e.versionEvents:
			if !ok {
				return nil
			}
			if ev.Seq > 0 && ev.Seq <= cursors["version"] {
				continue
			}
			if ev.Seq > 0 {
				cursors["version"] = ev.Seq
				e.saveCursorAsync(ctx, "version", ev.Seq)
			}
			if e.rollout != nil {
				concurrent.SafeGo(ctx, "learning-rollout-advance", func(ctx context.Context) {
					_ = e.rollout.AdvanceGate(ctx, ev.CandidateVersion, ev.Stats)
				})
			}

		// 外环（新）：EvalCompleted → 更新评分 + 触发 Rollout
		case ev, ok := <-e.evalEvents:
			if !ok {
				e.evalEvents = nil
				continue
			}
			if ev.Seq > 0 && ev.Seq <= cursors["eval"] {
				continue
			}
			if ev.Seq > 0 {
				cursors["eval"] = ev.Seq
				e.saveCursorAsync(ctx, "eval", ev.Seq)
			}
			e.handleEvalCompleted(ctx, ev)
		}
	}
}
