package learning

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/learning/optimizer"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
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
	optimizer        *optimizer.PromptOptimizer // 可 nil，nil 时跳过 AvoidRule 注入
	versionStore     VersionStoreAdapter        // 可 nil，nil 时跳过评分更新
	heuristicsWriter HeuristicsWriter           // 可 nil，nil 时跳过成功轨迹写入（P1-4）

	// 反思并发信号量（控制 goroutine 数量）
	sem chan struct{}

	// Tier1+：从 M3 Metrics 读取实时 SurpriseIndex 的函数（nil 时用 0.5 占位）
	surpriseIndexFn func() float64

	// L3/L4 进化网关依赖（可为 nil）
	hitlGateway     protocol.HITL
	stagingPipeline optimizer.StagingPipeline
	l4TriggerCh     <-chan Change // admin 主动触发 L4，非自动检测
	evolutionGate   EvolutionGate // M12: EvolutionGate instance
	gate            backgroundGate

	incidentConverter func(ctx context.Context, payload []byte) (string, error)

	// 2026-08-08：原为 *sql.DB，见 inv_NoRawSQLDBField。游标持久化只用 Exec/Query。
	db protocol.SQLQuerier // 学习游标持久化

	// 注意：Seq 幂等去重号由事件生产方（写入 taskEvents/heuristicEvents/... 的调用方）
	// 赋值，本 Engine 仅消费 ev.Seq 做单调游标比较（见下方 run 循环），不在本地
	// 构造序号。此前这里有一个 taskSeqCounter atomic.Int64 字段及引用不存在的
	// "ReportOutcome" 方法的注释，属于设计变更后未清理的死字段（unused linter
	// 命中），已随该字段一并删除。

	cursorCache map[string]int64
	cursorMu    sync.Mutex
}

type backgroundGate interface {
	BackgroundPermit(priority int) bool
}

func (e *Engine) WithBackgroundGate(g backgroundGate) { e.gate = g }

// SetSurpriseIndexProvider 注入 SurpriseIndex 读取函数（Tier1+ 从 M3 Metrics 读取）。
func (e *Engine) SetSurpriseIndexProvider(fn func() float64) { e.surpriseIndexFn = fn }

// SetDB 注入数据库连接。
func (e *Engine) SetDB(db protocol.SQLQuerier) { e.db = db }

// SetHITLGateway 注入 HITL 网关（L3/L4 审批路径；nil 时跳过通知）。
func (e *Engine) SetHITLGateway(h protocol.HITL) { e.hitlGateway = h }

func (e *Engine) SetStagingPipeline(s optimizer.StagingPipeline) { e.stagingPipeline = s }

// SetL4TriggerChannel 注入 L4 管理员信号通道（admin 主动触发，非自动）。
func (e *Engine) SetL4TriggerChannel(ch <-chan Change) { e.l4TriggerCh = ch }

func (e *Engine) SetOptimizer(opt *optimizer.PromptOptimizer) { e.optimizer = opt }

func (e *Engine) SetVersionStore(vs VersionStoreAdapter) { e.versionStore = vs }

func (e *Engine) SetHeuristicsWriter(hw HeuristicsWriter) { e.heuristicsWriter = hw }

func (e *Engine) SetIncidentConverter(fn func(ctx context.Context, payload []byte) (string, error)) {
	e.incidentConverter = fn
}

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
		cursorCache:   make(map[string]int64),
		evolutionGate: &SimpleEvolutionGate{},
	}
}

