// boot_server.go — §11~§11.5 启动阶段：
// HTTP Server 装配 → LogicCollapseMonitor（P2-FIX）→ OTA 热更新管理器 → STT/TTS → Start。
// 返回 *server.Server 供 run() 执行 Shutdown 和 printStartupSummary。
package main

import (
	agentctx "github.com/polarisagi/polaris/internal/agent/context"
	"github.com/polarisagi/polaris/internal/llm"
	"github.com/polarisagi/polaris/internal/observability/probe"
	"github.com/polarisagi/polaris/internal/tool/dispatch"

	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"syscall"
	"time"

	"github.com/polarisagi/polaris/internal/prompt/optimizer"

	"golang.org/x/time/rate"

	"github.com/polarisagi/polaris/internal/action/codeact"
	extskill "github.com/polarisagi/polaris/internal/extension/skill"
	"github.com/polarisagi/polaris/internal/gateway/server"
	"github.com/polarisagi/polaris/internal/gateway/server/plugin"
	si "github.com/polarisagi/polaris/internal/learning"
	swarmAgents "github.com/polarisagi/polaris/internal/swarm/agents"
	"github.com/polarisagi/polaris/internal/sysmgr/updater"
)

// bootServer 执行 §11~§11.5 初始化：装配 HTTP Server、OTA 管理器、STT/TTS，并调用 Start()。
// 返回 *server.Server，调用方 run() 负责 Shutdown。
func bootServer(ctx context.Context, sb *SubstrateBundle, tb *ToolBundle, ab *AgentBundle) (*server.Server, error) { //nolint:gocyclo
	addr := fmt.Sprintf("%s:%d", sb.Cfg.Interface.Host, sb.Cfg.Interface.Port)
	apiRateLimiter := rate.NewLimiter(rate.Limit(50), 100)
	httpServer := server.NewServer(addr, sb.DataDir, ab.AgentPool, ab.Blackboard, tb.HITLGateway,
		sb.Store.DB(), sb.InfReg, sb.SafeHTTP, sb.Dialer, sb.Cfg.Compressor, sb.TBR, apiRateLimiter)
	httpServer.SetAuditTrail(sb.AuditTrail)
	httpServer.SetLogStore(sb.LogStore)
	httpServer.SetToolRegistry(tb.ToolReg)
	httpServer.SetCatalog(tb.Catalog)
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
		)
		// 通过 adapter 注入：agent 包依赖接口，不直接持有 *codeact.CodeAct
		ab.Agent.SetCodeAct(&codeActAdapter{inner: codeActEngine})
		httpServer.SetCodeActEngine(codeActEngine)
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
		rolloutStore, rolloutStoreErr := optimizer.NewSQLiteRolloutStore(sb.Store.DB())
		if rolloutStoreErr != nil {
			slog.Warn("polaris: failed to init SQLiteRolloutStore, L2 staging disabled", "err", rolloutStoreErr)
		} else {
			collapseMonitor.WithStagingPipeline(rolloutStore)
			slog.Info("polaris: StagingPipeline injected into LogicCollapseMonitor")
		}
		ab.Agent.SetToolCallRecorder(&collapseRecorderAdapter{m: collapseMonitor})
		slog.Info("polaris: LogicCollapseMonitor injected as ToolCallRecorder into agent kernel",
			"feature", probe.FeatureLogicCollapse,
			"tier", collapseCompilerTier,
		)
	} else {
		slog.Info("polaris: LogicCollapseMonitor skipped (FeatureLogicCollapse disabled, AutoConf nil, or ContainerSandbox nil)")
	}
	// ─── [/P2-FIX] ───────────────────────────────────────────────────────────

	// ─── OTA 热更新管理器 ─────────────────────────────────────────────────────
	updMgr := updater.New(Version, CommitHash, BuildDate, sb.SafeHTTP)
	updMgr.StartBackgroundCheck(ctx, 2*time.Hour)
	updMgr.SetRestartFn(func() {
		// syscall.Exec 完全替换进程镜像，Go runtime 不执行任何 defer/finalizer。
		// 关闭序列（与 graceful shutdown 保持一致）：
		//   1. stop()         — 取消主 ctx，dbWriter.Run() flush 残余批次并退出
		//   2. <-dbWriterDone — 确认 DatabaseWriter goroutine 完全退出
		//   3. store.Close()  — SQLite WAL checkpoint + 清理 .db-wal / .db-shm
		exe, _ := os.Executable()
		slog.Info("polaris: initiating graceful db shutdown before exec-restart")
		sb.Stop()
		select {
		case <-sb.DBWriterDone:
		case <-time.After(5 * time.Second):
			slog.Warn("polaris: dbWriter flush timeout during hot-restart, proceeding anyway")
		}
		_ = sb.Store.Close()
		slog.Info("polaris: exec-restarting with new binary", "path", exe)
		_ = syscall.Exec(exe, os.Args, os.Environ())
		os.Exit(0)
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
	toolStage := agentctx.NewToolStage(tb.Catalog, sb.Embedder)
	// (Optional) toolStage.WithCognitiveStore(...) if we had a SurrealDB client in sb
	httpServer.SetToolStage(toolStage)
	httpServer.SetSkillRegistry(tb.SkillRegistry)
	// Dispatcher 统一路由至 tb.ToolReg.ExecuteTool（builtin/mcp/native）或 tb.SkillExecutor（skill），
	// 与 Agent Kernel 使用的同一条 PolicyGate→沙箱→执行 链路，不再单独持有 envelope 副本。
	disp := dispatch.New(tb.Catalog, tb.ToolReg, tb.SkillExecutor)
	disp.Use(dispatch.AuditInterceptor(sb.AuditTrail))
	httpServer.SetToolExecutor(disp.Execute)
	httpServer.SetLogStore(sb.LogStore)
	httpServer.SetEvalRunner(ab.EvalRunner)

	// ─── §11.5 STT/TTS 引擎初始化（FeatureLocalSTT 门控，异步下载，不阻塞启动）
	var sttGate *probe.FeatureGate
	if sb.AutoConf != nil {
		sttGate = sb.AutoConf.Gate
	}
	httpServer.InitSTTEngine(ctx, sb.DataDir, sttGate, sb.SafeHTTP, sb.Cfg.Inference.STT)
	httpServer.InitTTSEngine(ctx, sb.DataDir, sttGate, sb.SafeHTTP, sb.Cfg.Inference.TTS)

	// ─── §11.6 后台向量回填触发器 (Dynamic Embedding Backfill)
	if dyn, ok := sb.Embedder.(*llm.DynamicEmbedder); ok {
		go func() {
			<-dyn.WaitReady()
			slog.Info("polaris: Dynamic Embedder ready, triggering background plugin vector backfill...")
			if httpServer.PluginHandler() != nil {
				_, err := httpServer.PluginHandler().SyncAllMarketplaces(context.Background(), true)
				if err != nil {
					slog.Warn("polaris: Background vector backfill encountered errors", "err", err)
				}
			}
		}()
	}

	if err := httpServer.Start(); err != nil {
		slog.Error("polaris: failed to start HTTP server", "err", err)
		return nil, err
	}

	return httpServer, nil
}
