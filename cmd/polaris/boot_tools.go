// boot_tools.go — §6~§6.8 启动阶段：
// Sandbox 路由器 → ToolRegistry → MCP Manager → Skill Library →
// ConsolidationPipeline → ForgettingManager。
// ToolBundle 持有所有工具层产物，向 boot_knowledge/boot_agent/boot_server 传递。
package main

import (
	"path/filepath"

	polartool "github.com/polarisagi/polaris/internal/tool"
	"github.com/polarisagi/polaris/internal/tool/dispatch"

	"github.com/polarisagi/polaris/internal/learning/curriculum"
	"github.com/polarisagi/polaris/internal/memory/consolidation"
	"github.com/polarisagi/polaris/internal/observability/budget"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/internal/observability/probe"

	"context"
	"encoding/json"
	"log/slog"
	"os"
	"runtime"
	"time"

	"github.com/polarisagi/polaris/internal/action"
	"github.com/polarisagi/polaris/internal/action/hook"
	"github.com/polarisagi/polaris/internal/agent"
	"github.com/polarisagi/polaris/internal/automation/hitl"
	"github.com/polarisagi/polaris/internal/extension/lifecycle"
	"github.com/polarisagi/polaris/internal/extension/marketplace"
	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/internal/extension/native"
	"github.com/polarisagi/polaris/internal/extension/skill"
	"github.com/polarisagi/polaris/internal/gateway/server/chat"
	"github.com/polarisagi/polaris/internal/knowledge/connector"
	"github.com/polarisagi/polaris/internal/memory"
	memretrieval "github.com/polarisagi/polaris/internal/memory/retrieval"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/guard"
	"github.com/polarisagi/polaris/internal/security/token"
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/internal/tool/builtin"
	"github.com/polarisagi/polaris/internal/tool/catalog"
	toolsb "github.com/polarisagi/polaris/internal/tool/sandbox"
	"github.com/polarisagi/polaris/internal/vfs"
)

