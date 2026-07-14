// boot_agent.go — §8~§10.5 启动阶段：
// Eval Harness → Blackboard → Agent Kernel → M9 Self-Improvement → Supervisor。
// AgentBundle 持有所有 Agent 层产物，向 run() 和 bootServer 传递。
//
// 注意：Supervisor.Start() 由 run() 在注册完 defer 后调用，确保 defer Supervisor.Stop()
// 在 Start() 之前已注册（避免提前清理）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/polarisagi/polaris/internal/knowledge"
	"github.com/polarisagi/polaris/internal/knowledge/connector"

	"github.com/polarisagi/polaris/internal/action/lam"
	"github.com/polarisagi/polaris/internal/learning/curriculum"
	"github.com/polarisagi/polaris/internal/learning/reflexion"
	"github.com/polarisagi/polaris/internal/learning/surprise"
	"github.com/polarisagi/polaris/internal/learning/synthetic"
	"github.com/polarisagi/polaris/internal/memory"
	"github.com/polarisagi/polaris/internal/prompt/optimizer"
	"github.com/polarisagi/polaris/internal/security/guard"

	sysagent "github.com/polarisagi/polaris/internal/agent"
	agentctx "github.com/polarisagi/polaris/internal/agent/context"
	"github.com/polarisagi/polaris/internal/automation"
	"github.com/polarisagi/polaris/internal/automation/notify"
	"github.com/polarisagi/polaris/internal/eval"
	"github.com/polarisagi/polaris/internal/eval/analysis"
	"github.com/polarisagi/polaris/internal/eval/control"
	"github.com/polarisagi/polaris/internal/eval/harness"
	"github.com/polarisagi/polaris/internal/eval/regression"
	agentdag "github.com/polarisagi/polaris/internal/execute/dag"
	"github.com/polarisagi/polaris/internal/extension/skill"
	"github.com/polarisagi/polaris/internal/learning"
	"github.com/polarisagi/polaris/internal/observability/budget"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/observability/probe"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/credential"

	"github.com/polarisagi/polaris/internal/execute/orchestrator"
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/internal/swarm/agents"
	"github.com/polarisagi/polaris/internal/swarm/planner"
	"github.com/polarisagi/polaris/internal/swarm/supervisor"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// AgentBundle 持有 §8~§10.5 所有 Agent 层产物。
type AgentBundle struct {
	// Eval Harness
	EvalRunner *harness.RunnerImpl // 具体类型：InjectAgent/RunSuite 定义在 *RunnerImpl 上
	// EvalStore/MetaEvalSentinel 供 boot_server.go 的 httpServer.SetEvalAdmin 使用
	// （V8-S2 meta_holdout 运维接口，见 internal/gateway/server/sysadmin/evaladmin）。
	EvalStore        *harness.SQLiteEvalStore
	MetaEvalSentinel *analysis.MetaEvalSentinel

	// Blackboard & Scheduler
	Blackboard     *orchestrator.SQLiteBlackboard
	Sched          *automation.SQLiteScheduler
	AgentRegistry  *orchestrator.AgentRegistry
	Orch           *orchestrator.Orchestrator
	PipelineOrch   *orchestrator.PipelineOrchestrator
	PatternDAGExec *orchestrator.PatternDAGExecutor
	MapReduceExec  *orchestrator.MapReduceExecutor
	ParallelExec   *orchestrator.ParallelExecutor
	SequentialExec *orchestrator.SequentialExecutor
	SwarmCoord     *orchestrator.SwarmCoordinator

	// Agent Kernel & DAG Executor
	Agent   *sysagent.Agent
	DAGExec *agentdag.DAGExecutor

	// M9 Self-Improvement
	M9Engine     *learning.Engine
	RolloutStore *optimizer.SQLiteRolloutStore // 与 M9Engine 共用；nil 时 M9 Staging/Shadow 门禁禁用

	// AgentPool for per-session web agents
	AgentPool *sysagent.Pool

	// PersonaRefiner 用户画像精炼器（M05 §2.3），跨 agent-0/AgentPool/ChatHandler
	// 共享同一进程级单例；boot_server.go 通过 Server.SetPersonaRefiner 注入 ChatHandler
	// 用于系统提示词组装（消费端见 chat/system_prompt.go）。
	PersonaRefiner *agentctx.PersonaRefiner

	// Supervisor Tree（Workers 已注册；由 run() 调用 Start()）
	Supervisor *supervisor.Supervisor

	// ReaperStop：run() 在 shutdown 时显式调用（defer 也会再次调用，幂等）
	ReaperStop context.CancelFunc
}

