// boot_tools.go — §6~§6.8 启动阶段：
// Sandbox 路由器 → ToolRegistry → MCP Manager → Skill Library →
// ConsolidationPipeline → ForgettingManager。
// ToolBundle 持有所有工具层产物，向 boot_knowledge/boot_agent/boot_server 传递。
package main

import (
	"github.com/polarisagi/polaris/internal/learning/curriculum"
	"github.com/polarisagi/polaris/internal/memory/consolidation"
	"github.com/polarisagi/polaris/internal/observability/budget"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/internal/observability/probe"

	"context"
	"encoding/json"
	"log/slog"
	"runtime"
	"time"

	memstore "github.com/polarisagi/polaris/internal/memory/store"

	"github.com/polarisagi/polaris/internal/action"
	"github.com/polarisagi/polaris/internal/agent"
	"github.com/polarisagi/polaris/internal/automation/hitl"
	"github.com/polarisagi/polaris/internal/extension/bus"
	"github.com/polarisagi/polaris/internal/extension/lifecycle"
	"github.com/polarisagi/polaris/internal/extension/marketplace"
	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/internal/extension/native"
	"github.com/polarisagi/polaris/internal/extension/skill"
	"github.com/polarisagi/polaris/internal/knowledge/connector"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/token"
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/internal/store/repo"
	polartool "github.com/polarisagi/polaris/internal/tool"
	"github.com/polarisagi/polaris/internal/tool/builtin"
	"github.com/polarisagi/polaris/internal/tool/catalog"
	toolsb "github.com/polarisagi/polaris/internal/tool/sandbox"
)

// ToolBundle 持有 §6~§6.8 所有工具层产物。
type ToolBundle struct {
	ContainerSandbox *sandbox.ContainerSandbox // 可 nil（<Tier1 或 FeatureL3Sandbox 未启用）
	InProcSandbox    *sandbox.InProcessSandbox
	SandboxRouter    *sandbox.SandboxRouter
	Envelope         *sandbox.ExecEnvelope
	ToolReg          *polartool.InMemoryToolRegistry
	MCPMgr           *mcp.MCPManager
	MktClient        *marketplace.MCPMarketplaceClient
	HITLGateway      *hitl.GatewayImpl
	SysRepo          *repo.SQLiteSystemRepository
	ExtRepo          *repo.SQLiteExtensionRepository
	AppRepo          *repo.SQLiteAppRepository
	InstallMgr       *marketplace.Manager
	InstallFSM       *lifecycle.InstallFSM
	SkillRegistry    protocol.SkillRegistry
	SkillExecutor    protocol.SkillExecutor   // ScriptSkillExecutor；注入 Agent FastPath（M4 System 1）
	NativeCogn       native.CognitiveSearcher // 可 nil（SurrealDB 未启用时）
	EmbedFn          native.EmbedFn           // 可 nil（Ollama 未启用时；ExtensionActivator 降级为纯 FTS）
	RecoveryHandler  *agent.ProviderRecoveryHandler
	Catalog          catalog.Catalog // 统一工具目录
	Activator        *native.ExtensionActivator
	ExtensionBus     *bus.ExtensionBus
}

