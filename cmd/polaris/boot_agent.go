// boot_agent.go — §8~§10.5 启动阶段：
// Eval Harness → Blackboard → Agent Kernel → M9 Self-Improvement → Supervisor。
// AgentBundle 持有所有 Agent 层产物，向 run() 和 bootServer 传递。
//
// 注意：Supervisor.Start() 由 run() 在注册完 defer 后调用，确保 defer Supervisor.Stop()
// 在 Start() 之前已注册（避免提前清理）。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/action/lam"
	knowledgepkg "github.com/polarisagi/polaris/internal/knowledge"
	"github.com/polarisagi/polaris/internal/learning"
	"github.com/polarisagi/polaris/internal/learning/curriculum"
	"github.com/polarisagi/polaris/internal/learning/reflexion"
	"github.com/polarisagi/polaris/internal/prompt/optimizer"

	sysagent "github.com/polarisagi/polaris/internal/agent"
	agentctx "github.com/polarisagi/polaris/internal/agent/context"
	agentdag "github.com/polarisagi/polaris/internal/agent/dag"
	"github.com/polarisagi/polaris/internal/automation"
	"github.com/polarisagi/polaris/internal/eval/control"
	"github.com/polarisagi/polaris/internal/eval/harness"
	"github.com/polarisagi/polaris/internal/extension/native"
	"github.com/polarisagi/polaris/internal/extension/skill"
	si "github.com/polarisagi/polaris/internal/learning"
	"github.com/polarisagi/polaris/internal/observability/budget"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/observability/probe"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"

	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/internal/swarm/orchestrator"
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

	// Blackboard & Scheduler
	Blackboard    *orchestrator.SQLiteBlackboard
	Sched         *automation.SQLiteScheduler
	AgentRegistry *orchestrator.AgentRegistry
	Orch          *orchestrator.Orchestrator

	// Agent Kernel & DAG Executor
	Agent   *sysagent.Agent
	DAGExec *agentdag.DAGExecutor

	// M9 Self-Improvement
	M9Engine *si.Engine

	// Supervisor Tree（Workers 已注册；由 run() 调用 Start()）
	Supervisor *supervisor.Supervisor

	// ReaperStop：run() 在 shutdown 时显式调用（defer 也会再次调用，幂等）
	ReaperStop context.CancelFunc
}