// buildAgent 构造并完全装配一个 Agent 实例。
// 提取此函数消除 bootAgent 中主 Agent 与 AgentPool 工厂的重复装配代码。
// sessionID 决定 AgentID；其余参数来自 boot 阶段已就绪的 Bundle。
func buildAgent(
	sessionID string,
	sb *SubstrateBundle,
	mb *MemoryBundle,
	tb *ToolBundle,
	kb *KnowledgeBundle,
	taskRepo *repo.SQLiteTaskReadRepository,
	epAdapter agentctx.MemoryRetriever,
	knowAdapter agentctx.KnowledgeRetriever,
	lamEngine *lam.ComputerUseEngine,
	reflectionWorker *reflexion.ReflectionWorker,
	prefs map[string]string,
	bgCtx context.Context,
	personaRefiner *agentctx.PersonaRefiner,
) *sysagent.Agent {
	a := sysagent.NewAgent(sessionID, taskRepo, nil, sb.Router)
	a.SetExtQuerier(sb.Store.DB())
	a.Config.MaxReplan = sb.Cfg.Thresholds.M4Kernel.MaxReplanAttempts
	a.Config.DefaultBudget = sb.Cfg.Thresholds.M4Kernel.DefaultBudget
	a.Config.MaxSteps = sb.Cfg.Thresholds.M4Kernel.MaxSteps
	a.Config.IdleTimeoutSec = sb.Cfg.Thresholds.M4Kernel.SuspendIdleThresholdMin * 60
	a.Config.SurpriseHintThreshold = sb.Cfg.Thresholds.M4Kernel.SurpriseHintThreshold
	a.InjectHITL(tb.HITLGateway)
	a.InjectToolExecutor(tb.Dispatcher)
	// S_VALIDATE TaintGate 人工复核豁免（M11 §2.5 SanitizeByUserReview，2026-07-14）：
	// 与 tb.ToolReg 的出口污点检查共享同一个 ExemptionVault 实例。
	a.InjectTaintReviewChecker(tb.ExemptionVault)
	a.InjectOutboxWriter(sb.Outbox)
	// execute/dag.Runner/Validator 均无状态，buildAgent 同时服务 agent-0 与
	// AgentPool 每次动态创建的 per-session Agent，此处注入而非仅在 agent-0
	// 构造后追加，确保所有 Agent 实例（含 Pool 派生）都能跑通 S_EXECUTE/
	// S_VALIDATE（2026-07-12 随 internal/execute 模块化新增，见 provider.go）。
	a.InjectDAGRunner(agentdag.NewRunner())
	a.InjectDAGValidator(agentdag.NewValidator())
	a.SetAssembler(agentctx.NewAssembler(epAdapter, knowAdapter))
	a.InjectPlannerSpawner(func(ctx context.Context, goal, taskType string, provider protocol.Provider) {
		whisperChan := a.GetWhisperChan()
		if whisperChan == nil {
			return
		}
		// tb.Dispatcher 同时满足 planner.ToolLookup（Lookup(name) (types.Tool, error)），
		// 使 TaskDecomposer 能对 LLM 生成的 tool_name 做白名单校验（GR-7-002）。
		pool := planner.NewPlannerPool(goal, taskType, provider, whisperChan, tb.Dispatcher)
		pool.Run(ctx)
	})
	a.InjectExtensionActivator(&extensionActivatorAdapter{inner: tb.Activator})
	if tb.PolicyEvolver != nil {
		a.InjectToolHintProvider(tb.PolicyEvolver)
	}
	// PRM（S_PLAN 候选 DAG 选优，docs/arch/M04-Agent-Kernel.md §4.6）：默认关闭，
	// 需 Operator 在 configs 显式打开 m4_kernel.prm.enabled（2026-07-13 deadcode
	// 复核发现完整实现但从未接线：agent.NewDefaultPRM/InjectPRM 此前零调用点）。
	if sb.Cfg.Thresholds.M4Kernel.PRMEnabled {
		a.InjectPRM(sysagent.NewDefaultPRM(sysagent.PRMConfig{
			Enabled:        true,
			ScorerModel:    sb.Cfg.Thresholds.M4Kernel.PRMScorerModel,
			MinThreshold:   sb.Cfg.Thresholds.M4Kernel.PRMMinThreshold,
			MaxCandidates:  sb.Cfg.Thresholds.M4Kernel.PRMMaxCandidates,
			ComplexityGate: sb.Cfg.Thresholds.M4Kernel.PRMComplexityGate,
		}, sb.Router))
	}
	a.InjectReplanExtensionActivationTimeout(
		time.Duration(sb.Cfg.Thresholds.M4Kernel.ReplanExtensionActivationSecs) * time.Second,
	)
	a.InjectMemory(memory.NewMemoryFacade(memory.NewMemorySystemFromMemImpl(mb.Mem)))
	if mb.Mem != nil {
		a.SetMemoryInjector(mb.Mem)
	}
	a.SetLAMEngine(&lamPolicyAdapter{inner: lamEngine})
	sc := surprise.NewSurpriseCalculator(mb.FallacyPool)
	a.SetSurpriseCalc(sc)
	if kb != nil && kb.KnowledgeBase != nil {
		a.SetKnowledgeSearcher(&fsmKnowledgeAdapter{kb: kb.KnowledgeBase})
	}
	if prefs != nil {
		a.SetPreferences(prefs)
	}

	// Inject trajectory store event writer for state trans and LLM call recording (Task 1)
	a.GetStateMachine().SetSessionEventWriter(newStoreEventWriter(sb.Store))

	a.InjectTerminalCallback(func(_ context.Context, taskID, taskType string, replanCount int, success bool) {
		concurrent.SafeGo(bgCtx, "reflection-worker-"+sessionID, func(gctx context.Context) {
			if err := reflectionWorker.ConsolidateReflections(gctx, taskID, taskType, replanCount, success); err != nil {
				slog.Debug("polaris: reflection consolidation skipped", "task", taskID, "err", err)
			}
		})
	})
	// PersonaRefiner（M05 §2.3）：agent-0 与 AgentPool 派生 Agent 共享同一进程级
	// 单例（2026-07-13 deadcode 复核发现完整实现但从未接线：NewPersonaRefiner/
	// Load/RefineAtSessionEnd/Save/ToUserPreferences 此前零生产调用点）。
	a.InjectPersonaRefiner(personaRefiner)
	return a
}

// bootAgent 执行 §8~§10.5 初始化，返回 Agent 层 bundle。
// Supervisor.Start() 故意留在 run() 中调用，以确保 defer Supervisor.Stop() 先行注册。
type piiScrubberAdapter struct {
	detector *guard.PIIDetector
}

func (p *piiScrubberAdapter) Scrub(text string) string {
	if p.detector == nil {
		return text
	}
	res, _, _ := p.detector.Redact(context.Background(), text)
	return res
}

