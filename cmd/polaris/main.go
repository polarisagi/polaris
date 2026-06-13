// polaris main entry point.
// 启动序列：配置加载 → 存储初始化 → 策略引擎 → 推理路由器 → 认知核心 → 协同层 → 调度器/HITL → 信号监听优雅退出。
// 架构文档: docs/arch/ARCHITECTURE.md §3 启动顺序
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/polarisagi/polaris/configs"
	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/pkg/action"
	"github.com/polarisagi/polaris/pkg/action/tool"
	polartool "github.com/polarisagi/polaris/pkg/action/tool"
	"github.com/polarisagi/polaris/pkg/cognition/kernel"
	"github.com/polarisagi/polaris/pkg/cognition/memory"
	"github.com/polarisagi/polaris/pkg/cognition/skill"
	"github.com/polarisagi/polaris/pkg/edge/hitl"
	"github.com/polarisagi/polaris/pkg/edge/scheduler"
	"github.com/polarisagi/polaris/pkg/extensions/marketplace"
	"github.com/polarisagi/polaris/pkg/extensions/mcp"
	"github.com/polarisagi/polaris/pkg/extensions/native"
	"github.com/polarisagi/polaris/pkg/gateway/server"
	"github.com/polarisagi/polaris/pkg/governance/eval"
	"github.com/polarisagi/polaris/pkg/substrate"
	"github.com/polarisagi/polaris/pkg/substrate/downloader"
	"github.com/polarisagi/polaris/pkg/substrate/inference"
	"github.com/polarisagi/polaris/pkg/substrate/observability"
	"github.com/polarisagi/polaris/pkg/substrate/policy"
	"github.com/polarisagi/polaris/pkg/substrate/storage"
	"github.com/polarisagi/polaris/pkg/substrate/updater"
	"github.com/polarisagi/polaris/pkg/swarm"
	"github.com/polarisagi/polaris/pkg/swarm/agents"
	knowledgepkg "github.com/polarisagi/polaris/pkg/swarm/knowledge"
	si "github.com/polarisagi/polaris/pkg/swarm/self_improve"
	"github.com/polarisagi/polaris/pkg/swarm/supervisor"
)

type dummySurreal struct{}