// bootTools 执行 §6~§6.8 初始化，返回工具层 bundle。
func bootTools(ctx context.Context, sb *SubstrateBundle, mb *MemoryBundle) (*ToolBundle, error) { //nolint:gocyclo
	// ─── §6 Sandbox 路由器 (L1 M7) ──────────────────────────────────────────
	// CmdRunner：Rust FFI（bwrap/Seatbelt）→ Go 降级路径，跨平台统一执行层。
	// 替代原 Linux namespace（CLONE_NEWUSER/PID/NS）和 Firecracker/VZ/WSL2 多后端。
	cmdRunner := toolsb.NewWrapBashCmdRunner()

	var containerSandbox *sandbox.ContainerSandbox
	if sb.AutoConf != nil && sb.AutoConf.Gate.State(probe.FeatureL3Sandbox) != probe.FeatureDisabled {
		containerSandbox = sandbox.NewContainerSandbox(sb.AutoConf.Config.L3SandboxBackend, runtime.GOOS, sb.AutoConf.Config.Tier, cmdRunner)
		slog.Info("polaris: L3 container sandbox initialized (Rust native sandbox)",
			"backend", "rust_bwrap_seatbelt",
			"platform", runtime.GOOS)
	}
	inProcSandbox := sandbox.NewInProcessSandbox()
	// B4-F5: WasmtimeSandbox（L2）门控
	// FeatureL2Sandbox 未启用时（内存 < 512MB 或 Tier 低于要求），传 nil 给 SandboxRouter。
	// SandboxRouter 收到 nil wasmtimeSandbox 时，Wasm 工具降级到 InProcessSandbox。
	var wasmtimeSandbox *toolsb.WasmtimeSandbox
	if sb.AutoConf == nil || sb.AutoConf.Gate.State(probe.FeatureL2Sandbox) != probe.FeatureDisabled {
		wasmtimeSandbox = toolsb.NewWasmtimeSandbox(sb.Layout.Workspace)
		slog.Info("polaris: WasmtimeSandbox (L2) initialized")
	} else {
		slog.Info("polaris: WasmtimeSandbox (L2) skipped (FeatureL2Sandbox disabled)")
	}
	sandboxRouter := sandbox.NewSandboxRouter(inProcSandbox, containerSandbox, wasmtimeSandbox, runtime.GOOS, sb.Cfg.System.Tier)
	if sb.AutoConf != nil {
		sb.AutoConf.WithSandboxController(sandboxRouter)
	}
	envelope := sandbox.NewExecEnvelope(sb.Gate, sandboxRouter, sb.Cfg.System.Tier, runtime.GOOS, &inlineTokenVerifier{})
	slog.Info("polaris: sandbox router & envelope initialized", "os", runtime.GOOS, "tier", sb.Cfg.System.Tier)

	// ─── §6.3 内置工具注册 & MCP Manager ────────────────────────────────────
	// allowedPaths：DataDir 始终包含（Agent 工作区 + DB + 日志）。
	// sandbox.allowed_paths 追加用户自定义目录（项目路径等）。
	// 去重防止同一目录被 OS 沙箱重复绑定（bwrap --bind-try 重复无害但浪费）。
	allowedPaths := deduplicatePaths(append(
		[]string{sb.DataDir},
		sb.Cfg.Sandbox.AllowedPaths...,
	))
	toolReg := polartool.NewInMemoryToolRegistry(envelope)

	memoryCatalog := catalog.NewMemoryCatalog()

	mcpMgr := mcp.NewMCPManager(inProcSandbox, sb.SafeHTTP, sb.Gate)
	// MCP 工具注册时同步到 InMemoryToolRegistry，Agent Kernel FSM 可发现 MCP 工具
	mcpMgr.SetToolRegistrar(toolReg)
	mcpMgr.SetCatalog(memoryCatalog)
	mcpMgr.SetEnvelope(envelope)

	mktClient := marketplace.NewMCPMarketplaceClient("", sb.Layout.Extensions, sb.SafeHTTP)

	hitlGateway := hitl.NewGateway(sb.Store)
	hitlGateway.SetNotifier(hitl.NewChannelNotifier())
	sysRepo := repo.NewSQLiteSystemRepository(sb.Store.DB())
	prefsRepo := sysRepo
	extRepo := repo.NewSQLiteExtensionRepository(sb.Store.DB())
	appRepo := repo.NewSQLiteAppRepository(sb.Store.DB())
	installMgr := marketplace.NewManager(extRepo, mcpMgr, sb.Gate, prefsRepo, sb.AuditTrail, sb.TrustMap)
	if containerSandbox != nil {
		installMgr.WithHookRunner(containerSandbox)
	}

	cronRepo := repo.NewSQLiteCronRepository(sb.Store.DB())
	if err := builtin.RegisterBuiltinTools(inProcSandbox, toolReg, allowedPaths, sb.Dialer,
		sb.Cfg.Sandbox.Enabled,
		toolsb.NetworkPolicy(sb.Cfg.Sandbox.NetworkPolicy),
		sb.Cfg.Sandbox.BwrapPath,
		sb.Cfg,
		cronRepo,
	); err != nil {
		slog.Warn("polaris: builtin OS tool registration partial failure", "err", err)
	}

	// ─── §6.5 Skill Library (L1 M6) ─────────────────────────────────────────
	skillRegistry := skill.NewSQLiteRegistry(sb.Store.DB())

	if err := builtin.RegisterSkillTools(inProcSandbox, toolReg, skillRegistry, sb.Outbox); err != nil {
		slog.Warn("polaris: skill tool registration failed", "err", err)
	}

	if mb.Mem != nil {
		if semMem, ok := mb.Mem.Semantic().(*memstore.SemanticMem); ok && semMem != nil {
			if err := builtin.RegisterMemoryTools(inProcSandbox, toolReg, semMem, mb.Mem.Retriever()); err != nil {
				slog.Warn("polaris: memory tool registration failed", "err", err)
			}
		}
	}

	var nativeCogn native.CognitiveSearcher
	if sb.SurrealStore != nil {
		nativeCogn = nativeCognAdapter{s: sb.SurrealStore}
	}
	var nativeEmbedFn native.EmbedFn
	if sb.Embedder != nil {
		embedder := sb.Embedder // 捕获引用
		nativeEmbedFn = func(ctx context.Context, text string) ([]float32, error) {
			v := embedder.Embed(text)
			if v == nil {
				return nil, apperr.New(apperr.CodeInternal, "embed returned nil")
			}
			return v, nil
		}
	}

	// knowledge_search 依赖 knowledgeBase（L2 §7.6），此处先传 nil，待 bootKnowledge 完成后由
	// native.RegisterExtensionTool 补注（见 boot_knowledge.go §7.6 末尾）
	if err := native.RegisterExtensionTools(inProcSandbox, toolReg, mcpMgr, extRepo, mktClient, installMgr, hitlGateway, sb.Outbox, nativeCogn, nativeEmbedFn, nil); err != nil {
		slog.Warn("polaris: native extension tool registration partial failure", "err", err)
	}
	slog.Info("polaris: builtin tools registered, MCP manager initialized")

	// ─── GapFillWorker（M9 能力缺口探测，OutboxWorker handler）────────────
	gapFillWorker := curriculum.NewGapFillWorker(sb.Store.DB(), sb.Router, toolReg)
	sb.Outbox.RegisterHandler("m9_capability_gap", gapFillWorker.HandleOutbox)
	slog.Info("polaris: GapFillWorker registered to outbox for m9_capability_gap")

	// ─── M1 CircuitBreaker 恢复 handler ─────────────────────────────────────
	// vault/board 暂为 nil（启动时尚未装配），由 bootAgent 通过 SetBlackboard/SetPIIVault 热注入。
	recoveryHandler := agent.NewProviderRecoveryHandler(nil, nil)
	sb.Outbox.RegisterHandler("m1_provider_recovered", func(ctx context.Context, rec *store.OutboxRecord) error {
		return recoveryHandler.Handle(ctx, rec.Payload)
	})
	slog.Info("polaris: ProviderRecoveryHandler registered to outbox for m1_provider_recovered")

	// ─── E5+E6 语义压缩 & 扩展馆员 & Episodic 投影 handlers ─────────────────
	llmInfer := func(ctx context.Context, prompt string, opts ...types.InferOption) (string, error) {
		if sb.Router != nil {
			inferOpts := append([]types.InferOption{types.WithModel("reasoning")}, opts...)
			resp, err := sb.Router.Infer(ctx, []types.Message{{Role: "user", Content: prompt}}, inferOpts...)
			if err != nil {
				return "", err
			}
			return resp.Content, nil
		}
		return "", nil
	}
	semanticCompressHandler := consolidation.NewSemanticCompressHandler(sb.Store.DB(), protocol.LLMInferFunc(llmInfer), sb.Layout.Workspace)
	sb.Outbox.RegisterHandler("semantic_compress", semanticCompressHandler.Handle)

	var extCogn connector.SurrealWriterInterface = dummySurreal{}
	if sb.SurrealStore != nil {
		extCogn = &surrealCognAdapter{s: sb.SurrealStore}
	}
	extensionLibrarianHandler := connector.NewExtensionLibrarianHandler(sb.Store.DB(), extCogn, protocol.LLMInferFunc(llmInfer), nil)
	sb.Outbox.RegisterHandler("extension_librarian", extensionLibrarianHandler.Handle)

	sb.Outbox.RegisterHandler("episodic", consolidation.EpisodicProjectorHandler(sb.Store.DB(), sb.Cfg.System.DataEncryptionKey))
	slog.Info("polaris: SemanticCompressHandler, ExtensionLibrarianHandler and EpisodicProjectorHandler registered")

	// B4-F4: 热注入 SkillRegistry到 GapFillWorker
	// GapFillWorker 构造函数不接受 skillRegistry，通过 SetSkillRegistry 后注入解耦初始化顺序。
	gapFillWorker.SetSkillRegistry(skillRegistry)
	slog.Info("polaris: GapFillWorker.SkillRegistry injected (HE-6 State-in-DB now active)")
	skillSelector := skill.NewSelector(skillRegistry)
	_ = skillSelector

	// ─── [P1-FIX] M13-bis：注入运行时注册器 ──────────────────────────────────
	// installMgr 在上方创建时 skillRegistry 还未初始化，此处补注入。
	installFSM := lifecycle.NewInstallFSM(extRepo)
	installFSM.RegisterInstaller(lifecycle.NewMCPInstaller(extRepo, mcpMgr))
	installFSM.RegisterInstaller(lifecycle.NewPluginInstaller(extRepo, mcpMgr, skillRegistry))
	installFSM.RegisterInstaller(lifecycle.NewSkillInstaller(extRepo, skillRegistry))
	installFSM.RegisterInstaller(lifecycle.NewAppInstaller(extRepo))
	installMgr.WithInstallFSM(installFSM)
	slog.Info("polaris: InstallFSM injected into marketplace manager")

	// 启动时将 DB 中已有 tool-mode skills 批量同步到 InMemoryToolRegistry
	loadSkillsToToolRegistry(ctx, sb.Store.DB(), toolReg, inProcSandbox)

	skillExecutor := skill.NewScriptSkillExecutor(skillRegistry, nil, nil)
	skillReg := skillRegistry
	skillCatalog := catalog.NewSkillCatalog(skillReg)
	compCatalog := catalog.NewCompositeCatalog(memoryCatalog, skillCatalog)

	for _, t := range toolReg.List() {
		// Sync built-in to memoryCatalog
		memoryCatalog.Register(catalog.CatalogEntry{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
			Source:      t.Source,
			TrustTier:   t.TrustTier,
		})
	}

	slog.Info("polaris: skill library initialized (script-backed)")

	// ─── §6.6 ConsolidationPipeline（M5 §4 四阶段 Episodic→Semantic 蒸馏）──
	consolidationPipeline := consolidation.NewConsolidationPipelineFull(
		mb.Mem.Episodic(),
		mb.Mem.Semantic(),
		skillRegistry,
		sb.Router,
		mb.WriteFilter,
		mb.CascadeInvalidator,
		sb.Store.DB(),
	)
	var consolGuard *probe.OSMemoryGuard
	var consolGate *probe.FeatureGate
	if sb.AutoConf != nil {
		consolGuard = sb.AutoConf.Guard
		consolGate = sb.AutoConf.Gate
	}
	consolidationPipeline.WithBackgroundGate(budget.NewResourceBudget(sb.TBR, consolGuard, consolGate))
	sb.Outbox.RegisterHandler("memory_consolidate", func(ctx context.Context, rec *store.OutboxRecord) error {
		var payload struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(rec.Payload, &payload); err != nil {
			return nil //nolint:nilerr // malformed payload 跳过，避免 OutboxWorker 无限重试
		}
		if payload.SessionID == "" {
			return nil
		}

		return consolidationPipeline.Run(ctx, payload.SessionID)
	})
	slog.Info("polaris: memory consolidation pipeline registered (OutboxWorker/memory_consolidate)")

	// §6.8 PerMessageExtractor（每条消息立即提取实体，M5 准实时路径）──────────
	perMsgExtractor := consolidation.NewPerMessageExtractor(consolidationPipeline)
	sb.Outbox.RegisterHandler("episodic_extract", func(ctx context.Context, rec *store.OutboxRecord) error {
		return perMsgExtractor.HandleOutboxRecord(ctx, rec.Payload)
	})
	slog.Info("polaris: per-message extractor registered")

	// ─── §6.7 ForgettingManager（M5 §5 TTL 30d + Q-Learning 效用衰减 0.01/day）
	forgettingMgr := consolidation.NewForgettingManager(sb.Store, 0.01)
	coldArchiver := consolidation.NewColdArchiver(sb.Store)
	go func() {
		forgettingTicker := time.NewTicker(6 * time.Hour)
		defer forgettingTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-forgettingTicker.C:
				if err := forgettingMgr.PeriodicCleanup(); err != nil {
					slog.Warn("polaris: memory forgetting cleanup failed", "err", err)
				}
				if err := coldArchiver.PhysicalCompact(); err != nil {
					slog.Warn("polaris: cold archiver compact failed", "err", err)
				}
			}
		}
	}()
	slog.Info("polaris: memory forgetting manager started", "decay_rate", 0.01, "interval_h", 6)

	activator := native.NewExtensionActivator(extRepo, nativeCogn, mcpMgr, nativeEmbedFn)
	extensionBus := bus.New(installFSM, installMgr, activator, extRepo)

	return &ToolBundle{
		ContainerSandbox: containerSandbox,
		InProcSandbox:    inProcSandbox,
		SandboxRouter:    sandboxRouter,
		Envelope:         envelope,
		ToolReg:          toolReg,
		MCPMgr:           mcpMgr,
		MktClient:        mktClient,
		HITLGateway:      hitlGateway,
		SysRepo:          sysRepo,
		ExtRepo:          extRepo,
		AppRepo:          appRepo,
		InstallMgr:       installMgr,
		InstallFSM:       installFSM,
		SkillRegistry:    skillReg,
		SkillExecutor:    skillExecutor,
		NativeCogn:       nativeCogn,
		EmbedFn:          nativeEmbedFn,
		RecoveryHandler:  recoveryHandler,
		Catalog:          compCatalog,
		Activator:        activator,
		ExtensionBus:     extensionBus,
	}, nil
}

// deduplicatePaths 对路径列表去重（保持原顺序，忽略空串）。
// 防止 bwrap --bind-try 对同一目录重复绑定（无害但产生警告日志）。
func deduplicatePaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		if _, dup := seen[p]; !dup {
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	return out
}

type inlineTokenVerifier struct{}

func (v *inlineTokenVerifier) Verify(t *token.Token) error { return action.GetTokenManager().Verify(t) }

var _ sandbox.TokenVerifier = (*inlineTokenVerifier)(nil)