func bootAgent(ctx context.Context, sb *SubstrateBundle, mb *MemoryBundle, tb *ToolBundle, kb *KnowledgeBundle) (*AgentBundle, error) { //nolint:gocyclo
	// ─── §8 Eval Harness (L3 M12) ────────────────────────────────────────────
	evalAccessEngine := control.NewEngine(nil)
	evalStore := harness.NewSQLiteEvalStore(sb.Store, evalAccessEngine)
	evalRunner := harness.NewRunner(sb.Store, evalStore)
	// V8-S2 Meta-Eval Sentinel（meta_holdout 隔离分区审计，见 00-Global-Dictionary.md
	// §V8-Principle + internal/eval/analysis/meta_eval.go）。仅构造，不在此处调用——
	// 调用入口是 evaladmin 的 HTTP handler（httpServer.SetEvalAdmin，boot_server.go），
	// 需要 meta_auditor 签名才能真正读取 meta_holdout/触发审计。
	metaEvalSentinel := analysis.NewMetaEvalSentinel(evalStore)

	// Task 5: ContinuousSamplingMonitor 联动
	samplingMonitor := analysis.NewContinuousSamplingMonitor(nil)
	samplingMonitor.Start(ctx)
	evalRunner.InjectL3ThresholdProvider(samplingMonitor)

	// [Task 21] Setup LightweightRegressionDetector and inject into HITLGateway
	if tb.HITLGateway != nil {
		l3Detector := regression.NewLightweightRegressionDetector(sb.Store.DB())
		cooldown := time.Duration(sb.Cfg.Thresholds.M11Policy.L3ImprovementCooldownSeconds) * time.Second
		tb.HITLGateway.SetL3RegressionDeps(evalRunner, &regressionDetectorAdapter{inner: l3Detector}, cooldown)
	}

	// Task 4: SyntheticEvalGen Pipeline
	evalGen := synthetic.NewEvalGenerator(true, sb.Router)
	concurrent.SafeGo(ctx, "synthetic-eval-gen", func(ctx context.Context) {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				chunks, err := kb.Ingester.GetRecentChunks(ctx, 100)
				if err != nil || len(chunks) == 0 {
					continue
				}
				cases, err := evalGen.GenerateCases(ctx, chunks)
				if err != nil {
					slog.Warn("synthetic eval gen failed", "err", err)
					continue
				}
				for _, c := range cases {
					// [W-5-H] SyntheticCaseToEvalCase 接入
					evalCase := eval.SyntheticCaseToEvalCase(c)
					if err := evalStore.PutCase(ctx, "synthetic", "auto_gen", evalCase); err != nil {
						slog.Warn("failed to put synthetic eval case", "id", c.ID, "err", err)
					}
				}
			}
		}
	})

	// ─── §9 Blackboard & Scheduler (L2 M8 + L3 M13) ─────────────────────────
	blackboard := orchestrator.NewSQLiteBlackboard(sb.Store.DB())

	// KillSwitch recovery 回调：恢复 oom_evicted 挂起任务
	sb.KS.OnRecovery(func(ctx context.Context) {
		slog.Info("polaris: KillSwitch recovery triggered, resuming suspended tasks")
		rows, err := sb.Store.DB().QueryContext(ctx, "SELECT id FROM tasks WHERE status = ?", int(types.TaskSuspended))
		if err == nil {
			defer rows.Close()
			var taskIDs []string
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err == nil {
					taskIDs = append(taskIDs, id)
				}
			}
			for _, id := range taskIDs {
				_ = blackboard.ResumeFromSuspended(ctx, id)
			}
		}
		ev, _ := protocol.NewOutboxEvent(protocol.TopicProviderRecovered, "killswitch_recovery", map[string]string{"status": "recovered"}, "")
		_ = sb.Outbox.Write(ctx, ev)
	})

	// 热注入 blackboard 到 ProviderRecoveryHandler（bootTools 时尚未装配）
	tb.RecoveryHandler.SetBlackboard(blackboard)
	piiVault := agentctx.NewSessionPIIVault(sb.Store.DB(), []byte(sb.Cfg.System.DataEncryptionKey), memory.NewMemoryFacade(memory.NewMemorySystemFromMemImpl(mb.Mem)))
	tb.RecoveryHandler.SetPIIVault(piiVault)

	// Reaper（挂起任务超时唤醒）
	reaperCtx, reaperStop := context.WithCancel(ctx)
	reaper := orchestrator.NewReaper(blackboard)
	concurrent.SafeGo(reaperCtx, "boot_agent.reaper", func(ctx context.Context) {
		reaper.Run(ctx)
	})

	sched := automation.NewSQLiteScheduler(sb.Store)
	var memGuard *probe.OSMemoryGuard
	var featGate *probe.FeatureGate
	if sb.AutoConf != nil {
		memGuard = sb.AutoConf.Guard
		featGate = sb.AutoConf.Gate
	}
	sched.WithBackgroundGate(budget.NewResourceBudget(sb.TBR, memGuard, featGate))
	// GD-13-001：后台/自动化任务终态通知投递——写入端（消费端见下方 TopicNotification handler）。
	sched.WithOutboxWriter(sb.Outbox)
	slog.Info("polaris: blackboard, scheduler, HITL gateway initialized")

	// ─── §9.5 M8 Multi-Agent Orchestrator ────────────────────────────────────
	agentRegistry := orchestrator.NewAgentRegistry()
	orch := orchestrator.NewOrchestrator(blackboard, agentRegistry, sb.Cfg.System.MaxAgents)

	pipelineOrch := orchestrator.NewPipelineOrchestrator(blackboard, tb.HITLGateway, sb.DecisionLog, 100*time.Millisecond, 3, 1800)
	patternDAGExec := orchestrator.NewPatternDAGExecutor(blackboard, pipelineOrch)
	mapReduceExec := orchestrator.NewMapReduceExecutor(blackboard, 10*time.Minute)
	parallelExec := orchestrator.NewParallelExecutor(blackboard)
	sequentialExec := orchestrator.NewSequentialExecutor(blackboard, 5*time.Minute)
	swarmCoord := orchestrator.NewSwarmCoordinator(blackboard)

	// ─── §10 Agent Kernel (L1 M4) ────────────────────────────────────────────
	taskRepo := repo.NewSQLiteTaskReadRepository(sb.Store.DB())

	epAdapter := &episodicMemAdapter{ep: mb.Mem.Episodic()}
	var knowAdapter agentctx.KnowledgeRetriever
	if kb.KnowledgeBase != nil {
		knowAdapter = &knowledgeAdapter{kb: kb.KnowledgeBase}
	}
	reflectionWorker := reflexion.NewReflectionWorker(mb.Mem.Episodic(), sb.Router, mb.Mem.Reflection())

	var ds lam.DisplayServer
	switch {
	case probe.GlobalFeatureGate() == nil || !probe.GlobalFeatureGate().IsEnabled(probe.FeatureVisionDisplayServer):
		slog.Info("polaris: FeatureVisionDisplayServer not enabled (or not Linux/insufficient tier), LAM StreamBus will use no-op fallback")
	case !lam.XvfbAvailable():
		slog.Info("polaris: FeatureVisionDisplayServer enabled but xdotool/xwd/convert not found in PATH, LAM StreamBus will use no-op fallback (install these binaries to enable GUI automation)")
	default:
		slog.Info("polaris: FeatureVisionDisplayServer enabled, injecting XvfbDisplayServer for LAM StreamBus")
		ds = lam.NewXvfbDisplayServer(":99") // 默认 display ID，可通过配置扩展
	}
	streamBus := lam.NewStreamingActionBus(ds, 1000, 100.0) // 默认 1000 步，100 actions/sec

	lamEngine := lam.NewComputerUseEngine(
		lam.LAMConfig{Enabled: true, ResolverModel: "default-vlm"},
		sb.Router, // VLM provider（动作解析）
		nil,       // executor: 当前无 GUI 执行器，dry-run 模式
		sb.Gate,   // Cedar PolicyGate（deny-by-default）
		streamBus,
	)

	prefs, err := tb.SysRepo.ListPreferences(ctx)
	if err != nil {
		slog.Warn("polaris: failed to load preferences on startup", "err", err)
	}

	// PersonaRefiner（M05 §2.3）：单进程单例，Load() 一次性从 preferences 表加载
	// 冷启动画像；provider 用 sb.Router 供 RefineAtSessionEnd 生成 InteractionSummary。
	personaRefiner := agentctx.NewPersonaRefiner(sb.Store.DB(), sb.Router)
	if err := personaRefiner.Load(ctx); err != nil {
		slog.Warn("polaris: persona refiner load failed, using defaults", "err", err)
	}

	agent := buildAgent("agent-0", sb, mb, tb, kb, taskRepo, epAdapter, knowAdapter, lamEngine, reflectionWorker, prefs, ctx, personaRefiner)

	maxConcurrent := sb.Cfg.System.MaxAgents
	if maxConcurrent <= 0 {
		maxConcurrent = 4
	}

	agentPool := sysagent.NewPool(func(sessionID string) *sysagent.Agent {
		return buildAgent(sessionID, sb, mb, tb, kb, taskRepo, epAdapter, knowAdapter, lamEngine, reflectionWorker, prefs, ctx, personaRefiner)
	}, maxConcurrent)
	// KillSwitch 三阶段熔断（ADR-0009）接入：Pause/FullStop 阶段拒绝新 Agent 执行，
	// Agent 内核异常退出上报错误计数（Acquire/AcquireHeadless 是全部触发路径的唯一收敛点）。
	if sb.KS != nil {
		agentPool.WithKillSwitchGate(sb.KS)
	}

	// 通用兜底 Blackboard 任务消费者（2026-07-12 unwired-code-audit 复查补齐）：
	// M8 Orchestrator 的中心化按类型下推机制在生产环境从未被激活（RegisterWorker
	// 零调用），导致 handleAgentQuery/ProviderRecoveryHandler 兜底分支/
	// AutoCurriculumGenerator 发布的任务发布后无人认领，静默进入黑洞直至饥饿
	// reaper 强制失败。此处接入 DefaultTaskWorker，复用同日已验证的
	// workflowadmin.RunStepWorkerLoop"自订阅+CAS"模式，排除已有专用 Worker
	// 处理的 "workflow_step" 类型，其余任务一律走 AgentPool.AcquireHeadless。
	defaultTaskWorker := orchestrator.NewDefaultTaskWorker(blackboard, agentPool, "workflow_step")

	agentRegistry.Register("agent-0", orchestrator.AgentCard{ //nolint:errcheck
		Name:   "agent-0",
		Skills: []string{"general"},
	}, agent)

	// 委托 tb.Dispatcher.ExecuteWithTaint（未注册工具由其内部 Lookup 显式拒绝，不静默放行），
	// 与 Dispatcher / Agent 直接 tool_call 共用同一条 PolicyGate→沙箱→执行 链路，不重复构造 ExecRequest。
	dagExec := agentdag.NewDAGExecutor(func(ctx context.Context, toolName string, args []byte, taintLevel types.TaintLevel) (*types.ToolResult, error) {
		return tb.Dispatcher.ExecuteWithTaint(ctx, toolName, args, taintLevel)
	}, nil)

	// 注入 ScriptSkillCache + SkillExecutor，激活 System 1 FastPath 技能命中路径。
	// 仅在 FeatureLogicCollapse 开启（即 FeatureL3Sandbox 可用）时注入，
	// 否则技能蒸馏流水线本身未运行，缓存永远为空，注入无意义。
	if sb.AutoConf != nil && sb.AutoConf.Gate.State(probe.FeatureLogicCollapse) != probe.FeatureDisabled {
		if tb.SkillRegistry != nil && tb.SkillExecutor != nil {
			// spawnFn：验证技能在注册表中存在后，返回轻量 ProcessHandle 作为"已确认可用"令牌。
			// ProcessHandle 不持有实际进程；真正的执行由 SkillExecutor.ExecuteSkill 完成。
			spawnFn := func(ctx context.Context, skillID string) (*skill.ProcessHandle, error) {
				if _, err := tb.SkillRegistry.Get(ctx, skillID, ""); err != nil {
					return nil, err
				}
				return &skill.ProcessHandle{SkillID: skillID, ReadyAt: time.Now()}, nil
			}
			skillCache := skill.NewScriptSkillCache(spawnFn, 5, 10, 30)
			// 通过 adapter 注入：agent 包依赖 ScriptSkillCache 接口，不直接持有 *skill.ScriptSkillCache
			agent.WithSkillCache(&skillCacheAdapter{inner: skillCache})
			agent.WithSkillExecutor(tb.SkillExecutor)
			slog.Info("polaris: ScriptSkillCache + SkillExecutor injected into Agent FastPath")
		}
	}

	// ─── [C2.3] Boot 压力 Updater (Cognitive Pressure) ────────────────────────
	concurrent.SafeGo(ctx, "cognitive-pressure-updater", func(ctx context.Context) {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				active := blackboard.CountByStatus(types.TaskClaimed, types.TaskExecuting)
				surprise := metrics.GlobalSurpriseIndex().Current()
				maxPrio := blackboard.MaxActivePriority()
				p := metrics.ComputeCognitivePressure(active, surprise, maxPrio)
				metrics.GlobalCognitivePressure().Set(p)
			}
		}
	})

	slog.Info("polaris: agent kernel & DAG executor initialized")

	evalRunner.InjectAgent(&evalAgentAdapter{agent: agent})
	sched.SetAgentInvoker(&agentInvokerAdapter{agent: agent})

	// ─── §10.3 M9 Self-Improvement Engine ────────────────────────────────────
	taskEventCh := make(chan learning.TaskCompleteEvent, 64)
	versionEventCh := make(chan learning.VersionChangeEvent, 8)

	// 桥接 Blackboard 事件 → M9 TaskCompleteEvent（XR-14：所有后台 goroutine 必须走 SafeGo）
	// taskEventSeq：函数局部（非包级）单调递增计数器，供 learning_cursors 幂等去重
	// 使用（2026-07-04 审计补齐——此前所有生产构造点均未赋值 Seq，导致游标持久化
	// 机制的幂等跳过判断 ev.Seq>0 恒假、完全不生效）。局部变量不违反 CLAUDE.md
	// "internal/ 禁全局可变变量" 规则（该规则约束包级变量，闭包捕获的函数局部
	// 变量不在此列），且天然按 boot 生命周期隔离，无需额外并发保护（仅本 goroutine 写）。
	var taskEventSeq int64
	concurrent.SafeGo(ctx, "m9-bb-bridge", func(ctx context.Context) {
		bbEvents, subErr := blackboard.Subscribe(ctx)
		if subErr != nil {
			slog.Warn("polaris: m9 blackboard subscribe failed", "err", subErr)
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-bbEvents:
				if !ok {
					return
				}
				switch ev.Type {
				case "task_completed":
					taskEventSeq++
					select {
					case taskEventCh <- learning.TaskCompleteEvent{
						Seq:     taskEventSeq,
						TaskID:  ev.TaskID,
						Success: true,
					}:
					default:
					}
				case "task_failed":
					taskEventSeq++
					select {
					case taskEventCh <- learning.TaskCompleteEvent{
						Seq:     taskEventSeq,
						TaskID:  ev.TaskID,
						Success: false,
						Failure: learning.FailureLogic,
					}:
					default:
					}
				}
			}
		}
	})

	// ReflexionEngine：注入真实 LLM 推理（nil 时恒走规则 fallback，反思质量退化）
	// 与 AgentHER 依赖（db + SurrealDB 写入，replaySuccess 成功纠偏轨迹回写技能库）。
	reflexionEngine := reflexion.NewReflexionEngine(mb.FallacyPool, mb.Heuristics, tb.LLMInfer)
	if sb.SurrealStore != nil {
		reflexionEngine.InjectDependencies(sb.Store.DB(), &surrealCognAdapter{s: sb.SurrealStore})
	} else {
		reflexionEngine.InjectDependencies(sb.Store.DB(), nil)
	}
	// 启发式闭环通道：ReflexionEngine 产出 → learning.Engine 内环消费（M9 §2.1 闭环关键路径）。
	heuristicCh := make(chan types.HeuristicGeneratedPayload, 32)
	reflexionEngine.SetHeuristicChannel(heuristicCh)
	reflexionBridge := reflexion.NewReflexionBridge(reflexionEngine)
	idleDetector := curriculum.NewIdleDetector()
	curriculumGen := curriculum.NewAutoCurriculumGenerator(idleDetector, mb.FallacyPool, mb.Heuristics)
	curriculumGen.WithFitnessEval(curriculum.NewSQLFitnessEvaluator(sb.Store.DB()))
	curriculumBridge := reflexion.NewCurriculumBridge(curriculumGen, blackboard)
	// 2026-07-10 审计补齐：此前 rollout 是纯内存 optimizer.NewProgressiveRollout()（无 DB
	// 持久化），promptOptimizer 以 (nil, nil, 0) 构造（无 provider/无 versionStore），
	// Engine.stagingPipeline/versionStore 也从未被 Set 过——M9 GEPA 候选评分、激活、
	// Shadow/Canary 门禁在生产环境实际上从未真正运转。现改为：versionStore/rolloutStore
	// 均为真实 DB 支撑实例，rolloutStore 同时服务 M9Engine 和下方 LogicCollapseMonitor
	// （原各自独立构造两份，互不相干），ConfirmShadow 通过后经 promptActivator 回调激活
	// Prompt 候选（见 optimizer.SQLiteRolloutStore.ConfirmShadow，ADR-0029 §K）。
	versionStore := optimizer.NewPromptVersionStore(sb.Store.DB())
	var (
		rolloutStore  *optimizer.SQLiteRolloutStore
		rolloutBridge learning.RolloutAdvancer
	)
	if rs, rsErr := optimizer.NewSQLiteRolloutStore(sb.Store.DB()); rsErr != nil {
		slog.Warn("polaris: failed to init SQLiteRolloutStore, M9 staging/shadow gating disabled", "err", rsErr)
	} else {
		rs.WithPromptActivator(versionStore)
		// V8-S2 前置检查：默认关闭（M12EvalThresholds.MetaAuditGateEnabled），需运维
		// 显式开启——避免既有部署因从未生成过 meta_audit 记录而被永久卡在 Gate2。
		// evalStore 结构性满足 optimizer.MetaAuditReader（LatestMetaAudit 方法），无需适配器。
		maCfg := sb.Cfg.Thresholds.M12Eval
		rs.WithMetaAudit(evalStore, time.Duration(maCfg.MetaAuditMaxAgeHours)*time.Hour, maCfg.MetaAuditGateEnabled)
		rolloutStore = rs
		rolloutBridge = reflexion.NewRolloutBridge(rs)
	}

	promptOptimizer := optimizer.NewPromptOptimizerWithDB(sb.Router, versionStore, sb.Store.DB(), 0)
	m9Engine := learning.NewEngine(learning.DefaultEngineConfig(), reflexionBridge, curriculumBridge, rolloutBridge, taskEventCh, versionEventCh)
	// 2026-07-04 审计补齐（任务5）：SetDB 此前从未在生产启动代码中被调用，
	// 导致 learning_cursors 持久化/幂等去重整套机制在 e.db==nil 短路下形同虚设
	// （loadCursors 直接返回空 map，saveCursorAsync 第一行判空直接 return）。
	m9Engine.SetDB(sb.Store.DB())
	m9Engine.WithBackgroundGate(budget.NewResourceBudget(sb.TBR, memGuard, featGate))
	m9Engine.SetOptimizer(promptOptimizer)
	m9Engine.SetVersionStore(versionStore)
	if rolloutStore != nil {
		m9Engine.SetStagingPipeline(rolloutStore)
	}
	// M9 闭环接线（P1-2）：内环消费 Reflexion 启发式事件；SurpriseIndex 读 M3 权威值；
	// L3/L4 演化审批走 HITL 网关。
	m9Engine.SetHeuristicEvents(heuristicCh)
	// [W-1-A] 接入 EvalEvents
	evalEventCh := make(chan types.EvalCompletedPayload, 16)
	evalRunner.SetEvalChannel(evalEventCh)
	m9Engine.SetEvalEvents((<-chan types.EvalCompletedPayload)(evalEventCh))

	// [W-5-I] 接入 SetL4TriggerChannel
	l4Ch := make(chan learning.Change, 4)
	m9Engine.SetL4TriggerChannel(l4Ch)
	// 在 Admin 路由中若需触发，可通过全局/其他方式引用 l4Ch。暂不接 admin 路由。

	m9Engine.SetSurpriseIndexProvider(func() float64 { return metrics.GlobalSurpriseIndex().Current() })
	m9Engine.SetHITLGateway(tb.HITLGateway)

	converter := analysis.NewIncidentToEvalConverter(evalStore, &piiScrubberAdapter{detector: tb.PIIDetector})
	m9Engine.SetIncidentConverter(func(ctx context.Context, payload []byte) (string, error) {
		caseData, err := converter.Convert(ctx, payload)
		if err != nil || caseData == nil {
			return "", apperr.Wrap(apperr.CodeInternal, "failed to convert incident", err)
		}
		return caseData.ID, nil
	})

	slog.Info("polaris: M9 self-improvement engine + PromptOptimizer initialized")

	// [W-5-B] 接入 FoundingAnchor 周期漂移检测
	concurrent.SafeGo(ctx, "founding-anchor-drift-detector", func(ctx context.Context) {
		anchor, _, _ := eval.LoadOrCreate(sb.DataDir, nil, nil)
		if anchor == nil {
			return // Not enough trajectories to create anchor yet
		}
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				var recentTrajectories []harness.TrajectoryTrace // TODO: Provide real trajectories
				if len(recentTrajectories) == 0 {
					continue
				}
				fp := eval.ComputeFingerprint(recentTrajectories)
				report := eval.CompareWithAnchor(anchor, fp)
				if sb.DriftMonitor != nil {
					sb.DriftMonitor.SetScore(report.OverallDriftScore)
				}
				if report.ShouldFreeze && m9Engine != nil {
					_ = m9Engine.TriggerCurriculum(ctx)
				}
			}
		}
	})

	// ─── M12 §8 ShadowExecutor：Gate 2(Shadow) 周期回放触发器 ──────────────────
	// 2026-07-10 恢复接线：此前实现完整、测试完整但从未被实例化（详见
	// local_playground 会话记录），Gate 2 对 GEPA 候选完全不构成门禁。现周期性
	// 发现 rollout_states 中停留在 Gate 2 的候选，回放历史流量并对比评分，
	// 通过则调用 ConfirmShadow 推进到 Gate 3，不通过则 Rollback。
	if rolloutStore != nil {
		shadowExec := analysis.NewShadowExecutor(
			sb.Store.DB(),
			sb.Router,
			repo.NewSQLiteMockResponseCache(sb.Store.DB()),
			evalStore,
			rolloutStore,
		)
		concurrent.SafeGo(ctx, "shadow-executor-replay", func(ctx context.Context) {
			// 5 分钟周期，与 boot_knowledge.go corpus-stats-flush 同量级；ADR-0029 §K
			// 认定"分钟级延迟"对离线质量回归验证可接受，非实时链路无需更短周期。
			ticker := time.NewTicker(5 * time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					versions, err := rolloutStore.ListPendingShadow(ctx)
					if err != nil {
						slog.Warn("shadow_executor: list pending shadow failed", "err", err)
						continue
					}
					for _, v := range versions {
						systemPromptOverride, opts := resolveShadowCandidateOpts(ctx, versionStore, v)
						if err := shadowExec.RunReplayBatch(ctx, v, systemPromptOverride, opts); err != nil {
							slog.Warn("shadow_executor: replay batch failed", "version", v, "err", err)
						}
					}
				}
			}
		})
		slog.Info("polaris: ShadowExecutor periodic replay trigger started (5min interval)")
	}

	bgTaskScheduler := curriculum.NewBackgroundTaskScheduler(curriculumGen, blackboard)
	bgTaskScheduler.InjectImmuneGateway(NewImmuneGateway())
	// [W-1-B] 接入 SurpriseReader
	bgTaskScheduler.InjectSurpriseReader(&simpleSurpriseReader{})

	// Task 3: Wire up RedTeamProtocol
	rtp := eval.NewRedTeamProtocol(evalStore)
	rtp.SetAgentPool(agentPool)
	bgTaskScheduler.InjectRedTeamProtocol(rtp)

	bgTaskScheduler.Start(ctx)
	slog.Info("polaris: AutoCurriculumGenerator background scheduler started")

	// 训练适配器使用记录（避免 unused 编译错误；后续 M9 流水线消费）
	_ = sb.QLoRA
	_ = sb.PRM
	_ = sb.Steering

	// 初始化 MemoryAgent（统一经 MemoryFacade 访问记忆子系统，见 M04 §B2）
	memoryFacadeForAgent := memory.NewMemoryFacadeWithStore(memory.NewMemorySystemFromMemImpl(mb.Mem), sb.Store)
	memoryAgent := agents.NewMemoryAgent(memoryFacadeForAgent, agent.GetWhisperChan(), nil)

	// 注入 PII OpaqueToken 检测器与令牌库（M11 §5.1 语义闭环）
	agent.InjectPIITokenizer(tb.PIIDetector, tb.PIITokenVault)

	// TopicAgentInterrupt handler：gateway 写入的中断请求路由到 Agent Kernel。
	// 当前进程内单 kernel（agent-0）+ Pool 共存，Pool 内会话由 gateway 直连路径覆盖，
	// 此 handler 保证 outbox 异步路径不落死信（P0-1：禁止无 handler 的生产者）。
	sb.Outbox.RegisterHandler(protocol.TopicAgentInterrupt, func(_ context.Context, rec *store.OutboxRecord) error {
		var payload struct {
			TaskID  string                 `json:"task_id"`
			Request types.InterruptRequest `json:"request"`
		}
		if err := json.Unmarshal(rec.Payload, &payload); err != nil {
			return nil //nolint:nilerr // malformed payload 跳过，避免 OutboxWorker 无限重试
		}
		agent.Interrupt(payload.Request)
		return nil
	})

	// GD-13-001：后台/自动化任务终态通知投递——消费端。Webhook URL/开关读取
	// preferences 表（notification_webhook_url/notification_enabled），
	// 复用现有 SystemRepository，不新增 schema。
	sb.Outbox.RegisterHandler(protocol.TopicNotification, notify.NewDispatcher(tb.SysRepo).Handle)

	// ─── §10.5 Supervisor Tree（仅注册 workers；Start() 由 run() 在注册 defer 后调用）
	sv := supervisor.NewSupervisor(5, 5*time.Minute)
	// [W-3] 接入 SyncScheduler
	knowledgePipeline := knowledge.NewPipeline(sb.Store.DB(), nil, sb.Outbox, nil)
	for _, conn := range tb.KnowledgeConnRegistry.GetAll() {
		c := conn
		syncScheduler := connector.NewSyncScheduler(c, knowledgePipeline, 0)
		sv.AddWorker(fmt.Sprintf("sync-scheduler-%s", c.Name()), func(ctx context.Context) error {
			return syncScheduler.Start(ctx)
		})
	}

	sv.AddWorker("agent-0", func(ctx context.Context) error {
		return agent.Run(ctx)
	})
	// orch.ListenLoop 不再注册为 Supervisor Worker：其内部 dispatchPendingTasks
	// 在生产环境下 100% 无法成功派发任务（RegisterWorker 从未调用、agent-0 的
	// Skills=["general"] 与真实任务类型永远不匹配），继续运行只会造成资源浪费，
	// 且一旦命中匹配任务会与下方 default-task-worker 产生 CAS 竞争后又无处执行，
	// 白白让任务多等一轮 60s 租约超时。任务派发已由 default-task-worker 承接
	// （见上方 DefaultTaskWorker 注入注释）。orch/agentRegistry 保留供
	// AgentBundle.Orch/AgentRegistry 与未来可能的能力路由复用。
	sv.AddWorker("default-task-worker", func(ctx context.Context) error {
		return defaultTaskWorker.RunLoop(ctx)
	})
	sv.AddWorker("m9-engine", func(ctx context.Context) error {
		return m9Engine.Start(ctx)
	})
	sv.AddWorker("memory-agent", func(ctx context.Context) error {
		memoryAgent.Run(ctx)
		return nil
	})

	// kb 当前用于确认 RAG 已就绪，未来可向 agent 注入知识检索能力
	_ = kb

	// 注册知识库连接器和 SyncScheduler 到 MemoryAgent
	if kb.Ingester != nil { //nolint:nestif // 原因：启动期依次装配 Obsidian/Notion/MCP 三类知识源连接器，各自独立 if/err 分支，属一次性初始化清单而非热路径业务逻辑，参考 internal/automation/hitl/gateway.go Prompt() 的既有豁免先例。
		// Obsidian Connector
		obsidianConn, err := connector.NewObsidianConnector(sb.Layout.Workspace)
		if err != nil {
			slog.Error("polaris: failed to init ObsidianConnector", "err", err)
		} else {
			obsidianSched := connector.NewSyncScheduler(obsidianConn, kb.Ingester, 0)
			memoryAgent.RegisterSyncScheduler(obsidianSched)
			slog.Info("polaris: ObsidianConnector registered to MemoryAgent")
		}

		// Notion Connector — token 优先从 vault 加密存储（复用 preferences 表，
		// 与 Provider API Key 同一套 credential.Vault AES-256-GCM 方案）读取；
		// 首次运行时回退 NOTION_TOKEN 环境变量并将其加密落盘（seed-once，
		// 模式对齐 provider.SeedProvidersFromEnv），此后不再依赖明文 env
		// （env 对同 UID 进程经 /proc/<pid>/environ 可见，不适合长期持有密钥）。
		notionVault, err := credential.NewVaultInDir(sb.DataDir)
		if err != nil {
			slog.Error("polaris: failed to init credential vault for NotionConnector, skipping", "err", err)
		} else {
			sysRepo := repo.NewSQLiteSystemRepository(sb.Store.DB())
			notionConn := connector.NewNotionConnector(func(ctx context.Context) (string, error) {
				return resolveNotionToken(ctx, sysRepo, notionVault)
			})
			notionSched := connector.NewSyncScheduler(notionConn, kb.Ingester, 0)
			memoryAgent.RegisterSyncScheduler(notionSched)
			slog.Info("polaris: NotionConnector registered to MemoryAgent")
		}

		// MCP 知识源连接器（2026-07-04 审计补齐，任务17）：此前 mcp_installer.go
		// 只把声明了 capability=knowledge-source 的 MCP 服务器注册进
		// KnowledgeConnRegistry，但从未有代码读取该注册表去创建 SyncScheduler，
		// 是"注册了但从未被调度"的死数据。此处补齐，与 Obsidian/Notion 走同一
		// 条 SyncScheduler + RegisterSyncScheduler 接入方式。
		if tb.KnowledgeConnRegistry != nil {
			for _, mcpConn := range tb.KnowledgeConnRegistry.GetAll() {
				mcpSched := connector.NewSyncScheduler(mcpConn, kb.Ingester, 0)
				memoryAgent.RegisterSyncScheduler(mcpSched)
				slog.Info("polaris: MCP knowledge source connector registered to MemoryAgent", "id", mcpConn.ID())
			}
		}
	}

	return &AgentBundle{
		EvalRunner:       evalRunner,
		EvalStore:        evalStore,
		MetaEvalSentinel: metaEvalSentinel,
		Blackboard:       blackboard,
		Sched:            sched,
		AgentRegistry:    agentRegistry,
		Orch:             orch,
		PipelineOrch:     pipelineOrch,
		PatternDAGExec:   patternDAGExec,
		MapReduceExec:    mapReduceExec,
		ParallelExec:     parallelExec,
		SequentialExec:   sequentialExec,
		SwarmCoord:       swarmCoord,
		Agent:            agent,
		DAGExec:          dagExec,
		M9Engine:         m9Engine,
		RolloutStore:     rolloutStore,
		AgentPool:        agentPool,
		Supervisor:       sv,
		ReaperStop:       reaperStop,
		PersonaRefiner:   personaRefiner,
	}, nil
}