func (d dummySurreal) FTSIndex(docID, text string) error                               { return nil }
func (d dummySurreal) VecUpsert(id string, embedding []float32) error                  { return nil }
func (d dummySurreal) GraphRelate(fromID, edgeType, toID string, weight float64) error { return nil }

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "polaris: fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error { //nolint:gocyclo
	// ─── 0. 子命令分发 ──────────────────────────────────────────────────────
	if len(os.Args) > 1 {
		switch os.Args[1] {
		// CLI 客户端命令（不启动服务，直接与运行中的 server 通信）
		case "init", "setup":
			return runInit()
		case "chat":
			return runChatCmd(os.Args[2:])
		case "status":
			return runCLIStatus()
		case "export":
			return runExport(os.Args[2:])
		case "import":
			return runImport(os.Args[2:])
		case "config":
			return runConfigCmd(os.Args[2:])
		case "version", "--version", "-v":
			fmt.Printf("polaris v%s\n", cliVersion())
			return nil
		case "help", "--help", "-h":
			printCLIHelp()
			return nil
		// 内部子命令（原有）
		case "benchmark-routing":
			return runBenchmarkRouting(os.Args[2:])
		case "migrate":
			if len(os.Args) > 2 && os.Args[2] == "openclaw" {
				return runMigrateOpenClaw(os.Args[3:])
			}
		case "memory":
			if len(os.Args) > 2 && os.Args[2] == "process-staging" {
				return runProcessStaging()
			}
		}
	}

	// ─── 0. 信号监听 ────────────────────────────────────────────────────────
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// KillSwitch 提前声明，供 TripleCtrlCGuard goroutine 捕获（nil 安全）
	var ks *substrate.KillSwitch

	// TripleCtrlCGuard：独立信道监听 SIGINT，实现三击触发 KillSwitch FullStop
	sigintCh := make(chan os.Signal, 8)
	signal.Notify(sigintCh, syscall.SIGINT)
	go func() {
		for range sigintCh {
			if ks != nil {
				ks.OnSIGINT()
			}
		}
	}()

	// ─── 0.5 内核完整性校验 (L4) ────────────────────────────────────────────
	if err := config.VerifyKernelIntegrity(); err != nil {
		return errors.Wrap(errors.CodeInternal, "CRITICAL: kernel integrity compromised", err)
	}

	// ─── 1. 配置加载 ────────────────────────────────────────────────────────
	cfgPath := os.Getenv("POLARIS_CONFIG")
	if cfgPath != "" {
		// 显式配置路径缺失 → fail-fast，避免掩盖运维挂载问题
		if _, statErr := os.Stat(cfgPath); os.IsNotExist(statErr) {
			return errors.New(errors.CodeInternal,
				"POLARIS_CONFIG file not found: "+cfgPath)
		}
	} else {
		home, _ := os.UserHomeDir()
		cfgPath = filepath.Join(home, ".polarisagi/polaris", "config.toml")
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return errors.Wrap(errors.CodeInternal, "config.Load", err)
	}
	slog.Info("polaris: config loaded", "tier", cfg.System.Tier, "max_agents", cfg.System.MaxAgents)

	// ─── 1.1 下载代理配置（必须在任何下载操作前调用）─────────────────────────────
	// 优先级：POLARIS_GITHUB_PROXY 环境变量 > download.github_proxy 配置项 > 自动探测
	downloader.Configure(cfg.Download.GithubProxy)

	// ─── 0.3 数据目录解析与初始化 ─────────────────────────────────────────────
	dataDir, err := resolveDataDirBase(cfg)
	if err != nil {
		return err
	}
	layout := config.NewDataLayout(dataDir, cfg.System.Dirs)
	if err := layout.MkdirAll(); err != nil {
		slog.Warn("polaris: failed to create data directories", "err", err)
	}
	layout.Migrate() // 旧路径文件迁移到规范位置（幂等）

	// ─── 0.3.5 Thresholds 覆盖加载 ────────────────────────────────────────────
	thresholds, err := config.LoadThresholds(dataDir)
	if err != nil {
		return errors.Wrap(errors.CodeInternal, "config.LoadThresholds", err)
	}
	cfg.Thresholds = *thresholds

	// ─── 0.35 KillSwitch 初始化 ────────────────────────────────────────────────
	// 启动前先检查封印文件：若存在则拒绝启动（FullStop 状态跨重启持久化）
	if substrate.IsFullStopFilePresent(dataDir) {
		return errors.New(errors.CodeInternal,
			"system is sealed (.fullstop exists in "+dataDir+"); remove the file to restart")
	}
	ks = substrate.NewKillSwitch(dataDir)
	// 同步到 observability 全局原子量（供 M13 handleStatus 读取）
	ks.StateChangeCallback = func(newState substrate.KillState, _ string) {
		observability.GlobalKillswitchStage.Store(int32(newState))
	}

	// ─── 0.4 日志初始化（stdout + ~/.polarisagi/polaris/logs/polaris.log）─────────────
	if logFile := observability.SetupLogger(dataDir); logFile != nil {
		defer logFile.Close()
	}
	// 用 LogStore 包裹全局 handler，获取日志供前端 SSE 流
	logStore := server.NewLogStore(slog.Default().Handler(), 500)
	slog.SetDefault(slog.New(logStore))
	slog.Info("polaris: logger initialized", "data_dir", dataDir)

	// ─── 0.5 硬件探针 → Tier 判定 → FeatureGate ───────────────────────────
	autoConf, err := observability.NewAutoConfig()
	if err != nil {
		slog.Warn("polaris: AutoConfig failed, using Tier0 defaults", "err", err)
	} else {
		slog.Info("polaris: hardware probed",
			"tier", autoConf.Config.Tier,
			"ram_mb", autoConf.Config.TotalRAMMB,
			"cpu_cores", autoConf.Config.CPUCores,
		)
	}

	// ─── 0.6 全局指标生命周期 ──────────────────────────────────────────────────
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				observability.GlobalTokenBurnRate.Tick()
			}
		}
	}()

	// ─── 0.7 内存压力监控（每 5s 轮询，驱动 FeatureGate 运行时降级）──────────
	go autoConf.RunMemoryWatcher(ctx)
	slog.Info("polaris: memory pressure monitor started", "poll_interval_s", 5)

	// ─── 2. 存储初始化 (L0 基础设施) ─────────────────────────────────────────
	store, err := storage.OpenSQLite(layout.SQLiteDB, schema.FS)
	if err != nil {
		return errors.Wrap(errors.CodeInternal, "storage.OpenSQLite", err)
	}
	defer store.Close()
	slog.Info("polaris: storage initialized", "db", layout.SQLiteDB)

	// ─── 2.5 SurrealDB Core 认知存储（FeatureSurrealDBCore 门控）──
	// 后端由 [cognition] surreal_backend 控制：mem（默认，256MB+ 可用）或 rocksdb（持久化）。
	// 向量维度由 [inference] embedder_dim 控制，需与嵌入模型一致。
	// Tokio 线程数由 [cognition] surreal_worker_threads 控制（0=auto，VPS 建议设 2）。
	surrealStore := initSurrealStore(autoConf, cfg, layout)

	// ─── 2.6 StorageRouter（三轴统一路由）────────────────────────────────────
	var surrealProto protocol.Store
	if surrealStore != nil {
		surrealProto = surrealStore
	}
	storageRouter := substrate.NewStorageRouter(store, surrealProto)
	slog.Info("polaris: storage router initialized")

	// ─── 2.7 OutboxWorker（跨引擎投影）──────────────────────────────────────
	outboxWorker := substrate.NewOutboxWorker(store.DB(), 5, 3)
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		var cursor int64
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				batch, bErr := outboxWorker.FetchBatch(ctx, cursor, 20)
				if bErr != nil {
					slog.Warn("polaris: outbox fetch", "err", bErr)
					continue
				}
				for _, rec := range batch {
					if pErr := outboxWorker.Process(ctx, rec); pErr != nil {
						slog.Warn("polaris: outbox process", "err", pErr, "id", rec.ID)
					}
					if rec.ID > cursor {
						cursor = rec.ID
					}
				}
			}
		}
	}()
	slog.Info("polaris: outbox worker started", "poll_interval_s", 5)

	// ─── 2.8 DatabaseWriter（AI 核心数据单写者）──────────────────────────────────
	// 所有 AI 认知数据（events/decision_log）的写操作经此总线串行化 + 批量提交。
	// 配置类数据（channels/preferences/cron）属独立关注点，直接走 store.DB()，不经此总线。
	// 消费者：SQLiteEventLog / SQLiteDecisionLog / EventWriteBuffer / GraphWriter。
	dbWriter := substrate.NewDatabaseWriter(store.DB(), nil)
	dbWriterDone := make(chan struct{})
	go func() {
		dbWriter.Run(ctx) // ctx 取消时自动 flush + 退出
		close(dbWriterDone)
	}()
	eventLog := substrate.NewSQLiteEventLog(dbWriter)
	decisionLog := substrate.NewSQLiteDecisionLog(dbWriter)
	_ = eventLog    // 待 M4 Agent Kernel 注入（事件持久化）
	_ = decisionLog // 待 M3 观测层注入（决策审计）
	slog.Info("polaris: mutation bus (database writer) started")

	// ─── 2.9 AuditTrail 初始化 ─────────────────────────────────────────────────
	auditTrail := substrate.NewAuditTrail(store.DB(), layout.AuditArchive)
	if err := auditTrail.RecoverOnStartup(); err != nil {
		slog.Error("polaris: AuditTrail recovery failed", "err", err)
		return err
	}
	slog.Info("polaris: audit trail recovered and initialized")

	// ─── 月度成本报告 (M13 §1.1) ──────────────────────────────────────────────────
	scheduler.StartMonthlyCostReport(ctx, layout.Reports, store.DB())

	// ─── 3. 策略引擎 (L0 PolicyGate) ─────────────────────────────────────────
	gate := policy.NewGate(func() {
		// HITL prompt logic placeholder...
	})
	slog.Info("polaris: policy gate initialized (deny-by-default)")

	// ─── 3.5 信任发布者白名单 ────────────────────────────────────────────────
	publisherTrustMap := config.LoadTrustedPublishers(configs.FS, "extensions/trusted-publishers.yaml")

	// ─── 4. 推理路由器 (L0 M1) ───────────────────────────────────────────────
	dialer := substrate.NewSafeDialer()
	safeHTTPClient := substrate.NewSafeHTTPClient(dialer)
	inference.SetDefaultHTTPClient(safeHTTPClient)

	reg := inference.NewProviderRegistry()
	// env var 中的 API Key 写入 DB（INSERT OR IGNORE），由 LoadProvidersFromDB 统一加载。
	// 禁止 env var 直接注册内存 registry，DB 是唯一权威源。
	server.SeedProvidersFromEnv(ctx, store.DB())

	// ─── 4.5 本地推理（Ollama，FeatureLocalInference 门控）─────────────────────
	var embedder substrate.Embedder
	var qloraAdapter *inference.QLoRAAdapter
	var prmAdapter *inference.PRMAdapter
	var steeringAdapter *inference.SteeringAdapter
	if autoConf != nil { //nolint:nestif
		if autoConf.Gate.State(observability.FeatureLocalInference) != observability.FeatureDisabled {
			localModel := autoConf.Config.LocalModelID
			if localModel == "" {
				localModel = "llama3.2"
			}
			reg.Register("ollama-local", "Local LLM", inference.NewOllamaAdapter(localModel, safeHTTPClient))
			slog.Info("polaris: Ollama local inference registered", "model", localModel)
		}
		// ─── 4.6 本地嵌入（Ollama，FeatureLocalEmbedding 门控）──────────────────
		if autoConf.Gate.State(observability.FeatureLocalEmbedding) != observability.FeatureDisabled {
			embedModel := autoConf.Config.LocalEmbeddingModel
			if embedModel == "" {
				embedModel = "nomic-embed-text"
			}
			embedder = inference.NewOllamaEmbeddingAdapter(embedModel, safeHTTPClient)
			slog.Info("polaris: Ollama embedding registered", "model", embedModel)
		}
		// ─── 4.7 训练适配器（FeatureQLoRA / FeaturePRMTraining 门控）────────────
		if autoConf.Gate.State(observability.FeatureQLoRA) != observability.FeatureDisabled {
			qloraAdapter = inference.NewQLoRAAdapter("", safeHTTPClient)
			slog.Info("polaris: QLoRA training adapter initialized")
		}
		if autoConf.Gate.State(observability.FeaturePRMTraining) != observability.FeatureDisabled {
			prmAdapter = inference.NewPRMAdapter("", safeHTTPClient)
			slog.Info("polaris: PRM training adapter initialized")
		}
		// ─── 4.8 激活引导（FeatureActivationSteer 门控）─────────────────────────
		if autoConf.Gate.State(observability.FeatureActivationSteer) != observability.FeatureDisabled {
			steeringAdapter = inference.NewSteeringAdapter("", safeHTTPClient)
			slog.Info("polaris: activation steering adapter initialized")
		}
		// ─── 4.9 大型本地模型（FeatureLargeLocalLLM 门控，Tier2+，7B+ 量化模型）──
		if autoConf.Gate.State(observability.FeatureLargeLocalLLM) != observability.FeatureDisabled {
			if largeModel, ok := observability.TierLocalModel(autoConf.Config.Tier); ok {
				reg.Register("ollama-large", "Large Local LLM", inference.NewOllamaAdapter(largeModel, safeHTTPClient))
				slog.Info("polaris: large local LLM registered", "model", largeModel)
			}
		}
	}

	router := inference.NewInferenceRouter(reg, dialer)

	// ─── 4.10 M5 记忆系统初始化（SurrealDB 可用时走全路径，否则 Tier0 降级）──
	var mem *memory.MemImpl
	if surrealStore != nil {
		// Tier1+：FTS+HNSW+Graph+SQL 全路径
		cogn := &surrealCognAdapter{s: surrealStore}
		mem = memory.NewMemImplFull(store, surrealStore, cogn, store)
		slog.Info("polaris: memory initialized with SurrealDB cognitive axis (Tier1+)")
	} else {
		// Tier0 降级：纯 Go BM25 + SQLite BLOB 余弦
		mem = memory.NewMemImplWithDB(store, store)
		slog.Info("polaris: memory initialized in Tier0 mode (SQLite-only, SurrealDB disabled)")
	}
	slog.Info("polaris: inference router and memory initialized")

	// ─── 5. MEMF + 启发式记忆（M9 内环基础）────────────────────────────────────
	fallacyPool := swarm.NewFallacyMemoryPool(store.DB())
	heuristics := swarm.NewHeuristicsMemory(store.DB())
	slog.Info("polaris: MEMF and heuristics memory initialized")

	// ─── 6. Sandbox 路由器 (L1 M7) ───────────────────────────────────────────
	var containerSandbox *action.ContainerSandbox
	if autoConf != nil && autoConf.Gate.State(observability.FeatureL3Sandbox) != observability.FeatureDisabled {
		containerSandbox = action.NewContainerSandbox(autoConf.Config.L3SandboxBackend, runtime.GOOS, autoConf.Config.Tier)
		slog.Info("polaris: L3 container sandbox initialized", "backend", autoConf.Config.L3SandboxBackend)
	}
	inProcSandbox := action.NewInProcessSandbox()
	wasmtimeSandbox := tool.NewWasmtimeSandbox(layout.Workspace)
	// 内置工具直接信任，走 InProcess；Wasm 走 Wasmtime；LLM 生成代码/插件脚本走 Container 隔离。
	sandboxRouter := action.NewSandboxRouter(inProcSandbox, containerSandbox, wasmtimeSandbox, runtime.GOOS, cfg.System.Tier)
	slog.Info("polaris: sandbox router initialized", "os", runtime.GOOS, "tier", cfg.System.Tier)

	// ─── 6.3 内置工具注册 & MCP Manager ─────────────────────────────────────
	allowedPaths := []string{dataDir}
	toolReg := polartool.NewInMemoryToolRegistry(nil)
	mcpMgr := mcp.NewMCPManager(inProcSandbox, safeHTTPClient, gate)

	mktClient := marketplace.NewMCPMarketplaceClient("", layout.Extensions)

	hitlGateway := hitl.NewGateway(store)
	prefsRepo := server.NewSQLPreferencesRepo(store.DB())
	installMgr := marketplace.NewManager(store.DB(), mcpMgr, gate, prefsRepo, auditTrail, publisherTrustMap)

	if err := tool.RegisterBuiltinTools(inProcSandbox, toolReg, allowedPaths, dialer,
		cfg.Sandbox.Enabled,
		tool.NetworkPolicy(cfg.Sandbox.NetworkPolicy),
		cfg.Sandbox.BwrapPath,
		cfg,
		store.DB(),
	); err != nil {
		slog.Warn("polaris: builtin OS tool registration partial failure", "err", err)
	}
	var nativeCogn native.CognitiveSearcher
	if surrealStore != nil {
		nativeCogn = nativeCognAdapter{s: surrealStore}
	}
	if err := native.RegisterExtensionTools(inProcSandbox, toolReg, mcpMgr, store.DB(), mktClient, installMgr, hitlGateway, outboxWorker, nativeCogn, nil); err != nil {
		slog.Warn("polaris: native extension tool registration partial failure", "err", err)
	}
	slog.Info("polaris: builtin tools registered, MCP manager initialized")

	gapFillWorker := swarm.NewGapFillWorker(store.DB(), router, toolReg)
	outboxWorker.RegisterHandler("m9_capability_gap", gapFillWorker.HandleOutbox)
	slog.Info("polaris: GapFillWorker registered to outbox for m9_capability_gap")

	// M1 CircuitBreaker 恢复 handler：Provider 恢复上线后唤醒被挂起的 Task。
	// vault/board 暂为 nil（启动时尚未装配），后续通过 SetDeps 热注入。
	recoveryHandler := kernel.NewProviderRecoveryHandler(nil, nil)
	outboxWorker.RegisterHandler("m1_provider_recovered", func(ctx context.Context, rec *substrate.OutboxRecord) error {
		return recoveryHandler.Handle(ctx, rec.Payload)
	})
	slog.Info("polaris: ProviderRecoveryHandler registered to outbox for m1_provider_recovered")

	// 注册 E5+E6 Handlers
	var llmInfer agents.LLMInferFunc = func(ctx context.Context, prompt string, opts ...protocol.InferOption) (string, error) {
		if router != nil {
			inferOpts := append([]protocol.InferOption{protocol.WithModel("reasoning")}, opts...)
			resp, err := router.Infer(ctx, []protocol.Message{{Role: "user", Content: prompt}}, inferOpts...)
			if err != nil {
				return "", err
			}
			return resp.Content, nil
		}
		return "", nil
	}
	semanticCompressHandler := agents.NewSemanticCompressHandler(store.DB(), llmInfer, "~/.polarisagi/polaris/data/vfs/")
	outboxWorker.RegisterHandler("semantic_compress", semanticCompressHandler.Handle)

	extensionLibrarianHandler := agents.NewExtensionLibrarianHandler(store.DB(), dummySurreal{}, llmInfer, nil)
	outboxWorker.RegisterHandler("extension_librarian", extensionLibrarianHandler.Handle)
	slog.Info("polaris: SemanticCompressHandler and ExtensionLibrarianHandler registered")

	// ─── 6.5 Skill Library (L1 M6) ───────────────────────────────────────────
	skillRegistry := skill.NewSQLiteRegistry(store.DB())
	skillSelector := skill.NewSelector(skillRegistry)
	_ = skillSelector

	// 移除内置技能静态扫描：现统一通过 Marketplace 动态安装至标准数据目录。
	// 技能以 TypeScript/Python 脚本形式安装，通过 ContainerSandbox 执行。
	skillExecutor := skill.NewScriptSkillExecutor(skillRegistry, nil, nil)
	_ = skillExecutor
	slog.Info("polaris: skill library initialized (script-backed)")

	// ─── 7. Knowledge RAG (L2 M10) ───────────────────────────────────────────
	ingester := knowledgepkg.NewDefaultIngestionPipeline(storageRouter)
	var retriever knowledgepkg.HybridRetriever
	if surrealStore != nil {
		// Tier1+：SQLite FTS5 + SurrealDB HNSW 双路检索（BUG-1 修复：cognitive 正式注入）
		var knowledgeEmb knowledgepkg.VectorEmbedder
		if embedder != nil {
			knowledgeEmb = &knowledgeEmbedderAdapter{e: embedder}
		}
		retriever = knowledgepkg.NewHybridRetrieverWithCognitive(store.DB(), knowledgeEmb, &knowledgeCognAdapter{s: surrealStore})
		slog.Info("polaris: knowledge RAG initialized (FTS5 + SurrealDB HNSW, Tier1+)")
	} else {
		retriever = knowledgepkg.NewDefaultHybridRetriever(storageRouter, embedder)
		slog.Info("polaris: knowledge RAG initialized (StorageRouter, Tier0)")
	}

	// ─── 7.5 知识图谱构建管线（GraphBuildPipeline，M10 §2.7）───────────────────
	// InferenceRouter 实现 protocol.Provider，作为图构建的 LLM 客户端。
	var graphLLMClient swarm.LLMClient
	if router != nil {
		graphLLMClient = swarm.NewProviderLLMClient(router, "")
	}
	graphTier := 0
	if autoConf != nil {
		graphTier = int(autoConf.Config.Tier)
	}
	var graphPipeline *swarm.GraphBuildPipeline
	if autoConf != nil && autoConf.Gate.State(observability.FeatureGraphRAGFull) != observability.FeatureDisabled {
		graphPipeline = swarm.NewGraphBuildPipeline(graphLLMClient, graphTier, mem.Semantic())
		slog.Info("polaris: knowledge graph pipeline initialized", "tier", graphTier)
	} else {
		slog.Info("polaris: GraphRAG pipeline disabled by FeatureGate (Tier1+, 1024MB min)")
	}
	_ = graphPipeline

	// ─── 7.6 PII 检测器（M11 §5.1）──────────────────────────────────────────
	var piiDetector *policy.PIIDetector
	if autoConf != nil && autoConf.Gate.State(observability.FeaturePresidioPII) != observability.FeatureDisabled {
		piiDetector = policy.NewPIIDetectorWithPresidio("http://localhost:3000/analyze", safeHTTPClient)
		slog.Info("polaris: PII detector initialized (Presidio sidecar)")
	} else {
		piiDetector = policy.NewPIIDetector()
		slog.Info("polaris: PII detector initialized (Go regex Tier 0)")
	}
	_ = piiDetector

	// ─── 8. Eval Harness (L3 M12) ────────────────────────────────────────────
	evalStore := eval.NewSQLiteEvalStore(store)
	evalRunner := eval.NewRunner(store, evalStore)
	slog.Info("polaris: eval harness initialized")

	// ─── 9. Blackboard & Scheduler (L2 M8 + L3 M13) ─────────────────────────
	blackboard := swarm.NewSQLiteBlackboard(store.DB())
	reaperCtx, reaperStop := context.WithCancel(ctx)
	defer reaperStop()

	reaper := swarm.NewReaper(blackboard)
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-reaperCtx.Done():
				return
			case <-ticker.C:
				reaper.Phase1(reaperCtx)
			}
		}
	}()

	sched := scheduler.NewSQLiteScheduler(store)
	slog.Info("polaris: blackboard, scheduler, HITL gateway initialized")

	// ─── 9.5 M8 Multi-Agent Orchestrator ─────────────────────────────────────
	agentRegistry := swarm.NewAgentRegistry()
	orchestrator := swarm.NewOrchestrator(blackboard, agentRegistry, cfg.System.MaxAgents)

	// ─── 10. Agent Kernel (L1 M4) ────────────────────────────────────────────
	agent := kernel.NewAgent("agent-0", store.DB(), nil, router)
	agent.Config.MaxReplan = cfg.Thresholds.M4Kernel.MaxReplanAttempts
	agent.Config.DefaultBudget = cfg.Thresholds.M4Kernel.DefaultBudget
	agent.Config.MaxSteps = cfg.Thresholds.M4Kernel.MaxSteps
	agent.InjectHITL(hitlGateway)
	// 注入 ToolRegistry：FSM runExecuteDAG 路径依赖非 nil registry，否则 fail-closed。
	agent.InjectToolRegistry(toolReg)

	// 构造 ExtensionActivator（需要 db + cognitive + mcpMgr）
	activator := native.NewExtensionActivator(store.DB(), nativeCogn, mcpMgr)
	agent.InjectExtensionActivator(&extensionActivatorAdapter{inner: activator})
	// 注入记忆系统：ImmutableCore 持有 AgentName/ModelID/SystemPromptTemplate，
	// NewServer 和 injectSystemPrompt 均依赖 agent.Memory() != nil 才会写入系统提示词。
	agent.InjectMemory(mem)

	// Load preferences from DB and inject into agent
	if prefs, err := server.LoadAllPreferences(ctx, store.DB()); err == nil {
		agent.SetPreferences(prefs)
	} else {
		slog.Warn("polaris: failed to load preferences on startup", "err", err)
	}

	agentRegistry.Register("agent-0", swarm.AgentCard{ //nolint:errcheck
		Name:   "agent-0",
		Skills: []string{"general"},
	}, agent)

	dagExec := kernel.NewDAGExecutor(func(ctx context.Context, toolName string, args []byte, taintLevel protocol.TaintLevel) (*protocol.ToolResult, error) {
		tool := protocol.Tool{
			Name:        toolName,
			SandboxTier: 1,
		}
		return sandboxRouter.Execute(ctx, tool, args, protocol.TaintNone)
	}, nil)
	slog.Info("polaris: agent kernel & DAG executor initialized")
	evalRunner.InjectAgent(&evalAgentAdapter{agent: agent})

	// ─── 10.3 M9 Self-Improvement Engine ─────────────────────────────────────
	taskEventCh := make(chan si.TaskCompleteEvent, 64)
	versionEventCh := make(chan si.VersionChangeEvent, 8)

	// 桥接 Blackboard 事件 → M9 TaskCompleteEvent
	go func() {
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
	}()

	reflexionEngine := swarm.NewReflexionEngine(fallacyPool, heuristics, nil)
	reflexionBridge := swarm.NewReflexionBridge(reflexionEngine)
	idleDetector := swarm.NewIdleDetector()
	curriculumGen := swarm.NewAutoCurriculumGenerator(idleDetector, fallacyPool, heuristics)
	curriculumBridge := swarm.NewCurriculumBridge(curriculumGen, blackboard)
	rollout := swarm.NewProgressiveRollout()
	rolloutBridge := swarm.NewRolloutBridge(rollout)

	m9Engine := si.NewEngine(nil, reflexionBridge, curriculumBridge, rolloutBridge, taskEventCh, versionEventCh)

	// ─── PromptOptimizer（M9 三融合：GEPA + MemAPO + ContraPrompt）────────────
	promptOptimizer := swarm.NewPromptOptimizerMVP()
	_ = promptOptimizer
	slog.Info("polaris: M9 self-improvement engine + PromptOptimizer initialized")

	// go m9Engine.Start(ctx)
	// go promptOptimizer.Start(ctx)
	// go curriculumGen.Start(ctx)

	// 训练/引导适配器使用记录（避免 unused 编译错误，后续 M9 流水线消费）
	_ = qloraAdapter
	_ = prmAdapter
	_ = steeringAdapter

	// ─── 10.5 Supervisor Tree 守护进程 ─────────────────────────────────────────
	sv := supervisor.NewSupervisor(5, 5*time.Minute)

	sv.AddWorker("agent-0", func(ctx context.Context) error {
		return agent.Run(ctx)
	})
	sv.AddWorker("orchestrator", func(ctx context.Context) error {
		return orchestrator.ListenLoop(ctx)
	})
	sv.AddWorker("m9-engine", func(ctx context.Context) error {
		return m9Engine.Run(ctx)
	})

	sv.Start()
	defer sv.Stop()
	slog.Info("polaris: supervisor tree started", "workers", 3)

	// ─── 10.7 从 DB 加载全部厂商配置（唯一合法的 Provider 注册路径）
	// env var 凭据已在步骤 4 由 SeedProvidersFromEnv 写入 DB，此处统一加载。
	if err := server.LoadProvidersFromDB(ctx, store.DB(), reg, safeHTTPClient); err != nil {
		slog.Warn("polaris: LoadProvidersFromDB", "err", err)
	}

	// ─── 10.8 Eval Harness CI Gate ─────────────────────────────────────────────
	if len(os.Args) > 2 && os.Args[1] == "eval" && os.Args[2] == "--ci-gate" {
		slog.Info("polaris: running eval --ci-gate validation suite")
		report, runErr := evalRunner.RunSuite(ctx, "validation", "ci")
		if runErr != nil {
			return errors.Wrap(errors.CodeInternal, "eval ci-gate execution failed", runErr)
		}
		if report.Status == "failed" {
			return errors.New(errors.CodeInternal, fmt.Sprintf("eval ci-gate failed: pass=%d fail=%d safety_fail=%d", report.PassCount, report.FailCount, report.SafetyFail))
		}
		slog.Info("polaris: eval ci-gate passed", "pass_count", report.PassCount)
		return nil
	}

	// ─── 11. M13 Interface Server ──────────────────────────────────────────────
	// FeatureWebUI 仅控制 dashboard 是否渲染；REST API 始终启动（Agent 通信依赖）。
	if autoConf != nil && autoConf.Gate.State(observability.FeatureWebUI) == observability.FeatureDisabled {
		slog.Warn("polaris: FeatureWebUI disabled by FeatureGate — serving API-only mode, dashboard unavailable")
	}
	addr := fmt.Sprintf("%s:%d", cfg.Interface.Host, cfg.Interface.Port)
	httpServer := server.NewServer(addr, dataDir, agent, blackboard, hitlGateway, store.DB(), reg, safeHTTPClient, dialer, cfg.Compressor)

	// Ensure signing key exists
	var skillSigningKey []byte
	if key := os.Getenv("POLARIS_SKILL_SIGNING_KEY"); key != "" { //nolint:nestif
		skillSigningKey = []byte(key)
	} else {
		if b, err := os.ReadFile(layout.SkillSignKey); err == nil && len(b) > 0 {
			skillSigningKey = b
		} else {
			h := sha256.Sum256(fmt.Appendf(nil, "polaris-local-%d", time.Now().UnixNano()))
			skillSigningKey = h[:]
			if err := os.WriteFile(layout.SkillSignKey, skillSigningKey, 0600); err != nil {
				slog.Warn("polaris: failed to write skill_signing.key", "err", err)
			}
		}
	}

	// ── OTA 热更新管理器 ─────────────────────────────────────────────────────────
	updMgr := updater.New(Version, CommitHash, BuildDate, safeHTTPClient)
	updMgr.SetRestartFn(func() {
		// 使用 syscall.Exec 原地替换进程（保持 PID，launchd/systemd 无感重启）
		exe, _ := os.Executable()
		slog.Info("polaris: exec-restarting with new binary", "path", exe)
		_ = syscall.Exec(exe, os.Args, os.Environ())
		os.Exit(0) // syscall.Exec 失败时兜底
	})
	// 前端直接调 GitHub API 检查版本（hermes 设计），后端只负责执行更新，无需后台轮询。
	httpServer.SetUpdater(updMgr)

	httpServer.SetInstallManager(installMgr)
	httpServer.SetSkillSigningKey(skillSigningKey)
	httpServer.SetMCPManager(mcpMgr)
	// install hook 沙箱执行器：containerSandbox 可为 nil（Tier-0 macOS），
	// server.downloadAndInstallExtension 会降级为 skip+warn。
	if containerSandbox != nil {
		httpServer.SetScriptRunner(containerSandbox)
	}
	httpServer.SetToolRegistry(toolReg)
	httpServer.SetSkillRegistry(skillRegistry)
	httpServer.SetToolExecutor(func(ctx context.Context, name string, args []byte) (*protocol.ToolResult, error) {
		// script runtime 技能：LLM 工具名格式为 "skill__{slug}"，内部 DB 名为 "skill:{slug}"
		if slug, ok := strings.CutPrefix(name, "skill__"); ok {
			var instructions string
			_ = store.DB().QueryRowContext(ctx,
				`SELECT instructions FROM skills WHERE name=? AND deprecated=0`, "skill:"+slug).Scan(&instructions)
			var req struct {
				Input string `json:"input"`
			}
			_ = json.Unmarshal(args, &req)
			output := instructions
			if req.Input != "" {
				output += "\n\n---\n\n输入：" + req.Input
			}
			return &protocol.ToolResult{Output: []byte(output)}, nil
		}
		return sandboxRouter.Execute(ctx, protocol.Tool{Name: name}, args, protocol.TaintNone)
	})
	httpServer.SetLogStore(logStore)
	httpServer.SetEvalRunner(evalRunner)

	// ─── 11.5 STT 引擎初始化（FeatureLocalSTT 门控，异步下载，不阻塞启动）────────
	var sttGate *observability.FeatureGate
	if autoConf != nil {
		sttGate = autoConf.Gate
	}
	server.InitSTTEngine(ctx, dataDir, sttGate, safeHTTPClient, cfg.Inference.STT)
	server.InitTTSEngine(ctx, dataDir, sttGate, safeHTTPClient, cfg.Inference.TTS)

	if err := httpServer.Start(); err != nil {
		slog.Error("polaris: failed to start HTTP server", "err", err)
		return err
	}
	go mcpMgr.LoadFromDB(ctx, store.DB(), dataDir) // 统一异步连接所有 MCP（含插件内嵌，via plugin_id 字段）

	// ─── 12. 启动摘要 ─────────────────────────────────────────────────────────
	printStartupSummary(cfg, gate, router, mem, ingester, retriever, evalRunner, blackboard, sched, hitlGateway, agent, dagExec, httpServer)

	// ─── 13. 开箱引导 (Zero-Provider Detection) ──────────────────────────────
	var providerCount int
	_ = store.DB().QueryRow("SELECT COUNT(*) FROM providers").Scan(&providerCount)

	if providerCount == 0 {
		if cliTTY {
			_ = runInit()
		} else {
			slog.Warn("polaris: [Zero-Provider] No AI providers found in the database.")
			slog.Warn("polaris: Please visit http://localhost:28888 or run `polaris init` to configure the system.")
		}
	}

	// ─── 14. 等待终止信号 (优雅退出) ─────────────────────────────────────────
	slog.Info("polaris: system ready — waiting for signals (SIGINT/SIGTERM to exit)")
	<-ctx.Done()

	slog.Info("polaris: shutdown initiated, draining...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	_ = httpServer.Shutdown(shutdownCtx)
	reaperStop()

	// 等待 DatabaseWriter flush 残余批次后再 Close（保证 AI 核心数据不丢失）
	select {
	case <-dbWriterDone:
	case <-shutdownCtx.Done():
		slog.Warn("polaris: database writer flush timeout during shutdown")
	}
	dbWriter.Close()

	slog.Info("polaris: shutdown complete")
	return nil
}

