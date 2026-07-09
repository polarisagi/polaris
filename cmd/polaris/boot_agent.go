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

	"github.com/polarisagi/polaris/internal/knowledge/connector"

	"github.com/polarisagi/polaris/internal/action/lam"
	"github.com/polarisagi/polaris/internal/learning/curriculum"
	"github.com/polarisagi/polaris/internal/learning/reflexion"
	"github.com/polarisagi/polaris/internal/learning/surprise"
	"github.com/polarisagi/polaris/internal/memory"
	"github.com/polarisagi/polaris/internal/prompt/optimizer"

	sysagent "github.com/polarisagi/polaris/internal/agent"
	agentctx "github.com/polarisagi/polaris/internal/agent/context"
	agentdag "github.com/polarisagi/polaris/internal/agent/dag"
	"github.com/polarisagi/polaris/internal/automation"
	"github.com/polarisagi/polaris/internal/eval/control"
	"github.com/polarisagi/polaris/internal/eval/harness"
	"github.com/polarisagi/polaris/internal/eval/regression"
	"github.com/polarisagi/polaris/internal/extension/skill"
	"github.com/polarisagi/polaris/internal/learning"
	"github.com/polarisagi/polaris/internal/observability/budget"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/observability/probe"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/credential"

	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/internal/swarm/agents"
	"github.com/polarisagi/polaris/internal/swarm/orchestrator"
	"github.com/polarisagi/polaris/internal/swarm/planner"
	"github.com/polarisagi/polaris/internal/swarm/supervisor"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// AgentBundle 持有 §8~§10.5 所有 Agent 层产物。
type AgentBundle struct {
	// Eval Harness
	EvalRunner *harness.RunnerImpl // 具体类型：InjectAgent/RunSuite 定义在 *RunnerImpl 上

	// Blackboard & Scheduler
	Blackboard    *orchestrator.SQLiteBlackboard
	Sched         *automation.SQLiteScheduler
	AgentRegistry *orchestrator.AgentRegistry
	Orch          *orchestrator.Orchestrator

	// Agent Kernel & DAG Executor
	Agent   *sysagent.Agent
	DAGExec *agentdag.DAGExecutor

	// M9 Self-Improvement
	M9Engine *learning.Engine

	// AgentPool for per-session web agents
	AgentPool *sysagent.Pool

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
) *sysagent.Agent {
	a := sysagent.NewAgent(sessionID, taskRepo, nil, sb.Router)
	a.SetExtQuerier(sb.Store.DB())
	a.Config.MaxReplan = sb.Cfg.Thresholds.M4Kernel.MaxReplanAttempts
	a.Config.DefaultBudget = sb.Cfg.Thresholds.M4Kernel.DefaultBudget
	a.Config.MaxSteps = sb.Cfg.Thresholds.M4Kernel.MaxSteps
	a.Config.IdleTimeoutSec = sb.Cfg.Thresholds.M4Kernel.SuspendIdleThresholdMin * 60
	a.InjectHITL(tb.HITLGateway)
	a.InjectToolRegistry(tb.ToolReg)
	a.InjectOutboxWriter(sb.Outbox)
	a.SetAssembler(agentctx.NewAssembler(epAdapter, knowAdapter))
	a.InjectPlannerSpawner(func(ctx context.Context, goal, taskType string, provider protocol.Provider) {
		whisperChan := a.GetWhisperChan()
		if whisperChan == nil {
			return
		}
		pool := planner.NewPlannerPool(goal, taskType, provider, whisperChan)
		pool.Run(ctx)
	})
	a.InjectExtensionActivator(&extensionActivatorAdapter{inner: tb.Activator})
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
	a.InjectTerminalCallback(func(_ context.Context, taskID, taskType string, replanCount int, success bool) {
		concurrent.SafeGo(bgCtx, "reflection-worker-"+sessionID, func(gctx context.Context) {
			if err := reflectionWorker.ConsolidateReflections(gctx, taskID, taskType, replanCount, success); err != nil {
				slog.Debug("polaris: reflection consolidation skipped", "task", taskID, "err", err)
			}
		})
	})
	return a
}