// ToolBundle 持有 §6~§6.8 所有工具层产物。
type ToolBundle struct {
	ContainerSandbox      *sandbox.ContainerSandbox // 可 nil（<Tier1 或 FeatureL3Sandbox 未启用）
	InProcSandbox         *sandbox.InProcessSandbox
	SandboxRouter         *sandbox.SandboxRouter
	Envelope              *sandbox.ExecEnvelope
	ToolReg               *polartool.InMemoryToolRegistry
	MCPMgr                *mcp.MCPManager
	MktClient             *marketplace.MCPMarketplaceClient
	HITLGateway           *hitl.GatewayImpl
	SysRepo               *repo.SQLiteSystemRepository
	ExtRepo               *repo.SQLiteExtensionRepository
	AppRepo               *repo.SQLiteAppRepository
	InstallMgr            *marketplace.Manager
	InstallFSM            *lifecycle.InstallFSM
	SkillRegistry         protocol.SkillRegistry
	SkillExecutor         protocol.SkillExecutor // ScriptSkillExecutor；注入 Agent FastPath（M4 System 1）
	ConsolidationPipeline *consolidation.ConsolidationPipeline
	ToolRefOffloader      chat.ToolRefOffloader
	NativeCogn            native.CognitiveSearcher // 可 nil（SurrealDB 未启用时）
	EmbedFn               native.EmbedFn           // 可 nil（Ollama 未启用时；ExtensionActivator 降级为纯 FTS）
	PIIDesensitizer       *guard.PIIDesensitizer
	PIIDetector           *guard.PIIDetector
	PIITokenVault         *guard.PIITokenVault // M11 §5.4：会话级可逆 PII 令牌 vault，注入 ToolRegistry 供工具执行前还原
	KnowledgeConnRegistry *connector.Registry  // M10 Task17：MCP 知识源连接器注册表，boot_agent.go 消费 GetAll() 接入 SyncScheduler
	RecoveryHandler       *agent.ProviderRecoveryHandler
	Catalog               catalog.Catalog // 统一工具目录
	Activator             *native.ExtensionActivator
	// PolicyEvolver 工具自进化闭环（2026-07-12 补齐接线）：bootAgent 经
	// fsm.ToolHintProvider 消费其 BuildSystemHintBlock() 注入 System Prompt。
	PolicyEvolver *action.PolicyEvolver
	// LLMInfer 通用 LLM 推理闭包（封装 sb.Router），供 SemanticCompressHandler/
	// ExtensionLibrarianHandler/CodeAct SecurityAuditAgent(L2) 等多个消费方复用，
	// 避免每处各自重新实现一份"prompt string → sb.Router.Infer" 的桥接闭包。
	LLMInfer   protocol.LLMInferFunc
	Dispatcher *dispatch.Dispatcher
	// VFSWorkspace 供 boot_server.go 注入 CodeAct（临时脚本落盘，批次4 XR-11
	// 复核修复）等下游需要 VFS 隔离边界的消费方；bootTools 早于 bootServer 执行，
	// 构造顺序天然满足依赖。
	VFSWorkspace *vfs.WorkspaceManager
	// ExemptionVault HITL 豁免令牌共享存储（2026-07-14）：与 toolReg 出口污点检查、
	// hitlGateway 铸造端共享同一实例；boot_agent.go buildAgent 额外注入 Agent 作为
	// S_VALIDATE TaintGate 的 TaintReviewChecker（M11 §2.5 SanitizeByUserReview）。
	ExemptionVault *token.ExemptionVault
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
	// NativeOSSandbox（L4-native）：Rust bwrap/Seatbelt，无容器运行时依赖。
	// Tier-0（2GB VPS）上 FeatureL3Sandbox 未启用时作为 CodeAct 脚本执行后端。
	// 始终初始化（Rust dylib 已随二进制打包），与 FeatureL3Sandbox 门控无关。
	// 复用 cmdRunner（WrapBashCmdRunner）以规避 internal/sandbox ↔ internal/tool/sandbox 循环 import。
	nativeOSSandbox := sandbox.NewNativeOSSandbox(cmdRunner)
	sandboxRouter := sandbox.NewSandboxRouter(inProcSandbox, containerSandbox, wasmtimeSandbox, runtime.GOOS, sb.Cfg.System.Tier)
	sandboxRouter.WithNativeOS(nativeOSSandbox)
	// RemoteSandbox（Sbx-L4，可选非硬依赖，[Tier-0-Limit]）：仅在运营者显式配置
	// endpoint 并开启 enabled 时注入；未配置时 r.remote 保持 nil，SandboxRemote/
	// SandboxContainer 路由请求按 sandbox_router.go 既有 fallback 链降级，不阻塞启动。
	if sb.Cfg.Sandbox.Remote.Enabled {
		if sb.Cfg.Sandbox.Remote.Endpoint == "" {
			slog.Warn("polaris: remote sandbox (Sbx-L4) enabled but endpoint empty, skipping init")
		} else {
			authToken := sb.Cfg.Sandbox.Remote.AuthToken
			if authToken == "" {
				authToken = os.Getenv("POLARIS_REMOTE_SANDBOX_TOKEN")
			}
			remoteSandbox := sandbox.NewRemoteSandbox(
				sb.Cfg.Sandbox.Remote.Endpoint,
				authToken,
				sb.Cfg.Sandbox.Remote.TimeoutSec,
				nil, // nil → NewRemoteSandbox 内部使用 network.NewSafeHTTPClient（XR-06 出站网络安全要求）
			)
			sandboxRouter.WithRemote(remoteSandbox)
			slog.Info("polaris: remote sandbox (Sbx-L4) initialized", "endpoint", sb.Cfg.Sandbox.Remote.Endpoint)
		}
	}
	if sb.AutoConf != nil {
		sb.AutoConf.WithSandboxController(sandboxRouter)
	}
	envelope := sandbox.NewExecEnvelope(sb.Gate, sandboxRouter, sb.Cfg.System.Tier, runtime.GOOS, &inlineTokenVerifier{})
	slog.Info("polaris: sandbox router & envelope initialized", "os", runtime.GOOS, "tier", sb.Cfg.System.Tier)

	// PreToolUse/PostToolUse Hook 引擎（ADR-0015 §2.2）：从 ~/.polarisagi/polaris/hooks/hooks.yaml
	// （用户级）+ .polaris/hooks/hooks.yaml（项目级）加载，接入 ExecEnvelope 统一入口。
	// 配置缺失/为空不报错（ADR-0006 确定性降级）；YAML 语法错误则降级为空 Registry，不阻塞启动。
	hookRegistry, hookLoadErr := hook.GetDefaultRegistry()
	if hookLoadErr != nil {
		slog.Warn("polaris: hooks.yaml load failed, PreToolUse/PostToolUse hooks disabled this run", "err", hookLoadErr)
		hookRegistry, _ = hook.Load() // 空路径列表 → 空 Registry，Match 恒返回 nil
	}

	// 初始化 PII 护栏组件 (Task 8/20)
	piiDesens := guard.NewPIIDesensitizer()
	// PIITokenVault：会话级可逆令牌 vault（2026-07-04 审计修复：此前定义完整但
	// 从未被实例化/注入，是纯死代码）。与 piiDesens 一样按进程生命周期共享单例，
	// 保持与 PIIDesensitizer 一致的既有架构模式。
	piiVault := guard.NewPIITokenVault()
	var piiDetector *guard.PIIDetector
	if sb.AutoConf != nil && sb.AutoConf.Gate.State(probe.FeaturePresidioPII) != probe.FeatureDisabled {
		piiDetector = guard.NewPIIDetectorWithPresidio("http://localhost:3000/analyze", sb.SafeHTTP)
		slog.Info("polaris: PII detector initialized (Presidio sidecar)")
	} else {
		piiDetector = guard.NewPIIDetector()
		slog.Info("polaris: PII detector initialized (Go regex Tier 0)")
	}

	hookRunner := hook.NewRunner(hookRegistry, sb.Gate, envelope, piiDetector, piiDesens)
	envelope.SetHookFirer(hookRunner)
	slog.Info("polaris: PreToolUse/PostToolUse hook engine wired into ExecEnvelope")

	// ─── §6.3 内置工具注册 & MCP Manager ────────────────────────────────────
	// allowedPaths：DataDir 始终包含（Agent 工作区 + DB + 日志）。
	// sandbox.allowed_paths 追加用户自定义目录（项目路径等）。
	// 去重防止同一目录被 OS 沙箱重复绑定（bwrap --bind-try 重复无害但浪费）。
	allowedPaths := deduplicatePaths(append(
		[]string{sb.DataDir},
		sb.Cfg.Sandbox.AllowedPaths...,
	))
	toolReg := polartool.NewInMemoryToolRegistry(envelope)
	toolReg.WithTokenVault(piiVault)

	// M04 §3 出口污点检查 + HITL 豁免转义（2026-07-14 补齐，见 hitlGateway 构造处
	// SetExemptionVault 的配套注入）：sb.Gate 满足 polartool.TaintEgressChecker
	// 接口（CheckEgressWithExemption 方法），exemptionVault 是铸造/查询豁免令牌
	// 的唯一共享存储，两处必须指向同一个实例。
	exemptionVault := token.NewExemptionVault()
	toolReg.WithTaintEgressChecker(sb.Gate)
	toolReg.WithExemptionVault(exemptionVault)

	// Inject trajectory store event writer for tool call recording (Task 1)
	toolReg.WithSessionEventWriter(newStoreEventWriter(sb.Store))

	// 工具自进化闭环（2026-07-12 unwired-code-audit 补齐）：PolicyEvolver 此前
	// 完整实现（滑动窗口成功率统计 + 失败模式识别）但从未被构造，ExecuteTool
	// 结果也从未上报——闭环两端均是死代码。policyEvolverOutcomeAdapter 桥接
	// polartool.ToolOutcomeRecorder（consumer-side 接口，防 internal/tool 反向
	// 依赖 internal/action）→ PolicyEvolver.RecordOutcome；读侧（BuildSystemHintBlock）
	// 由 bootAgent 经 fsm.ToolHintProvider 直接结构化满足（方法签名一致，无需适配器）。
	policyEvolver := action.NewPolicyEvolver(0, 0)
	toolReg.WithOutcomeRecorder(&policyEvolverOutcomeAdapter{pe: policyEvolver})

	memoryCatalog := catalog.NewMemoryCatalog()

	mcpMgr := mcp.NewMCPManager(inProcSandbox, sb.SafeHTTP, sb.Gate)
	// MCP 工具注册时同步到 InMemoryToolRegistry，Agent Kernel FSM 可发现 MCP 工具
	mcpMgr.SetToolRegistrar(toolReg)
	mcpMgr.SetCatalog(memoryCatalog)
	mcpMgr.SetEnvelope(envelope)

	mktClient := marketplace.NewMCPMarketplaceClient("", sb.Layout.Extensions, sb.SafeHTTP)

	hitlGateway := hitl.NewGateway(sb.Store)
	hitlGateway.SetNotifier(hitl.NewChannelNotifier())
	// 与上方 toolReg.WithExemptionVault 共享同一个 exemptionVault 实例：
	// 铸造方（hitlGateway.Respond 审批通过时）与查询方（toolReg 下一次
	// ExecuteTool 出口污点检查）必须读写同一份存储。
	hitlGateway.SetExemptionVault(exemptionVault)

	// V-4 核实：解决启动期循环依赖，在 hitlGateway 初始化后通过 SetOnKillSwitch
	// 注入回 boot_substrate 阶段已实例化的 sb.Gate。
	sb.Gate.SetOnKillSwitch(func() {
		slog.Error("polaris: POLICY GATE HITL TRIGGERED — human review required",
			"component", "policy_gate",
			"action", "hitl_callback",
		)
		// 使用 adapters_security.go 中的 hitlNotifierAdapter 进行适配调用
		_ = (&hitlNotifierAdapter{gateway: hitlGateway}).NotifyHITL(context.Background(), "policy_gate", "Cedar evaluation failed continuously")
		sb.KS.ReportError()
	})
	sysRepo := repo.NewSQLiteSystemRepository(sb.Store.DB())
	prefsRepo := sysRepo
	extRepo := repo.NewSQLiteExtensionRepository(sb.Store.DB())
	appRepo := repo.NewSQLiteAppRepository(sb.Store.DB())

	// 注入网络审批存储：MCPManager 查询 preferences 表以决定 TrustTier<=2 MCP 的网络隔离策略。
	mcpMgr.SetNetApprovalStore(sysRepo)

	installMgr := marketplace.NewManager(extRepo, mcpMgr, sb.Gate, prefsRepo, sb.AuditTrail, sb.TrustMap)
	if containerSandbox != nil {
		installMgr.WithHookRunner(containerSandbox)
	}
	// mktInstallerAdapter：postInstallSteps 的文件下载分支此前因 WithInstaller
	// 从未调用而永久跳过（ADR-0051）；mktClient.Install 是完整实现，直接注入。
	installMgr.WithInstaller(&mktInstallerAdapter{client: mktClient})

	cronRepo := repo.NewSQLiteCronRepository(sb.Store.DB())
	if err := builtin.RegisterBuiltinTools(inProcSandbox, toolReg, allowedPaths, sb.Dialer,
		sb.Cfg.Sandbox.Enabled,
		sb.Cfg.Sandbox.NetworkPolicy,
		sb.Cfg.Sandbox.BwrapPath,
		sb.Cfg,
		cronRepo,
		sb.Layout.Workspace,
		&mcpAsyncTaskAdapter{inner: mcpMgr},
	); err != nil {
		slog.Warn("polaris: builtin OS tool registration partial failure", "err", err)
	}

	// ─── §6.5 Skill Library (L1 M6) ─────────────────────────────────────────
	skillRegistry := skill.NewSQLiteRegistry(sb.Store.DB())

	if err := builtin.RegisterSkillTools(inProcSandbox, toolReg, skillRegistry, sb.Outbox); err != nil {
		slog.Warn("polaris: skill tool registration failed", "err", err)
	}

	if mb.Mem != nil {
		// U-1：builtin.SemanticMemWriter 是 protocol.SemanticMemory 的方法子集，
		// 接口值结构子类型自动满足，直接传接口，禁止具体类型断言（P1-4 规则 3）。
		exclusiveWriter := memretrieval.NewExclusiveWriter(mb.Mem.Semantic(), mb.CascadeInvalidator, sb.Store.DB())
		// 复核修正（本轮审查）：此前此处额外注册 memory_prune 工具（A17），但 A17 的
		// 立项背景误判"全仓库搜索 Prune/prune 未命中任何已注册工具"，实际是遗漏了
		// 早已存在、语义完全相同的 memory_expire（M05 §5-bis / 00-Global-Dictionary.md
		// 已记录、受【防退化】保护的 Memory-Write-Tool 组成员）。二者对 LLM 可见时
		// 参数字段还不一致（entity_name vs name），存在被误选风险；且 memory_expire
		// 当时的实现有 bug（见 memory_tools_exec.go MakeMemoryExpireFn 注释）从未真正
		// 生效，才让审计误判其"不存在"。现已修复 memory_expire 直接调用
		// MarkEntityExpired，功能与原 memory_prune 完全等价，遂删除重复的
		// memory_prune 工具（含 internal/tool/builtin/memory_prune/ 整包）及其
		// Cedar 规则，memory_expire 保留为唯一入口。
		if err := builtin.RegisterMemoryTools(inProcSandbox, toolReg, exclusiveWriter, mb.Mem.Semantic(), mb.Mem.Retriever(), mb.Mem.Reflection(), mb.Mem.Working().CoreMemory()); err != nil {
			slog.Warn("polaris: memory tool registration failed", "err", err)
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
	if err := native.RegisterExtensionTools(inProcSandbox, toolReg, extRepo, mktClient, installMgr, hitlGateway, sb.Outbox, nativeCogn, nativeEmbedFn, nil); err != nil {
		slog.Warn("polaris: native extension tool registration partial failure", "err", err)
	}
	slog.Info("polaris: builtin tools registered, MCP manager initialized")

	// ─── GapFillWorker（M9 能力缺口探测，OutboxWorker handler）────────────
	gapFillWorker := curriculum.NewGapFillWorker(sb.Store.DB(), sb.Router, toolReg)
	sb.Outbox.RegisterHandler(protocol.TopicCapabilityGap, gapFillWorker.HandleOutbox)
	slog.Info("polaris: GapFillWorker registered to outbox for m9_capability_gap")

	// ─── M1 CircuitBreaker 恢复 handler ─────────────────────────────────────
	// vault/board 暂为 nil（启动时尚未装配），由 bootAgent 通过 SetBlackboard/SetPIIVault 热注入。
	recoveryHandler := agent.NewProviderRecoveryHandler(nil, nil)
	sb.Outbox.RegisterHandler(protocol.TopicProviderRecovered, func(ctx context.Context, rec *store.OutboxRecord) error {
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

	// 初始化 WorkspaceManager 与 ToolRefOffloader
	const workspaceMaxSize = 500 * 1024 * 1024 // Tier0 quota，来源：internal/vfs/workspace_manager.go §Tier0=500MB
	vfsWM := vfs.NewWorkspaceManager(sb.Layout.Workspace, workspaceMaxSize)
	toolRefOffloader := memory.NewToolRefOffloader(sb.Store.DB(), vfsWM)

	// GR-5-001 补线：bootMemory 早于 bootTools 执行（vfsWM 尚不存在），episodic
	// 层超限 Payload 落盘的 BlobOverflowWriter 此前从未真正接线。bootTools 已经
	// 持有 mb（bootMemory 的产物）作为参数，vfsWM 一旦构造完成即可原地补上，
	// 无需为此单独调整 bootSubstrate/bootMemory/bootTools 的既有执行顺序。
	mb.Mem.SetEpisodicBlobOverflowWriter(vfsWM)
	slog.Info("polaris: episodic BlobOverflowWriter wired to VFS workspace manager (GR-5-001)")

	semanticCompressHandler := consolidation.NewSemanticCompressHandler(sb.Store.DB(), protocol.LLMInferFunc(llmInfer), vfsWM)
	sb.Outbox.RegisterHandler(protocol.TopicSemanticCompress, semanticCompressHandler.Handle)

	var extCogn protocol.CognitiveSearcher = dummySurreal{}
	if sb.SurrealStore != nil {
		extCogn = &surrealCognAdapter{s: sb.SurrealStore}
	}
	extensionLibrarianHandler := connector.NewExtensionLibrarianHandler(sb.Store.DB(), extCogn, protocol.LLMInferFunc(llmInfer), nil)
	sb.Outbox.RegisterHandler(protocol.TopicExtensionLibrarian, extensionLibrarianHandler.Handle)

	sb.Outbox.RegisterHandler(protocol.TopicEpisodicProject, consolidation.EpisodicProjectorHandler(sb.Store.DB(), []byte(sb.Cfg.System.DataEncryptionKey)))
	slog.Info("polaris: SemanticCompressHandler, ExtensionLibrarianHandler and EpisodicProjectorHandler registered")

	// B4-F4: 热注入 SkillRegistry到 GapFillWorker
	// GapFillWorker 构造函数不接受 skillRegistry，通过 SetSkillRegistry 后注入解耦初始化顺序。
	gapFillWorker.SetSkillRegistry(skillRegistry)
	slog.Info("polaris: GapFillWorker.SkillRegistry injected (HE-6 State-in-DB now active)")
	var skillSelector protocol.SkillSelector
	if sb.SurrealStore != nil {
		skillSelector = skill.NewHybridRetriever(skillRegistry, sb.SurrealStore, skill.EmbedFn(nativeEmbedFn))
	} else {
		skillSelector = skill.NewHybridRetriever(skillRegistry, nil, nil)
	}
	_ = skillSelector

	// ─── [P1-FIX] M13-bis：注入运行时注册器 ──────────────────────────────────
	// installMgr 在上方创建时 skillRegistry 还未初始化，此处补注入。
	// knowledgeConnRegistry（2026-07-04 审计补齐，任务17）：实例化一次，注入
	// MCPInstaller（capability=knowledge-source 的 MCP 服务器安装时写入），
	// 并通过 ToolBundle 传给 boot_agent.go 在启动时遍历 GetAll() 接入
	// SyncScheduler——此前只有"注册"没有"调度"两端接线均已就绪但从未打通。
	knowledgeConnRegistry := connector.NewRegistry()
	installFSM := lifecycle.NewInstallFSM(extRepo)
	installFSM.RegisterInstaller(lifecycle.NewMCPInstaller(extRepo, mcpMgr).WithRegistry(knowledgeConnRegistry))
	installFSM.RegisterInstaller(lifecycle.NewPluginInstaller(extRepo, mcpMgr, skillRegistry))
	// [W-2-B] 接入 SkillValidationPipeline
	signingKey := []byte(sb.Cfg.System.DataEncryptionKey)
	// WithMaxCodeSize 2026-07-21 deadcode 审查修复：该 Option 从未被传入，
	// maxCodeSize 恒为零值导致 Validate() 内的大小校验被跳过（技能代码尺寸完全不设限）。
	// 复用 codeact 侧已有的同一份配置阈值（boot_server.go 的 M7Tool.MaxCodeSizeBytes,
	// 默认 16384），而非发明一个新阈值。
	pipeline := skill.NewSkillValidationPipeline(signingKey, &scriptExecutorAdapter{sbx: containerSandbox},
		skill.WithMaxCodeSize(sb.Cfg.Thresholds.M7Tool.MaxCodeSizeBytes))
	installFSM.RegisterInstaller(lifecycle.NewSkillInstaller(extRepo, skillRegistry).WithValidators(
		&pipelineValidatorAdapter{pipeline: pipeline},
		&pipelineValidatorAdapter{pipeline: pipeline},
	))
	installFSM.RegisterInstaller(lifecycle.NewAppInstaller(extRepo))
	installMgr.WithInstallFSM(installFSM)
	slog.Info("polaris: InstallFSM injected into marketplace manager")

	// ScriptSkillExecutor 是技能执行的唯一实现（tool-mode instructions 渲染 / Logic Collapse
	// 脚本执行均在此完成，见 internal/extension/skill/skill.go）。
	// runner 注入 ContainerSandbox（Rust bwrap/Seatbelt 统一沙箱，物理隔离由其内部提供）；
	// Tier0 或 FeatureL3Sandbox 未启用时 containerSandbox 为 nil，ExecuteSkill 内部自动降级为
	// instructions-only（不执行脚本）。WithPolicy 注入 Cedar Gate，脚本执行前强制走 PolicyGate
	// 授权检查，deny-by-default。
	var skillRunner skill.ScriptRunner
	if containerSandbox != nil {
		skillRunner = containerSandbox
	}
	skillExecutor := skill.NewScriptSkillExecutor(skillRegistry, skillRunner, nil).WithPolicy(sb.Gate)

	// 启动时将 DB 中已有 tool-mode skills 批量同步到 InMemoryToolRegistry + InProcessSandbox，
	// 注册的执行函数委托至 skillExecutor（唯一实现，禁止重复渲染 instructions）。
	loadSkillsToToolRegistry(ctx, sb.Store.DB(), toolReg, inProcSandbox, skillExecutor)

	skillReg := skillRegistry
	skillCatalog := catalog.NewSkillCatalog(skillReg)
	compCatalog := catalog.NewCompositeCatalog(memoryCatalog, skillCatalog)
	if meta, err := polartool.GetBuiltinToolMeta("tool_search"); err == nil {
		inProcSandbox.Register(meta.Name, polartool.MakeToolSearchFn(compCatalog, sb.Embedder))
		_ = toolReg.Register(meta)
	} else {
		slog.Warn("polaris: failed to load tool_search meta", "err", err)
	}

	compCatalog.Embedder = sb.Embedder
	compCatalog.LazyLoadThreshold = sb.Cfg.Thresholds.M13Interface.LazyLoadToolThreshold

	for _, t := range toolReg.List() {
		// Sync built-in to memoryCatalog
		memoryCatalog.Register(protocol.CatalogEntry{
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
		consolidation.NewDefaultSummarizer(sb.Router, sb.PromptMgr),
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
	sb.Outbox.RegisterHandler(protocol.TopicMemoryConsolidate, func(ctx context.Context, rec *store.OutboxRecord) error {
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
	sb.Outbox.RegisterHandler(protocol.TopicEpisodicExtract, func(ctx context.Context, rec *store.OutboxRecord) error {
		return perMsgExtractor.HandleOutboxRecord(ctx, rec.Payload)
	})
	slog.Info("polaris: per-message extractor registered")

	var cogn protocol.CognitiveSearcher
	if sb.SurrealStore != nil {
		cogn = &surrealCognAdapter{s: sb.SurrealStore}
	}
	forgettingMgr := consolidation.NewForgettingManager(sb.Store, cogn, 0.01)
	coldArchiver := consolidation.NewColdArchiver(sb.Store)

	coldDBDir := filepath.Join(sb.DataDir, "cold")
	eventArchiver := consolidation.NewEventArchiver(sb.Store.DB(), sb.Cfg.Thresholds.M2Storage.EventlogWarmDays, coldDBDir,
		sb.Cfg.Thresholds.M2Storage.EventlogDiskWatermarkPct).
		WithRowSizeLimits(
			sb.Cfg.Thresholds.M2Storage.EventlogHotRowLimit,
			sb.Cfg.Thresholds.M2Storage.EventlogHotSizeMB,
		)
	concurrent.SafeGo(ctx, "boot_tools.memory_forgetting", func(ctx context.Context) {
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
				if err := eventArchiver.Archive(context.Background()); err != nil {
					slog.Warn("polaris: event archiver failed", "err", err)
				}
			}
		}
	})
	slog.Info("polaris: memory forgetting manager started", "decay_rate", 0.01, "interval_h", 6)

	activator := native.NewExtensionActivator(extRepo, nativeCogn, mcpMgr, nativeEmbedFn)

	disp := dispatch.New(compCatalog, toolReg, skillExecutor)
	disp.Use(dispatch.SchemaValidateInterceptor())
	disp.Use(dispatch.AuditInterceptor(sb.AuditTrail))

	return &ToolBundle{
		ContainerSandbox:      containerSandbox,
		InProcSandbox:         inProcSandbox,
		SandboxRouter:         sandboxRouter,
		Envelope:              envelope,
		ToolReg:               toolReg,
		MCPMgr:                mcpMgr,
		MktClient:             mktClient,
		HITLGateway:           hitlGateway,
		SysRepo:               sysRepo,
		ExtRepo:               extRepo,
		AppRepo:               appRepo,
		InstallMgr:            installMgr,
		InstallFSM:            installFSM,
		SkillRegistry:         skillReg,
		SkillExecutor:         skillExecutor,
		ConsolidationPipeline: consolidationPipeline,
		ToolRefOffloader:      toolRefOffloader,
		NativeCogn:            nativeCogn,
		EmbedFn:               nativeEmbedFn,
		PIIDesensitizer:       piiDesens,
		PIIDetector:           piiDetector,
		PIITokenVault:         piiVault,
		KnowledgeConnRegistry: knowledgeConnRegistry,
		RecoveryHandler:       recoveryHandler,
		Catalog:               compCatalog,
		Activator:             activator,
		PolicyEvolver:         policyEvolver,
		LLMInfer:              protocol.LLMInferFunc(llmInfer),
		Dispatcher:            disp,
		VFSWorkspace:          vfsWM,
		ExemptionVault:        exemptionVault,
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

type scriptExecutorAdapter struct {
	sbx *sandbox.ContainerSandbox
}

func (a *scriptExecutorAdapter) ExecuteTest(ctx context.Context, scriptBytes []byte, input []byte) ([]byte, error) {
	if a.sbx == nil {
		return nil, apperr.New(apperr.CodeInternal, "container sandbox not available")
	}
	tmpFile, err := os.CreateTemp("", "skill_test_*.js")
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "failed to create temp file for test skill", err)
	}
	defer os.Remove(tmpFile.Name())
	if _, err := tmpFile.Write(scriptBytes); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "failed to write test skill script", err)
	}
	tmpFile.Close()
	res, err := a.sbx.RunScript(ctx, "test_skill", tmpFile.Name(), input, types.TrustCommunity)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "failed to run test skill script", err)
	}
	return res, nil
}

type pipelineValidatorAdapter struct {
	pipeline *skill.SkillValidationPipeline
}

func (a *pipelineValidatorAdapter) Analyze(code []byte) (bool, []string, error) {
	res, err := a.pipeline.Validate(code, 0)
	if err != nil {
		return false, nil, apperr.Wrap(apperr.CodeInternal, "pipeline validation failed", err)
	}
	return res.Passed, nil, nil
}

func (a *pipelineValidatorAdapter) Assess(code []byte) (int, int) {
	res, err := a.pipeline.Validate(code, 0)
	if err != nil {
		return 1, 3 // default to medium/3 on error
	}
	return res.RiskLevel, res.SandboxTier
}