func resolveDataDirBase(cfg *config.Config) (string, error) {
	dir := os.Getenv("POLARIS_DATA_DIR")
	if dir == "" && cfg != nil && cfg.System.DataDir != "" {
		dir = cfg.System.DataDir
	}
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", errors.Wrap(errors.CodeInternal,
				"cannot determine home directory; set POLARIS_DATA_DIR explicitly", err)
		}
		dir = filepath.Join(home, ".polarisagi/polaris")
	} else if strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", errors.Wrap(errors.CodeInternal,
				"cannot determine home directory for ~ expansion", err)
		}
		dir = filepath.Join(home, dir[2:])
	}
	return dir, nil
}

func printStartupSummary(cfg *config.Config, components ...any) {
	slog.Info("polaris: system initialized",
		"tier", cfg.System.Tier,
		"max_agents", cfg.System.MaxAgents,
		"os", runtime.GOOS,
		"components", len(components),
	)
}

// evalAgentAdapter 适配 kernel.Agent → eval.EvalAgent 接口。
// kernel.Agent.Run(ctx) error 与 eval.EvalAgent.Run(ctx, []byte) ([]byte, []string, error) 签名不匹配。
type evalAgentAdapter struct {
	agent *kernel.Agent
}

// initSurrealStore 初始化 SurrealDB Core 认知存储，提取出独立函数以降低 main 嵌套复杂度。
// backend: "mem"（默认，256MB+ 可用）/ "rocksdb"（持久化，推荐大内存服务器）。
// workerThreads: <= 0 = auto（min(CPU, 4)）；VPS 推荐在 config 中设 2 以节省内存。
func initSurrealStore(
	autoConf *observability.AutoConfig,
	cfg *config.Config,
	layout config.DataLayout,
) *storage.SurrealDBCoreStore {
	if autoConf == nil || autoConf.Gate.State(observability.FeatureSurrealDBCore) == observability.FeatureDisabled {
		slog.Info("polaris: SurrealDB Core disabled by FeatureGate (memory pressure), cognitive axis → SQLite")
		return nil
	}
	backend := cfg.Cognition.SurrealBackend
	if backend == "" {
		backend = "mem"
	}
	dbPath := cfg.Cognition.SurrealDBPath
	if dbPath == "" {
		dbPath = layout.SurrealDB
	}
	vecDim := cfg.Inference.EmbedderDim
	if vecDim <= 0 {
		vecDim = 1536
	}
	workerThreads := cfg.Cognition.SurrealWorkerThreads
	store, err := storage.OpenSurrealDBCore(backend, dbPath, vecDim, workerThreads)
	if err != nil {
		slog.Warn("polaris: SurrealDB Core init failed, cognitive axis falls back to SQLite", "err", err)
		return nil
	}
	slog.Info("polaris: SurrealDB Core initialized",
		"backend", backend,
		"vec_dim", vecDim,
		"worker_threads", workerThreads,
	)
	return store
}