// bootAgent 执行 §8~§10.5 初始化，返回 Agent 层 bundle。
// Supervisor.Start() 故意留在 run() 中调用，以确保 defer Supervisor.Stop() 先行注册。
func bootAgent(ctx context.Context, sb *SubstrateBundle, mb *MemoryBundle, tb *ToolBundle, kb *KnowledgeBundle) (*AgentBundle, error) { //nolint:gocyclo
	// ─── §8 Eval Harness (L3 M12) ────────────────────────────────────────────
	evalAccessEngine := control.NewEngine(nil)
	evalStore := harness.NewSQLiteEvalStore(sb.Store, evalAccessEngine)
	evalRunner := harness.NewRunner(sb.Store, evalStore)
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
		_ = sb.Outbox.Write(ctx, protocol.OutboxEntry{
			Operation: "killswitch_recovery",
			Payload:   []byte(`{"status": "recovered"}`),
		})
	})

	// 热注入 blackboard 到 ProviderRecoveryHandler（bootTools 时尚未装配）
	tb.RecoveryHandler.SetBlackboard(blackboard)
	piiVault := agentctx.NewSessionPIIVault(sb.Store.DB(), sb.Cfg.System.DataEncryptionKey, mb.Mem)
	tb.RecoveryHandler.SetPIIVault(piiVault)

	// Reaper（挂起任务超时唤醒）
	reaperCtx, reaperStop := context.WithCancel(ctx)
	reaper := orchestrator.NewReaper(blackboard)
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
	agent := sysagent.NewAgent("agent-0", taskRepo, nil, sb.Router)
	agent.SetExtQuerier(sb.Store.DB())
	agent.Config.MaxReplan = sb.Cfg.Thresholds.M4Kernel.MaxReplanAttempts
	agent.Config.DefaultBudget = sb.Cfg.Thresholds.M4Kernel.DefaultBudget
	agent.Config.MaxSteps = sb.Cfg.Thresholds.M4Kernel.MaxSteps
	agent.Config.IdleTimeoutSec = sb.Cfg.Thresholds.M4Kernel.SuspendIdleThresholdMin * 60
	agent.InjectHITL(tb.HITLGateway)
	agent.InjectToolRegistry(tb.ToolReg)
	agent.InjectOutboxWriter(sb.Outbox)

	epAdapter := &episodicMemAdapter{ep: mb.Mem.Episodic()}
	var knowAdapter agentctx.KnowledgeRetriever
	if kb.KnowledgeBase != nil {
		knowAdapter = &knowledgeAdapter{kb: kb.KnowledgeBase}
	}
	agent.SetAssembler(agentctx.NewAssembler(epAdapter, knowAdapter))

	// 注入 PlannerPool 构造器，打破 kernel↔swarm 循环依赖
	agent.InjectPlannerSpawner(func(ctx context.Context, goal, taskType string, provider protocol.Provider) {
		whisperChan := agent.GetWhisperChan()
		if whisperChan == nil {
			slog.Warn("spawn_planner: whisperChan is nil, PlannerPool 结果无法回传")
			return
		}
		pool := planner.NewPlannerPool(goal, taskType, provider, whisperChan)
		pool.Run(ctx)
	})

	// 构造 ExtensionActivator（需要 db + cognitive + mcpMgr）
	activator := native.NewExtensionActivator(tb.ExtRepo, tb.NativeCogn, tb.MCPMgr)
	agent.InjectExtensionActivator(&extensionActivatorAdapter{inner: activator})
	agent.InjectMemory(mb.Mem)
	if mb.Mem != nil {
		agent.SetMemoryInjector(mb.Mem)
	}

	// 注入 LAM (R3)：使用真实 provider + policy gate，并绑定到 Agent（非 dry-run 丢弃）
	lamEngine := lam.NewComputerUseEngine(
		lam.LAMConfig{Enabled: true, ResolverModel: "default-vlm"},
		sb.Router, // VLM provider（动作解析）
		nil,       // executor: 当前无 GUI 执行器，dry-run 模式
		sb.Gate,   // Cedar PolicyGate（deny-by-default）
	)
	agent.SetLAMEngine(lamEngine)

	if kb != nil && kb.KnowledgeBase != nil {
		agent.SetKnowledgeSearcher(&fsmKnowledgeAdapter{kb: kb.KnowledgeBase})
	}

	if prefs, err := tb.SysRepo.ListPreferences(ctx); err == nil {
		agent.SetPreferences(prefs)
	} else {
		slog.Warn("polaris: failed to load preferences on startup", "err", err)
	}

	agentRegistry.Register("agent-0", orchestrator.AgentCard{ //nolint:errcheck
		Name:   "agent-0",
		Skills: []string{"general"},
	}, agent)

	dagExec := agentdag.NewDAGExecutor(func(ctx context.Context, toolName string, args []byte, taintLevel types.TaintLevel) (*types.ToolResult, error) {
		tool, lerr := tb.ToolReg.Lookup(toolName) // 真实元数据：Source/TrustTier/Capability/RiskLevel/SideEffects
		if lerr != nil {
			return nil, apperr.Wrap(apperr.CodeNotFound, "dag_exec: tool lookup", lerr) // 未注册 → 显式拒绝，不静默放行
		}
		res, err := tb.Envelope.Execute(ctx, sandbox.ExecRequest{
			Principal: sandbox.PrincipalAgent, Kind: sandbox.KindToolExecute,
			Resource: tool.Name, TrustTier: tool.TrustTier, Tool: tool,
			Input: args, TaintLevel: taintLevel, // 透传输入污点，勿恒置 TaintNone
			CPUQuotaMs: int(tool.Timeout.Milliseconds()),
		})
		if err != nil {
			return nil, err
		}
		return &types.ToolResult{Success: res.Success, Output: res.Output, Error: res.Error,
			LatencyMs: res.LatencyMs, TaintLevel: res.TaintLevel, ImageParts: res.ImageParts}, nil
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
			agent.WithSkillCache(skillCache)
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
	taskEventCh := make(chan si.TaskCompleteEvent, 64)
	versionEventCh := make(chan si.VersionChangeEvent, 8)

	// 桥接 Blackboard 事件 → M9 TaskCompleteEvent（XR-14：所有后台 goroutine 必须走 SafeGo）
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
					select {
					case taskEventCh <- si.TaskCompleteEvent{
						TaskID:  ev.TaskID,
						Success: true,
					}:
					default:
					}
				case "task_failed":
					select {
					case taskEventCh <- si.TaskCompleteEvent{
						TaskID:  ev.TaskID,
						Success: false,
						Failure: si.FailureLogic,
					}:
					default:
					}
				}
			}
		}
	})

	reflexionEngine := reflexion.NewReflexionEngine(mb.FallacyPool, mb.Heuristics, nil)
	reflexionBridge := reflexion.NewReflexionBridge(reflexionEngine)
	idleDetector := curriculum.NewIdleDetector()
	curriculumGen := curriculum.NewAutoCurriculumGenerator(idleDetector, mb.FallacyPool, mb.Heuristics)
	curriculumBridge := reflexion.NewCurriculumBridge(curriculumGen, blackboard)
	rollout := optimizer.NewProgressiveRollout()
	rolloutBridge := reflexion.NewRolloutBridge(rollout)

	promptOptimizer := optimizer.NewPromptOptimizer(nil, nil, 0)
	m9Engine := learning.NewEngine(learning.DefaultEngineConfig(), reflexionBridge, curriculumBridge, rolloutBridge, taskEventCh, versionEventCh)
	m9Engine.WithBackgroundGate(budget.NewResourceBudget(sb.TBR, memGuard, featGate))
	m9Engine.SetOptimizer(promptOptimizer)

	slog.Info("polaris: M9 self-improvement engine + PromptOptimizer initialized")

	bgTaskScheduler := curriculum.NewBackgroundTaskScheduler(curriculumGen, blackboard)
	bgTaskScheduler.Start(ctx)
	slog.Info("polaris: AutoCurriculumGenerator background scheduler started")

	// 训练适配器使用记录（避免 unused 编译错误；后续 M9 流水线消费）
	_ = sb.QLoRA
	_ = sb.PRM
	_ = sb.Steering

	// ─── §10.5 Supervisor Tree（仅注册 workers；Start() 由 run() 在注册 defer 后调用）
	sv := supervisor.NewSupervisor(5, 5*time.Minute)
	sv.AddWorker("agent-0", func(ctx context.Context) error {
		return agent.Run(ctx)
	})
	sv.AddWorker("orchestrator", func(ctx context.Context) error {
		return orch.ListenLoop(ctx)
	})
	sv.AddWorker("m9-engine", func(ctx context.Context) error {
		return m9Engine.Run(ctx)
	})

	// kb 当前用于确认 RAG 已就绪，未来可向 agent 注入知识检索能力
	_ = kb

	return &AgentBundle{
		EvalRunner:    evalRunner,
		Blackboard:    blackboard,
		Sched:         sched,
		AgentRegistry: agentRegistry,
		Orch:          orch,
		Agent:         agent,
		DAGExec:       dagExec,
		M9Engine:      m9Engine,
		Supervisor:    sv,
		ReaperStop:    reaperStop,
	}, nil
}