// resolveShadowCandidateOpts 为 ShadowExecutor 回放解析候选版本的覆盖参数。
// 候选来自 M9 GEPA/PromptOptimizer（handleEvalCompleted 提交，version == prompt_versions.id）
// 时能查到真实 prompt_text，作为 system 消息覆盖返回；候选来自 L3/L4 HITL 审批路径时
// prompt_versions 无对应行（那两条路径提交的 AgentVersionSnapshot 目前只携带描述性文字，
// 没有可结构化的覆盖参数），返回空覆盖——诚实地不构造，避免影子对比在无意义覆盖下
// 产生"必过"的假阳性（详见本次会话对 candidateOpts 来源的讨论）。
func resolveShadowCandidateOpts(ctx context.Context, versionStore *optimizer.PromptVersionStore, version string) (string, []types.InferOption) {
	if versionStore == nil {
		return "", nil
	}
	pv, err := versionStore.GetByID(ctx, version)
	if err != nil || pv == nil || pv.Prompt == "" {
		return "", nil
	}
	return pv.Prompt, nil
}

// notionTokenPrefKey preferences 表中存放 NotionConnector token 密文的 key。
// 复用 016_preferences.sql 的通用 KV 表而非新建专用密钥表——preferences 已经是
// 系统级 KV 存储，值本身在写入前经 credential.Vault 加密，语义上与直接落盘
// 明文 token 完全不同，不需要为单个第三方连接器新增一张表。
const notionTokenPrefKey = "connector_secret:notion_token"