// surrealCognAdapter 将 *storage.SurrealDBCoreStore 适配为 memory.CognitiveSearcher。
// FTSIndex / VecUpsert 签名完全一致，直接透传。
// VecKNN / FTSSearch 返回类型 storage.ScoredID → memory.CognitiveSearchResult 需转换。
type surrealCognAdapter struct{ s *storage.SurrealDBCoreStore }

func (a *surrealCognAdapter) FTSIndex(docID, text string) error {
	return a.s.FTSIndex(docID, text)
}

func (a *surrealCognAdapter) VecUpsert(id string, embedding []float32) error {
	return a.s.VecUpsert(id, embedding)
}

func (a *surrealCognAdapter) VecKNN(query []float32, k int) ([]memory.CognitiveSearchResult, error) {
	hits, err := a.s.VecKNN(query, k)
	if err != nil {
		return nil, err
	}
	out := make([]memory.CognitiveSearchResult, len(hits))
	for i, h := range hits {
		out[i] = memory.CognitiveSearchResult{ID: h.ID, Score: h.Score}
	}
	return out, nil
}

func (a *surrealCognAdapter) FTSSearch(query string, k int) ([]memory.CognitiveSearchResult, error) {
	hits, err := a.s.FTSSearch(query, k)
	if err != nil {
		return nil, err
	}
	out := make([]memory.CognitiveSearchResult, len(hits))
	for i, h := range hits {
		out[i] = memory.CognitiveSearchResult{ID: h.ID, Score: h.Score}
	}
	return out, nil
}

