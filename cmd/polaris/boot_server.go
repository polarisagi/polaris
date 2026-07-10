// boot_server.go — §11~§11.5 启动阶段：
// HTTP Server 装配 → LogicCollapseMonitor（P2-FIX）→ OTA 热更新管理器 → STT/TTS → Start。
// 返回 *server.Server 供 run() 执行 Shutdown 和 printStartupSummary。
package main

import (
	"path/filepath"

	"github.com/polarisagi/polaris/configs"
	"github.com/polarisagi/polaris/internal/agent"
	agentctx "github.com/polarisagi/polaris/internal/agent/context"
	"github.com/polarisagi/polaris/internal/channel"
	"github.com/polarisagi/polaris/internal/llm"
	"github.com/polarisagi/polaris/internal/observability/probe"
	"github.com/polarisagi/polaris/internal/prompt"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/network"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/internal/tool/dispatch"

	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"syscall"
	"time"

	"golang.org/x/time/rate"

	"github.com/polarisagi/polaris/internal/action"
	"github.com/polarisagi/polaris/internal/action/codeact"
	autopkg "github.com/polarisagi/polaris/internal/automation"
	extskill "github.com/polarisagi/polaris/internal/extension/skill"
	"github.com/polarisagi/polaris/internal/gateway/server"
	"github.com/polarisagi/polaris/internal/gateway/server/plugin"
	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin"
	si "github.com/polarisagi/polaris/internal/learning"
	swarmAgents "github.com/polarisagi/polaris/internal/swarm/agents"
	"github.com/polarisagi/polaris/internal/sysmgr/updater"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

var (
	// 用于单元测试注入 mock
	execFunc          = syscall.Exec        //nolint:gochecknoglobals
	exitFunc          = os.Exit             //nolint:gochecknoglobals
	loadProvidersFunc = LoadProvidersFromDB //nolint:gochecknoglobals
)

// bootServer 执行 §11~§11.5 初始化：装配 HTTP Server、OTA 管理器、STT/TTS，并调用 Start()。
// 返回 *server.Server，调用方 run() 负责 Shutdown。
func bootServer(ctx context.Context, sb *SubstrateBundle, tb *ToolBundle, ab *AgentBundle) (*server.Server, error) { //nolint:gocyclo
	if sb.Cfg.Security.LocalOnlyMode {
		slog.Info("polaris: initializing local_only network sandbox")
		ns := network.NewNetworkSandbox(100) // maxAllowlistSize = 100
		ns.SetSafeDialer(sb.Dialer)
		// 补充接线（2026-07-04 审计）：SetLocalProvider 此前从未被调用，导致
		// StartupCheck() 里的 Tier3 本地模型内存预算守卫（checkLocalModelMemoryBudget）
		// 因 ns.localProvider==nil 被静默跳过，纵深防御少了一层。此处补齐。
		provider, hasProvider := sb.InfReg.Get("llama-local")
		if lp, ok := provider.(protocol.LocalProvider); hasProvider && ok {
			ns.SetLocalProvider(lp)
		}
		// 顺序修复（2026-07-04 审计）：StartupCheck() 的 loopback-only 连通性自检
		// 前提是沙箱防护已生效（探测 8.8.8.8:53 应该被拒绝才算通过）。若先于
		// Enable() 调用，网络仍完全开放，探测必然拿到 SYN-ACK，StartupCheck
		// 会 100% 返回 "OS sandbox not effective" 错误，导致 local_only_mode=true
		// 时服务器永远无法启动。必须先 Enable() 激活三层防御，再 StartupCheck()
		// 验证防御确实生效。
		if err := ns.Enable(); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "failed to enable local_only network sandbox", err)
		}
		if err := ns.StartupCheck(); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "local_only startup check failed", err)
		}
		slog.Info("polaris: local_only network sandbox activated successfully")
	}

	addr := fmt.Sprintf("%s:%d", sb.Cfg.Interface.Host, sb.Cfg.Interface.Port)
	apiRateLimiter := rate.NewLimiter(rate.Limit(50), 100)
	mpData, _ := configs.FS.ReadFile("extensions/marketplaces.yaml")
	regData, _ := configs.FS.ReadFile("extensions/registry.yaml")

	httpServer := server.NewServer(addr, sb.DataDir, ab.AgentPool, ab.Blackboard, tb.HITLGateway,
		sb.Store.DB(), sb.Store.ReadDB(), sb.InfReg, sb.SafeHTTP, sb.Dialer, sb.Cfg.Compressor, sb.Cfg.Agent, sb.TBR, apiRateLimiter)
	promptMgr := prompt.NewManager(filepath.Join(sb.DataDir, "config"), configs.FS)
	httpServer.SetPromptManager(promptMgr)
	channelMgr := channel.NewManager(sb.SafeHTTP, func(channelType, channelID string, cfg map[string]any, msg protocol.ChannelMessage) {}, channel.WithSafeDialer(sb.Dialer))
	httpServer.SetChannelStarter(channelMgr)

	httpServer.SetAuditTrail(sb.AuditTrail)
	httpServer.SetLogStore(sb.LogStore)
	httpServer.SetToolRegistry(tb.ToolReg)
	httpServer.SetCatalog(tb.Catalog)
	httpServer.SeedBuiltinConfig(mpData, regData)
	httpServer.SetReloadProviders(func() {
		if err := loadProvidersFunc(context.Background(), sb.Store.DB(), sb.Vault, sb.InfReg, sb.SafeHTTP, sb.TBR); err != nil {
			slog.Error("polaris: failed to hot-reload providers", "err", err)
		}
	})
	httpServer.SetWorktreeManagerFactory(func(wd, r string) sysadmin.WorktreeManager { return autopkg.NewWorktreeManager(wd, r) })
	httpServer.SetSkillRegistry(tb.SkillRegistry)
	httpServer.SetEmbedder(sb.Embedder, sb.Cfg.Embedding.Threshold)
	httpServer.SetSyncSkillFunc(func(skillName, instructions string) {
		// Temporarily disabled in Phase 1, Phase 2 UnifiedToolCatalog will replace this.
	})

	// 设置插件同步向量索引器
	if sb.SurrealStore != nil {
		idx := plugin.NewEmbeddingIndexer(&pluginCognIndexAdapter{s: sb.SurrealStore}, sb.Embedder)
		httpServer.SetEmbeddingIndexer(idx)
	}

	// ─── Skill 签名密钥 ──────────────────────────────────────────────────────
	var skillSigningKey []byte
	if key := os.Getenv("POLARIS_SKILL_SIGNING_KEY"); key != "" { //nolint:nestif
		skillSigningKey = []byte(key)
	} else {
		if b, err := os.ReadFile(sb.Layout.SkillSignKey); err == nil && len(b) > 0 {
			skillSigningKey = b
		} else {
			h := sha256.Sum256(fmt.Appendf(nil, "polaris-local-%d", time.Now().UnixNano()))
			skillSigningKey = h[:]
			if err := os.WriteFile(sb.Layout.SkillSignKey, skillSigningKey, 0600); err != nil {
				slog.Warn("polaris: failed to write skill_signing.key", "err", err)
			}
		}
	}

	// ─── [B4-F2] CodeAct 引擎初始化（FeatureL3Sandbox 门控）───────────────────
	// CodeAct 强制依赖 Sbx-L3（ContainerSandbox）；L3 未启用时跳过。
	// 架构文档: docs/arch/M07-Tool-Action-Layer.md §7.4
	if tb.ContainerSandbox != nil {
		// GovernanceAgent 是 CodeAct 的 L1 代码安全校验网关（必须非 nil）。
		// policyGate：protocol.PolicyGate 接口；security/policy.Gate 通过 IsAuthorized 满足。
		govAgent, _ := swarmAgents.NewGovernanceAgent(sb.Gate, sb.Store.DB())

		// L2：SecurityAuditAgent 提供 LLM 语义审查（Prompt Injection 消毒 + ThinkingMax
		// 推理 + 结构化风险输出）。此前 CodeAct 的 WithASTChecker(L0)/WithPeerReviewer(L2)
		// 从未在生产环境被调用——三层校验实际上只有 L1(regex) 生效，L0(真实 AST 解析)和
		// L2(LLM 语义审查+HITL) 形同虚设。这里补齐，使三层审查真正全部生效。
		// hitl/timeout 参数已随 AuditAsync 死代码一并删除（2026-07-02）：SecurityAuditAgent
		// 现在只提供同步 ReviewSync，HITL 审批统一由下面 codeact.WithHITL(tb.HITLGateway) 处理。
		auditAgent := swarmAgents.NewSecurityAuditAgent(tb.LLMInfer, "zh")

		codeActEngine := codeact.NewCodeAct(
			tb.Envelope,
			nil, // toolExec 审计可选；nil 时跳过 RecordAudit 调用
			codeact.WithGovernanceAgent(&govAgentAdapter{inner: govAgent}),
			codeact.WithASTChecker(&codeact.DefaultASTChecker{}),
			codeact.WithPeerReviewer(&securityAuditReviewerAdapter{inner: auditAgent}),
			codeact.WithHITL(tb.HITLGateway),
			codeact.WithMaxCodeSize(sb.Cfg.Thresholds.M7Tool.MaxCodeSizeBytes),
			codeact.WithPIIGuard(tb.PIIDetector, tb.PIIDesensitizer),
		)
		// 通过 adapter 注入：agent 包依赖接口，不直接持有 *codeact.CodeAct
		ab.Agent.SetCodeAct(&codeActAdapter{inner: codeActEngine})
		// gateway 侧同样只依赖 action.ActionFacade 接口，不直接持有 *codeact.CodeAct（M04 §B2）
		httpServer.SetCodeActEngine(action.NewActionFacade(codeActEngine))
		slog.Info("polaris: CodeAct engine initialized and injected",
			"sandbox", "L3-container",
			"backend", sb.AutoConf.Config.L3SandboxBackend,
		)
	} else {
		slog.Info("polaris: CodeAct engine skipped (FeatureL3Sandbox disabled or ContainerSandbox nil)")
	}
	// ─── [/B4-F2] ────────────────────────────────────────────────────────────

	// ─── [P2-FIX] M9：LogicCollapseMonitor + StagingPipeline ────────────────
	// 在 skillSigningKey 初始化之后创建，确保编译器签名密钥可用。
	// FeatureLogicCollapse 门控：内存 < 1024MB 时跳过（避免在 Tier-0 低内存 VPS 加重负担）。
	logicCollapseEnabled := sb.AutoConf != nil &&
		sb.AutoConf.Gate.State(probe.FeatureLogicCollapse) != probe.FeatureDisabled

	if logicCollapseEnabled && tb.ContainerSandbox != nil {
		collapseCompilerTier := sb.AutoConf.Config.Tier
		collapseCompiler := extskill.NewLogicCollapseCompiler(extskill.LogicCollapseConfig{
			Gate:             extskill.NewCompileGate(collapseCompilerTier),
			CodeGen:          si.NewDefaultLLMCodeGenerator(sb.Router),
			Registry:         tb.SkillRegistry,
			WorkDir:          sb.Layout.Workspace,
			SigningKey:       skillSigningKey,
			ContainerSandbox: tb.ContainerSandbox,
		})
		collapseMonitor := si.NewLogicCollapseMonitor(
			collapseCompiler,
			si.NewDefaultLLMCodeGenerator(sb.Router),
			tb.SkillRegistry,
			&hitlNotifierAdapter{gateway: tb.HITLGateway},
			skillSigningKey,
			sb.Layout.Workspace,
		)
		// 2026-07-10：复用 bootAgent 构造的 ab.RolloutStore（与 M9Engine 共用同一份
		// rollout_states 状态），此前这里各自 new 一份 SQLiteRolloutStore，
		// 与 M9Engine 的候选状态互不相干，Gate 推进对不上号。
		if ab.RolloutStore != nil {
			collapseMonitor.WithStagingPipeline(ab.RolloutStore)
			slog.Info("polaris: StagingPipeline (shared with M9Engine) injected into LogicCollapseMonitor")
		} else {
			slog.Warn("polaris: RolloutStore unavailable, LogicCollapseMonitor L2 staging disabled")
		}
		ab.Agent.SetToolCallRecorder(&collapseRecorderAdapter{m: collapseMonitor})
		slog.Info("polaris: LogicCollapseMonitor injected as ToolCallRecorder into agent kernel",
			"feature", probe.FeatureLogicCollapse,
			"tier", collapseCompilerTier,
		)

		// [Task 10] 技能重生成监控
		if tb.ConsolidationPipeline != nil {
			skillEvolutionMonitor := extskill.NewSkillEvolutionMonitor(
				sb.Store.DB(),
				tb.SkillRegistry,
				collapseCompiler,
				&sb.Cfg.Thresholds,
			)
			tb.ConsolidationPipeline.WithSkillEvolver(skillEvolutionMonitor)
			slog.Info("polaris: SkillEvolutionMonitor injected into ConsolidationPipeline")
		}
	} else {
		slog.Info("polaris: LogicCollapseMonitor skipped (FeatureLogicCollapse disabled, AutoConf nil, or ContainerSandbox nil)")
	}
	// ─── [/P2-FIX] ───────────────────────────────────────────────────────────

	// ─── [Task 11] BudgetManager 接入主控制流 ───────────────────────────────────────
	// 创建会话级 BudgetManager 并注入 Agent 。
	// nil-safe：Agent.SetBudget(nil) 时内联 TokenBudget 逻辑仍有效（向后兑容）。
	budgetMgr := agent.NewBudgetManager()
	ab.Agent.SetBudget(budgetMgr)
	// MonthlyBudgetUSD：2026-07-04 审计修复（附录·任务11）——此前硬编码为 0，
	// 与 GET/PUT /v1/config/budget（internal/gateway/server/sysadmin/budget.go，
	// 持久化到 kv_store）完全断开：用户通过 API 设置的预算上限重启后丢失，
	// PUT 期间也不会热更新到运行中的 Agent。此处启动时从同一个 BudgetRepository
	// 读回持久化值；HandleSetBudget 侧的热更新见该文件改动。
	monthlyBudgetUSD := 0.0
	if sb.Store != nil && sb.Store.DB() != nil {
		budgetRepo := repo.NewSQLiteBudgetRepository(sb.Store.DB())
		if v, err := budgetRepo.GetBudget(ctx); err != nil {
			slog.Warn("polaris: failed to load persisted monthly budget, defaulting to unlimited", "err", err)
		} else {
			monthlyBudgetUSD = v
		}
	}
	ab.Agent.SetMonthlyBudgetUSD(monthlyBudgetUSD)
	slog.Info("polaris: BudgetManager initialized and injected into Agent", "monthly_budget_usd", monthlyBudgetUSD)
	// ─── [/Task 11] ──────────────────────────────────────────────────────────

	// ─── OTA 热更新管理器 ─────────────────────────────────────────────────────
	updMgr := updater.New(Version, CommitHash, BuildDate, sb.SafeHTTP)
	updMgr.StartBackgroundCheck(ctx, 2*time.Hour)
	updMgr.SetRestartFn(func() {
		performHotRestart(sb)
	})
	httpServer.SetUpdater(updMgr)

	// ─── 其余 Server 装配 ─────────────────────────────────────────────────────
	httpServer.SetInstallManager(tb.InstallMgr)
	httpServer.SetSkillSigningKey(skillSigningKey)
	httpServer.SetMCPManager(tb.MCPMgr)
	if tb.ContainerSandbox != nil {
		httpServer.SetScriptRunner(tb.ContainerSandbox)
	}
	httpServer.SetToolRegistry(tb.ToolReg)
	httpServer.SetCatalog(tb.Catalog)
	httpServer.SeedBuiltinConfig(mpData, regData)
	httpServer.SetReloadProviders(func() {
		if err := loadProvidersFunc(context.Background(), sb.Store.DB(), sb.Vault, sb.InfReg, sb.SafeHTTP, sb.TBR); err != nil {
			slog.Error("polaris: failed to hot-reload providers", "err", err)
		}
	})
	httpServer.SetWorktreeManagerFactory(func(wd, r string) sysadmin.WorktreeManager { return autopkg.NewWorktreeManager(wd, r) })
	toolStage := agentctx.NewToolStage(tb.Catalog, sb.Embedder)
	// (Optional) toolStage.WithCognitiveStore(...) if we had a SurrealDB client in sb
	httpServer.SetToolStage(toolStage)
	httpServer.SetSkillRegistry(tb.SkillRegistry)
	// Dispatcher 统一路由至 tb.ToolReg.ExecuteTool（builtin/mcp/native）或 tb.SkillExecutor（skill），
	// 与 Agent Kernel 使用的同一条 PolicyGate→沙箱→执行 链路，不再单独持有 envelope 副本。
	disp := dispatch.New(tb.Catalog, tb.ToolReg, tb.SkillExecutor)
	disp.Use(dispatch.SchemaValidateInterceptor())
	disp.Use(dispatch.AuditInterceptor(sb.AuditTrail))
	httpServer.SetToolExecutor(disp.Execute)
	httpServer.SetLogStore(sb.LogStore)
	httpServer.SetEvalRunner(ab.EvalRunner)
	httpServer.SetToolRefOffloader(tb.ToolRefOffloader)

	// ─── §11.5 STT/TTS 引擎初始化（FeatureLocalSTT 门控，异步下载，不阻塞启动）
	var sttGate *probe.FeatureGate
	if sb.AutoConf != nil {
		sttGate = sb.AutoConf.Gate
	}
	initSTTEngine(ctx, httpServer, sb.DataDir, sttGate, sb.SafeHTTP, sb.Cfg.Inference.STT)
	initTTSEngine(ctx, httpServer, sb.DataDir, sttGate, sb.SafeHTTP, sb.Cfg.Inference.TTS)

	// ─── §11.6 后台向量回填触发器 (Dynamic Embedding Backfill)
	if dyn, ok := sb.Embedder.(*llm.DynamicEmbedder); ok {
		concurrent.SafeGo(context.Background(), "boot_server.vector_backfill", func(ctx context.Context) {
			<-dyn.WaitReady()
			slog.Info("polaris: Dynamic Embedder ready, triggering background plugin vector backfill...")
			if httpServer.PluginHandler() != nil {
				_, err := httpServer.PluginHandler().SyncAllMarketplaces(ctx, true)
				if err != nil {
					slog.Warn("polaris: Background vector backfill encountered errors", "err", err)
				}
			}
		})
	}

	if err := httpServer.Start(); err != nil {
		slog.Error("polaris: failed to start HTTP server", "err", err)
		return nil, err
	}

	return httpServer, nil
}