// resolveNotionToken 解析 NotionConnector 所需 token：
//  1. 优先读取 preferences 表中的密文并用 vault 解密；
//  2. 未配置时回退 NOTION_TOKEN 环境变量，读取成功后立即加密写回 preferences
//     （seed-once，此后不再依赖明文 env，模式对齐 provider.SeedProvidersFromEnv）；
//  3. 两者皆无则报错，NotionConnector 的 Watch/List/Fetch 调用会因此失败并被
//     SyncScheduler 的重试/日志路径捕获，不会导致进程崩溃。
func resolveNotionToken(ctx context.Context, sysRepo *repo.SQLiteSystemRepository, vault *credential.Vault) (string, error) {
	encrypted, err := sysRepo.GetPreference(ctx, notionTokenPrefKey)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "read notion token from preferences", err)
	}
	if encrypted != "" {
		token, decErr := vault.Decrypt(encrypted)
		if decErr != nil {
			return "", apperr.Wrap(apperr.CodeInternal, "decrypt notion token", decErr)
		}
		if token != "" {
			return token, nil
		}
	}

	token := os.Getenv("NOTION_TOKEN")
	if token == "" {
		return "", apperr.New(apperr.CodeNotFound, "NOTION_TOKEN not set and no vault-stored token found")
	}

	encToken, encErr := vault.Encrypt(token)
	if encErr != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "encrypt notion token", encErr)
	}
	if err := sysRepo.UpsertPreference(ctx, notionTokenPrefKey, encToken); err != nil {
		// 落盘失败不阻断本次调用——下次调用会再次读取 env 兜底，仅退化为
		// "每次都读明文 env"，不影响功能正确性，只是没有实现"落盘后免依赖 env"的优化。
		slog.Warn("polaris: failed to persist encrypted notion token, will re-read env next time", "err", err)
	}
	return token, nil
}

// simpleSurpriseReader 实现 curriculum.SurpriseReader 接口，读取全局意外度
type simpleSurpriseReader struct{}

func (r *simpleSurpriseReader) CurrentSurprise() float64 {
	return metrics.GlobalSurpriseIndex().Current()
}