// knowledgeCognAdapter 将 *storage.SurrealDBCoreStore 适配为 knowledge.CognitiveSearcher。
// 返回类型 storage.ScoredID → knowledgepkg.CognitiveSearchResult（两个包独立定义，BUG-5 TODO）。
// TODO(BUG-5): 与 surrealCognAdapter 合并，将 CognitiveSearchResult 统一到 internal/protocol。
type knowledgeCognAdapter struct{ s *storage.SurrealDBCoreStore }

func (a *knowledgeCognAdapter) VecKNN(query []float32, k int) ([]knowledgepkg.CognitiveSearchResult, error) {
	hits, err := a.s.VecKNN(query, k)
	if err != nil {
		return nil, err
	}
	out := make([]knowledgepkg.CognitiveSearchResult, len(hits))
	for i, h := range hits {
		out[i] = knowledgepkg.CognitiveSearchResult{ID: h.ID, Score: h.Score}
	}
	return out, nil
}

func (a *knowledgeCognAdapter) FTSSearch(query string, k int) ([]knowledgepkg.CognitiveSearchResult, error) {
	hits, err := a.s.FTSSearch(query, k)
	if err != nil {
		return nil, err
	}
	out := make([]knowledgepkg.CognitiveSearchResult, len(hits))
	for i, h := range hits {
		out[i] = knowledgepkg.CognitiveSearchResult{ID: h.ID, Score: h.Score}
	}
	return out, nil
}