func performHotRestart(sb *SubstrateBundle) {
	// syscall.Exec 完全替换进程镜像，Go runtime 不执行任何 defer/finalizer。
	// 关闭序列（与 graceful shutdown 保持一致）：
	//   1. stop()         — 取消主 ctx，dbWriter.Run() flush 残余批次并退出
	//   2. <-dbWriterDone — 确认 DatabaseWriter goroutine 完全退出
	//   3. store.Close()  — SQLite WAL checkpoint + 清理 .db-wal / .db-shm
	exe, _ := os.Executable()
	slog.Info("polaris: initiating graceful db shutdown before exec-restart")

	if sb != nil {
		sb.Stop()
		if sb.DBWriterDone != nil {
			select {
			case <-sb.DBWriterDone:
			case <-time.After(5 * time.Second):
				slog.Warn("polaris: dbWriter flush timeout during hot-restart, proceeding anyway")
			}
		}
		if sb.Store != nil {
			_ = sb.Store.Close()
		}
	}

	slog.Info("polaris: exec-restarting with new binary", "path", exe)
	if err := execFunc(exe, os.Args, os.Environ()); err != nil {
		slog.Error("polaris: hot-restart failed to exec new binary", "exe", exe, "args", os.Args, "err", err)
		exitFunc(1)
		return
	}
	exitFunc(0)
}