type agentInvokerAdapter struct {
	agent *sysagent.Agent
}

func (a *agentInvokerAdapter) InvokeAgent(ctx context.Context, intent string, opts ...any) (string, error) {
	a.agent.SetTaskIntent([]byte(intent))
	err := a.agent.SendIntent(types.TriggerIntentReceived)
	return a.agent.AgentID(), err
}

type episodicMemAdapter struct {
	ep protocol.EpisodicMemory
}

func (a *episodicMemAdapter) Query(ctx context.Context, q string, maxTaint types.TaintLevel) ([]agentctx.ContextItem, error) {
	res, err := a.ep.Query(ctx, types.EpisodicQuery{Semantic: q, MaxTaintLevel: maxTaint, K: 10})
	if err != nil {
		return nil, err
	}
	var items []agentctx.ContextItem
	for _, r := range res {
		if ev, ok := r.Event.(*types.Event); ok && ev != nil {
			content := fmt.Sprintf("[%s] %s: %s", ev.CreatedAt.Format(time.RFC3339), ev.Type, string(ev.Payload))
			items = append(items, agentctx.ContextItem{
				Content:   content,
				Source:    "episodic",
				Relevance: r.Score,
				Taint:     ev.TaintLevel,
			})
		}
	}
	return items, nil
}

type knowledgeAdapter struct {
	kb *knowledgepkg.KnowledgeBase
}

func (a *knowledgeAdapter) Search(ctx context.Context, q string, depth int) ([]agentctx.ContextItem, error) {
	if a.kb == nil {
		return nil, nil
	}
	topK := 5
	if depth > 1 {
		topK = 10
	}
	req := knowledgepkg.KnowledgeBaseSearchRequest{
		Query:    q,
		TopK:     topK,
		TaintMax: int(types.TaintHigh),
	}
	res, err := a.kb.Search(ctx, req)
	if err != nil {
		return nil, err
	}
	items := make([]agentctx.ContextItem, 0, len(res))
	for _, ac := range res {
		content := ac.Primary.Content
		if ac.Parent != nil {
			content = ac.Parent.Content + "\n" + content
		}
		items = append(items, agentctx.ContextItem{
			Content:   content,
			Source:    "knowledge",
			Relevance: 1.0,
			Taint:     types.TaintLevel(ac.Primary.TaintLevel),
		})
	}
	for i := range items {
		items[i].Relevance = 1.0 / float64(i+1)
	}
	return items, nil
}