// knowledgeEmbedderAdapter 将 substrate.Embedder 适配为 knowledge.VectorEmbedder。
// substrate.Embedder.Embed(text string) []float32 → Embed(ctx, text) ([]float32, error)。
type knowledgeEmbedderAdapter struct{ e substrate.Embedder }

func (a *knowledgeEmbedderAdapter) Embed(_ context.Context, text string) ([]float32, error) {
	return a.e.Embed(text), nil
}

func (a *evalAgentAdapter) Run(ctx context.Context, input []byte) ([]byte, []string, error) {
	a.agent.SetTaskIntent(input)

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.agent.Run(ctx)
	}()

	_ = a.agent.SendIntent(protocol.TriggerIntentReceived)

	select {
	case err := <-errCh:
		if err != nil {
			return nil, nil, err
		}
		return a.agent.GetExecuteResult(), nil, nil
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

// nativeCognAdapter 将 *storage.SurrealDBCoreStore 适配为 native.CognitiveSearcher。
type nativeCognAdapter struct{ s *storage.SurrealDBCoreStore }

func (a nativeCognAdapter) FTSSearch(query string, k int) ([]native.ScoredResult, error) {
	if a.s == nil {
		return nil, nil
	}
	res, err := a.s.FTSSearch(query, k)
	if err != nil {
		return nil, err
	}
	out := make([]native.ScoredResult, len(res))
	for i, r := range res {
		out[i] = native.ScoredResult{ID: r.ID, Score: r.Score}
	}
	return out, nil
}

func (a nativeCognAdapter) VecKNN(query []float32, k int) ([]native.ScoredResult, error) {
	if a.s == nil {
		return nil, nil
	}
	res, err := a.s.VecKNN(query, k)
	if err != nil {
		return nil, err
	}
	out := make([]native.ScoredResult, len(res))
	for i, r := range res {
		out[i] = native.ScoredResult{ID: r.ID, Score: r.Score}
	}
	return out, nil
}

func (a nativeCognAdapter) GraphSpreadingActivation(startIDs []string, maxDepth int, energyDecay, dormancyThreshold float64, fanOutLimit int) ([]native.ScoredResult, error) {
	if a.s == nil {
		return nil, nil
	}
	res, err := a.s.GraphSpreadingActivation(startIDs, maxDepth, energyDecay, dormancyThreshold, fanOutLimit)
	if err != nil {
		return nil, err
	}
	out := make([]native.ScoredResult, len(res))
	for i, r := range res {
		out[i] = native.ScoredResult{ID: r.ID, Score: r.Score}
	}
	return out, nil
}

type extensionActivatorAdapter struct {
	inner *native.ExtensionActivator
}

func (a *extensionActivatorAdapter) FindAndActivate(ctx context.Context, goal string) ([]kernel.ExtActivatedHint, error) {
	hints, err := a.inner.FindAndActivate(ctx, goal)
	if err != nil || hints == nil {
		return nil, err
	}
	result := make([]kernel.ExtActivatedHint, 0, len(hints))
	for _, h := range hints {
		result = append(result, kernel.ExtActivatedHint{
			ToolName:    h.ToolName,
			Description: h.Description,
		})
	}
	return result, nil
}