// handleTaskCompleteEvent 处理内环任务完成事件：失败任务异步交给 Reflexion 反思，
// 成功任务记录到 HeuristicsMemory 驱动 success_rate 更新（从 Start 拆出，nestif 治理，行为不变）。
func (e *Engine) handleTaskCompleteEvent(ctx context.Context, ev TaskCompleteEvent) {
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
					if _, err := e.reflector.Reflect(sgCtx, event.TaskID, event.TaskType, result, nil, 0); err != nil {
						slog.Warn("learning engine: reflect failed", "task_id", event.TaskID, "err", err)
					}
				}
			})
		default:
			// [GD-13-005] 信号量满，丢弃（尽力而为原则）——但丢弃本身必须可观测，
			// 否则被丢弃的失败任务既不进 Reflexion，也不计入任何计数器，只有
			// 成功任务经 HeuristicsWriter.RecordSuccess 被记录，success_rate
			// 会系统性偏高（HE-4：不能悄悄丢样本又假装分母完整）。
			if metrics.InstrLearningReflectionDroppedTotal != nil {
				metrics.InstrLearningReflectionDroppedTotal.Add(ctx, 1)
			}
			slog.Warn("learning engine: reflexion semaphore full, dropping failed task event",
				"task_id", ev.TaskID, "task_type", ev.TaskType)
		}
		return
	}

	// 成功轨迹写入 optimizer.HeuristicsMemory，驱动 success_rate 更新（P1-4）。
	// 原实现忽略成功任务，导致 skillGapAnalysis 永远读不到有效 success_rate。
	if e.heuristicsWriter != nil {
		e.heuristicsWriter.RecordSuccess(ev.TaskID, ev.TaskType)
	}
}

// Start 启动三环主循环，阻塞直到 ctx 取消。
// 内环：消费 taskEvents + heuristicEvents，并发执行 Reflect（受信号量限制）
// 中环：2min ticker 触发 AutoCurriculumGenerator
// 外环：消费 versionEvents + evalEvents，触发 Rollout AdvanceGate
//
// L2 (SkillGeneration) 由 LogicCollapseMonitor 在 RecordSuccess 时异步触发。
// L3 (StrategyModify)  策略漂移检测 → HITLGateway.Prompt → 人工审批 → SubmitCandidate
// L4 (SourceArchitecture) 多签名审批门控 → SubmitCandidate
func (e *Engine) Start(ctx context.Context) error { //nolint:gocyclo
	// GR-7-001：游标加载失败必须阻止启动，而非以空/残缺游标继续（会导致
	// 学习引擎从错误位置重放，见 loadCursors 内部注释）。
	cursors, err := e.loadCursors(ctx)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "Engine.Start: loadCursors 失败", err)
	}

	// Start background cursor flusher
	concurrent.SafeGo(ctx, "learning-cursor-flusher", func(ctx context.Context) {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				e.flushCursors(context.WithoutCancel(ctx))
				return
			case <-ticker.C:
				e.flushCursors(ctx)
			}
		}
	})

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
			return ctx.Err() //nolint:wrapcheck // 调用方按 err == context.Canceled 严格比较（见 engine_test.go），须保留哨兵身份

		// 内环：任务完成事件 → Reflexion（失败）或 HeuristicsWriter（成功）
		case ev, ok := <-e.taskEvents:
			if !ok {
				e.taskEvents = nil
				continue
			}
			if ev.Seq > 0 && ev.Seq <= cursors["task"] {
				continue // 幂等跳过
			}
			if ev.Seq > 0 {
				cursors["task"] = ev.Seq
				e.saveCursorAsync(ctx, "task", ev.Seq)
			}
			e.handleTaskCompleteEvent(ctx, ev)

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

			// [W-5-G] IncidentToEvalConverter 接入
			// High-severity incident fallback -> convert to eval case
			if e.incidentConverter != nil && strings.Contains(ev.Heuristic, "high-severity") {
				payload, _ := json.Marshal(ev)
				concurrent.SafeGo(ctx, "learning.incident_convert", func(gctx context.Context) {
					if id, err := e.incidentConverter(gctx, payload); err == nil && id != "" {
						slog.Info("incident converted to eval case", "id", id)
					}
				})
			}

		// 中环：定时触发 AutoCurriculum
		case <-midTicker.C:
			if e.gate != nil && !e.gate.BackgroundPermit(2) {
				continue // skip 本轮
			}
			if e.curriculum != nil {
				concurrent.SafeGo(ctx, "learning-curriculum-generate", func(ctx context.Context) {
					if err := e.curriculum.Generate(ctx, e.currentSurpriseIndex()); err != nil {
						slog.Warn("learning engine: curriculum generate failed", "err", err)
					}
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
				e.versionEvents = nil
				continue
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
					if err := e.rollout.AdvanceGate(ctx, ev.CandidateVersion, ev.Stats); err != nil {
						slog.Warn("learning engine: rollout advance gate failed", "candidate", ev.CandidateVersion, "err", err)
					}
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