// bootAgent 执行 §8~§10.5 初始化，返回 Agent 层 bundle。
// Supervisor.Start() 故意留在 run() 中调用，以确保 defer Supervisor.Stop() 先行注册。
func bootAgent(ctx context.Context, sb *SubstrateBundle, mb *MemoryBundle, tb *ToolBundle, kb *KnowledgeBundle) (*AgentBundle, error) { //nolint:gocyclo
	// ─── §8 Eval Harness (L3 M12) ────────────────────────────────────────────
	evalAccessEngine := control.NewEngine(nil)
	evalStore := harness.NewSQLiteEvalStore(sb.Store, evalAccessEngine)
	evalRunner := harness.NewRunner(sb.Store, evalStore)

	// [Task 21] Setup LightweightRegressionDetector and inject into HITLGateway
	if tb.HITLGateway != nil {
		l3Detector := regression.NewLightweightRegressionDetector(sb.Store.DB())
		cooldown := time.Duration(sb.Cfg.Thresholds.M11Policy.L3ImprovementCooldownSeconds) * time.Second
		tb.HITLGateway.SetL3RegressionDeps(evalRunner, &regressionDetectorAdapter{inner: l3Detector}, cooldown)
	}

	slog.Info("polaris: eval harness initialized")

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
	piiVault := agentctx.NewSessionPIIVault(sb.Store.DB(), sb.Cfg.System.DataEncryptionKey, memory.NewMemoryFacade(memory.NewMemorySystemFromMemImpl(mb.Mem)))
	tb.RecoveryHandler.SetPIIVault(piiVault)

	// Reaper（挂起任务超时唤醒）
	reaperCtx, reaperStop := context.WithCancel(ctx)
	reaper := orchestrator.NewReaper(blackboard)
	//custom-nolint:bare-goroutine // 历史代码暂留，需结合上下文梳理 ctx 传递链路，后续重构替换
	go reaper.Run(reaperCtx)

	sched := automation.NewSQLiteScheduler(sb.Store)
	var memGuard *probe.OSMemoryGuard
	var featGate *probe.FeatureGate
	if sb.AutoConf != nil {
		memGuard = sb.AutoConf.Guard
		featGate = sb.AutoConf.Gate
	}
	sched.WithBackgroundGate(budget.NewResourceBudget(sb.TBR, memGuard, featGate))
	slog.Info("polaris: blackboard, scheduler, HITL gateway initialized")

	// ─── §9.5 M8 Multi-Agent Orchestrator ────────────────────────────────────
	agentRegistry := orchestrator.NewAgentRegistry()
	orch := orchestrator.NewOrchestrator(blackboard, agentRegistry, sb.Cfg.System.MaxAgents)

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

	agent := buildAgent("agent-0", sb, mb, tb, kb, taskRepo, epAdapter, knowAdapter, lamEngine, reflectionWorker, prefs, ctx)

	maxConcurrent := sb.Cfg.System.MaxAgents
	if maxConcurrent <= 0 {
		maxConcurrent = 4
	}

	agentPool := sysagent.NewPool(func(sessionID string) *sysagent.Agent {
		return buildAgent(sessionID, sb, mb, tb, kb, taskRepo, epAdapter, knowAdapter, lamEngine, reflectionWorker, prefs, ctx)
	}, maxConcurrent)

	agentRegistry.Register("agent-0", orchestrator.AgentCard{ //nolint:errcheck
		Name:   "agent-0",
		Skills: []string{"general"},
	}, agent)

	// 委托 tb.ToolReg.ExecuteTool（未注册工具由其内部 Lookup 显式拒绝，不静默放行），
	// 与 Dispatcher / Agent 直接 tool_call 共用同一条 PolicyGate→沙箱→执行 链路，不重复构造 ExecRequest。
	dagExec := agentdag.NewDAGExecutor(func(ctx context.Context, toolName string, args []byte, taintLevel types.TaintLevel) (*types.ToolResult, error) {
		return tb.ToolReg.ExecuteTool(ctx, toolName, args, taintLevel)
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
	rollout := optimizer.NewProgressiveRollout()
	rolloutBridge := reflexion.NewRolloutBridge(rollout)

	promptOptimizer := optimizer.NewPromptOptimizer(nil, nil, 0)
	m9Engine := learning.NewEngine(learning.DefaultEngineConfig(), reflexionBridge, curriculumBridge, rolloutBridge, taskEventCh, versionEventCh)
	// 2026-07-04 审计补齐（任务5）：SetDB 此前从未在生产启动代码中被调用，
	// 导致 learning_cursors 持久化/幂等去重整套机制在 e.db==nil 短路下形同虚设
	// （loadCursors 直接返回空 map，saveCursorAsync 第一行判空直接 return）。
	m9Engine.SetDB(sb.Store.DB())
	m9Engine.WithBackgroundGate(budget.NewResourceBudget(sb.TBR, memGuard, featGate))
	m9Engine.SetOptimizer(promptOptimizer)
	// M9 闭环接线（P1-2）：内环消费 Reflexion 启发式事件；SurpriseIndex 读 M3 权威值；
	// L3/L4 演化审批走 HITL 网关。
	m9Engine.SetHeuristicEvents(heuristicCh)
	m9Engine.SetSurpriseIndexProvider(func() float64 { return metrics.GlobalSurpriseIndex().Current() })
	m9Engine.SetHITLGateway(tb.HITLGateway)

	slog.Info("polaris: M9 self-improvement engine + PromptOptimizer initialized")

	bgTaskScheduler := curriculum.NewBackgroundTaskScheduler(curriculumGen, blackboard)
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

	// ─── §10.5 Supervisor Tree（仅注册 workers；Start() 由 run() 在注册 defer 后调用）
	sv := supervisor.NewSupervisor(5, 5*time.Minute)
	sv.AddWorker("agent-0", func(ctx context.Context) error {
		return agent.Run(ctx)
	})
	sv.AddWorker("orchestrator", func(ctx context.Context) error {
		return orch.ListenLoop(ctx)
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
		EvalRunner:    evalRunner,
		Blackboard:    blackboard,
		Sched:         sched,
		AgentRegistry: agentRegistry,
		Orch:          orch,
		Agent:         agent,
		DAGExec:       dagExec,
		M9Engine:      m9Engine,
		AgentPool:     agentPool,
		Supervisor:    sv,
		ReaperStop:    reaperStop,
	}, nil
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
		return "", fmt.Errorf("read notion token from preferences: %w", err)
	}
	if encrypted != "" {
		token, decErr := vault.Decrypt(encrypted)
		if decErr != nil {
			return "", fmt.Errorf("decrypt notion token: %w", decErr)
		}
		if token != "" {
			return token, nil
		}
	}

	token := os.Getenv("NOTION_TOKEN")
	if token == "" {
		return "", fmt.Errorf("NOTION_TOKEN not set and no vault-stored token found")
	}

	encToken, encErr := vault.Encrypt(token)
	if encErr != nil {
		return "", fmt.Errorf("encrypt notion token: %w", encErr)
	}
	if err := sysRepo.UpsertPreference(ctx, notionTokenPrefKey, encToken); err != nil {
		// 落盘失败不阻断本次调用——下次调用会再次读取 env 兜底，仅退化为
		// "每次都读明文 env"，不影响功能正确性，只是没有实现"落盘后免依赖 env"的优化。
		slog.Warn("polaris: failed to persist encrypted notion token, will re-read env next time", "err", err)
	}
	return token, nil
}
