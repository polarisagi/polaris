// boot_substrate.go — §0.5~§4 启动阶段：
// 配置加载 → 数据目录 → 硬件探针 → KillSwitch → 日志 → 存储层 → 策略引擎 → 推理路由器。
// SubstrateBundle 持有所有 L0 基础设施产物，向上层 boot 函数传递。
//
// resolveDataDirBase 和 initSurrealStore 从 main.go 移入，消除 main.go 对 storage/observability 的直接依赖。
package main

import (
	"github.com/polarisagi/polaris/internal/security/policy"

	"github.com/polarisagi/polaris/internal/security/credential"
	"github.com/polarisagi/polaris/internal/security/network"
	"github.com/polarisagi/polaris/internal/security/taint"

	"github.com/polarisagi/polaris/internal/observability/probe"

	"github.com/polarisagi/polaris/internal/observability/metrics"

	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/polarisagi/polaris/configs"
	"github.com/polarisagi/polaris/internal/automation"
	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/downloader"
	"github.com/polarisagi/polaris/internal/eval"
	"github.com/polarisagi/polaris/internal/ffi"
	"github.com/polarisagi/polaris/internal/gateway/egress"
	"github.com/polarisagi/polaris/internal/gateway/server"
	"github.com/polarisagi/polaris/internal/gateway/server/provider"
	"github.com/polarisagi/polaris/internal/llm"
	llmadapter "github.com/polarisagi/polaris/internal/llm/adapter"
	"github.com/polarisagi/polaris/internal/llm/ollamamgr"
	"github.com/polarisagi/polaris/internal/observability"
	"github.com/polarisagi/polaris/internal/observability/budget"
	"github.com/polarisagi/polaris/internal/prompt"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/internal/security"
	sysstore "github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/internal/store/audit"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/internal/store/search"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// SubstrateBundle 持有 §0.5~§4 所有 L0 基础设施产物。
// 向所有上层 boot 函数传递，避免大量参数列表。
type SubstrateBundle struct {
	// 配置与数据目录
	Cfg       *config.Config
	DataDir   string
	Layout    config.DataLayout
	Vault     *credential.Vault
	PromptMgr protocol.PromptFacade

	// RAGChunksTaintSerializer 是 rag_chunks 表跨 SQL 持久化边界的 HMAC-SHA256
	// 校验器（M11-Policy-Safety.md §2.1 第三重防护，inv_M11_02）。key 从 Vault
	// masterKey 派生（domain-separated subkey，用途标识见 ragChunksTaintHMACPurpose），
	// Vault 缺失时为 nil（此时 knowledge 包各写/读点退化为不校验，见
	// sealChunkTaint/verifyChunkTaint 的 nil-serializer 分支）。
	RAGChunksTaintSerializer *taint.TaintBoundarySerializer

	// 硬件探针与可观测性（AutoConf 可 nil，Tier0 降级）
	AutoConf     *observability.AutoConfig
	TBR          *metrics.TokenBurnRate
	DriftMonitor *eval.DriftMonitor

	// 安全三件套
	KS         *security.KillSwitch
	AuditTrail *security.AuditTrail
	AuditChain *audit.AuditChain

	// 日志：LogFile 需调用方 defer Close（可 nil）
	LogFile  io.Closer
	LogStore *server.LogStore

	// 存储层
	Store         *sysstore.SQLiteStore
	SurrealStore  *sysstore.SurrealDBCoreStore // 可 nil（<8GB VPS）
	StorageRouter *sysstore.StorageRouter
	Outbox        *sysstore.OutboxWorker
	DBWriter      *sysstore.DatabaseWriter
	DBWriterDone  <-chan struct{}
	DecisionLog   *audit.SQLiteDecisionLog // M3/M7 决策审计日志，注入给 PipelineOrchestrator

	// 策略引擎
	Gate     *policy.Gate
	TrustMap map[string]int

	// 网络安全层
	Dialer   *network.SafeDialer
	SafeHTTP *http.Client

	// 推理层
	InfReg   *llm.ProviderRegistry
	Router   *llm.InferenceRouter
	Embedder search.Embedder // 可 nil（FeatureLocalEmbedding 未启用）；实际类型为 *search.SyncBatcherAdapter，
	// 请求经 EmbeddingBatcher 自动合批后再调用 DynEmbedder，供 Router/Knowledge/Memory/Tool 等高频调用方使用。
	// DynEmbedder 底层动态原子代理（Set() 热替换真实引擎，WaitReady() 首次就绪信号）。
	// Embedder 字段自 EmbeddingBatcher 接线后已不再直接持有 *llm.DynamicEmbedder，下游若需要
	// WaitReady() 信号或真批量 EmbedBatch()（如 boot_server.go §11.6 回填触发器、插件目录预计算器），
	// 须直接使用本字段而非对 Embedder 做类型断言。恒非 nil（bootSubstrate 无条件构造）。
	DynEmbedder *llm.DynamicEmbedder

	// 训练适配器（门控，M9 流水线消费；当前作占位）
	QLoRA    *llmadapter.QLoRAAdapter
	PRM      *llmadapter.PRMAdapter
	Steering *llmadapter.SteeringAdapter
	// CVStore 激活引导控制向量注册表（M09 §1.3 /steer 命令面，2026-07-21 补齐），
	// 与 Steering 同步构造（非 nil 当且仅当 Steering 非 nil）。
	CVStore *llmadapter.ControlVectorStore

	// OTA restart fn 捕获：取消主 ctx + 等待 dbWriter + 关闭 store
	Stop context.CancelFunc
}

// bootSubstrate 执行 §0.5~§4 初始化，返回 L0 基础设施 bundle。
// stop 来自 run() 的 signal.NotifyContext，注入 bundle 供 OTA restart fn 使用。
func bootSubstrate(ctx context.Context, stop context.CancelFunc) (*SubstrateBundle, error) { //nolint:gocyclo
	concurrent.SetOnPanic(func() {
		if metrics.InstrGoroutinePanicTotal != nil {
			metrics.InstrGoroutinePanicTotal.Add(context.Background(), 1)
		}
	})

	// ─── 0.5 内核完整性校验 (L4) ─────────────────────────────────────────────
	if err := config.VerifyKernelIntegrity(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "CRITICAL: kernel integrity compromised", err)
	}

	// ─── 1. 配置加载 ─────────────────────────────────────────────────────────
	cfgPath := os.Getenv("POLARIS_CONFIG")
	if cfgPath != "" {
		// 显式配置路径缺失 → fail-fast，避免掩盖运维挂载问题
		if _, statErr := os.Stat(cfgPath); os.IsNotExist(statErr) {
			return nil, apperr.New(apperr.CodeInternal, "POLARIS_CONFIG file not found: "+cfgPath)
		}
	} else {
		home, _ := os.UserHomeDir()
		cfgPath = filepath.Join(home, ".polarisagi/polaris", "config.toml")
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "config.Load", err)
	}
	slog.Info("polaris: config loaded", "tier", cfg.System.Tier, "max_agents", cfg.System.MaxAgents)
	config.Update(cfg)

	// ─── 0.3 数据目录解析与初始化 ────────────────────────────────────────────
	dataDir, err := resolveDataDirBase(cfg)
	if err != nil {
		return nil, err
	}
	layout := config.NewDataLayout(dataDir, cfg.System.Dirs)
	if err := layout.MkdirAll(); err != nil {
		slog.Warn("polaris: failed to create data directories", "err", err)
	}
	layout.Migrate()

	vault, err := credential.NewVaultInDir(dataDir)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "credential.NewVaultInDir", err)
	}
	// ragChunksTaintHMACPurpose 是从 vault masterKey 派生 rag_chunks 跨边界
	// HMAC 密钥的用途标识（inv_M11_02，见 009_rag_chunks.sql taint_hmac 列注释）。
	// 修改此常量等价于密钥轮换——历史行 taint_hmac 全部校验失败，读取时降级为
	// TaintHigh（fail-closed，仅信任等级重置，非数据丢失）。
	const ragChunksTaintHMACPurpose = "rag_chunks_taint_boundary_v1"
	ragChunksTaintSerializer := taint.NewTaintBoundarySerializer(vault.DeriveKey(ragChunksTaintHMACPurpose))

	// ─── 0.3.5 Thresholds 覆盖加载 ──────────────────────────────────────────
	thresholds, err := config.GetThresholds(dataDir)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "config.GetThresholds", err)
	}
	cfg.Thresholds = *thresholds

	// ─── 0.34 硬件探针 & TBR ────────────────────────────────────────────────
	autoConf, err := observability.NewAutoConfig()
	if err != nil {
		slog.Warn("polaris: AutoConfig failed, using Tier0 defaults", "err", err)
	}
	var tbr *metrics.TokenBurnRate
	if autoConf != nil {
		tbr = autoConf.TBR
	} else {
		tbr = metrics.NewTokenBurnRate()
	}
	driftMonitor := eval.NewDriftMonitor()
	metrics.SetFoundingAnchorDriftScorer(driftMonitor.GetScore)

	// ─── 0.35 KillSwitch ────────────────────────────────────────────────────
	if security.IsFullStopFilePresent(dataDir) {
		return nil, apperr.New(apperr.CodeInternal,
			"system is sealed (.fullstop exists in "+dataDir+"); remove the file to restart")
	}
	ks := security.NewKillSwitch(dataDir, tbr)
	metrics.GlobalPerformanceDrift().RegisterListener(func(alert metrics.DriftAlert) {
		slog.Warn("polaris: performance drift detected",
			"level", alert.Level(),
			"current_rate", alert.CurrentRate,
			"baseline_rate", alert.BaselineRate,
			"relative_drop", alert.RelativeDrop,
			"window_size", alert.WindowSize)
		if alert.Level() == metrics.DriftLevelCritical {
			slog.Error("polaris: critical performance drift! Triggering KillSwitch FullStop")
			ks.ManualFullStop("performance_drift", "Critical performance drift detected")
		}
	})

	ks.StateChangeCallback = func(newState types.KillState, _ string) {
		metrics.GlobalKillswitchStage.Store(int32(newState))
	}

	promptMgr := prompt.NewManager(filepath.Join(cfgPath, ".."), configs.FS)

	// ─── 0.4 日志初始化 ──────────────────────────────────────────────────────
	logFile := observability.SetupLogger(dataDir)
	logStore := server.NewLogStore(slog.Default().Handler(), 500)
	slog.SetDefault(slog.New(logStore))
	slog.Info("polaris: logger initialized", "data_dir", dataDir)
	if autoConf != nil {
		slog.Info("polaris: hardware probed",
			"tier", autoConf.Config.Tier,
			"ram_mb", autoConf.Config.TotalRAMMB,
			"cpu_cores", autoConf.Config.CPUCores,
		)
		// AutoConfig.Summary() 此前已完整实现（tier/ram/cpu/provider/local_model/
		// qlora/l3_sandbox/script_workers/storage 一行摘要），但从未被任何调用方
		// 消费，启动日志一直只有上面这 3 个字段的精简版本。
		slog.Info(autoConf.Summary())
	}

	// ─── 0.6 TBR 心跳 goroutine ─────────────────────────────────────────────
	concurrent.SafeGo(ctx, "boot_substrate.tbr_heartbeat", func(ctx context.Context) {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				tbr.Tick()
			}
		}
	})

	// ─── 0.7 内存压力监控（每 5s 轮询，驱动 FeatureGate 运行时降级）──────────
	concurrent.SafeGo(ctx, "boot_substrate.memory_watcher", func(ctx context.Context) {
		autoConf.RunMemoryWatcher(ctx)
	})
	slog.Info("polaris: memory pressure monitor started", "poll_interval_s", 5)

	// ─── 2. SQLite 存储 ──────────────────────────────────────────────────────
	// cleanup guard：以下任何步骤失败都要关闭已打开的资源
	var committed bool
	if logFile != nil {
		defer func() {
			if !committed {
				logFile.Close()
			}
		}()
	}

	store, err := sysstore.OpenSQLite(layout.SQLiteDB, schema.FS)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "sysstore.OpenSQLite", err)
	}
	defer func() {
		if !committed {
			store.Close()
		}
	}()
	slog.Info("polaris: storage initialized", "db", layout.SQLiteDB)

	// ─── 2.5 SurrealDB Core 认知存储（FeatureSurrealDBCore 门控）────────────
	surrealStore := initSurrealStore(autoConf, cfg, layout)

	// B4-F3: 将 SurrealDB Purge 回调注入 AutoConfig，
	// 使 OSMemoryGuard DegradationCritical 时可清理认知轴内存。
	if autoConf != nil && surrealStore != nil {
		autoConf.WithSurrealPurger(func() {
			if err := surrealStore.Purge(); err != nil {
				slog.Warn("polaris: surrealStore.Purge failed during memory pressure", "err", err)
			}
		})
		slog.Info("polaris: SurrealDB purger wired into AutoConfig memory pressure callback")
	}

	// ─── 2.6 StorageRouter（三轴统一路由）───────────────────────────────────
	var surrealProto protocol.Store
	if surrealStore != nil {
		surrealProto = surrealStore
	}
	storageRouter := sysstore.NewStorageRouter(store, surrealProto)
	slog.Info("polaris: storage router initialized")

	// ─── 2.7 OutboxWorker（跨引擎投影）─────────────────────────────────────
	// [MUST] 必须调用 Run()，不得内联 goroutine 替代。
	// Run() 内含 loadCursor（DB 恢复游标）、saveCursor（单调性保护）、
	// processAndMark（CAS 防竞争）、Poison Pill 隔离、失败指数退避。
	// 参考：docs/arch/Module-Dependency-Axioms.md XR-04 + outbox_worker.go L176
	outboxWorker := sysstore.NewOutboxWorker(store.DB(), 5, 3)
	concurrent.SafeGo(ctx, "outbox-worker", func(ctx context.Context) {
		if err := outboxWorker.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("polaris: outbox worker exited unexpectedly", "err", err)
		}
	})
	slog.Info("polaris: outbox worker started (Run mode, cursor-persistent)", "poll_interval_s", 5)

	// ─── 2.8 DatabaseWriter（AI 核心数据单写者）────────────────────────────
	dbWriter := sysstore.NewDatabaseWriter(store.DB(), nil)
	dbWriterDoneCh := make(chan struct{})
	concurrent.SafeGo(ctx, "boot_substrate.db_writer", func(ctx context.Context) {
		dbWriter.Run(ctx)
		close(dbWriterDoneCh)
	})
	eventLog := audit.NewSQLiteEventLog(dbWriter)
	decisionLog := audit.NewSQLiteDecisionLog(dbWriter)
	_ = eventLog // 待 M4 Agent Kernel 注入
	slog.Info("polaris: mutation bus (database writer) started")

	// ─── 2.9 AuditTrail ──────────────────────────────────────────────────────
	auditRepo := repo.NewSQLiteAuditRepository(store.DB(), eventLog)
	auditTrail := security.NewAuditTrail(auditRepo, layout.AuditArchive)
	if err := auditTrail.RecoverOnStartup(); err != nil {
		slog.Error("polaris: AuditTrail recovery failed", "err", err)
		return nil, err
	}
	slog.Info("polaris: audit trail recovered and initialized")

	auditChain := audit.NewAuditChain(store.DB())
	var auditGuard *probe.OSMemoryGuard
	var auditGate *probe.FeatureGate
	if autoConf != nil {
		auditGuard = autoConf.Guard
		auditGate = autoConf.Gate
	}
	concurrent.SafeGo(ctx, "audit-chain-periodic-verify", func(ctx context.Context) {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		resourceBudget := budget.NewResourceBudget(tbr, auditGuard, auditGate)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if resourceBudget.BackgroundPermit(3) {
					rep, err := auditChain.VerifyChain(ctx, 0)
					if err != nil {
						slog.Error("audit: chain verify failed", "err", err)
					} else if !rep.Valid {
						slog.Error("audit: chain integrity broken", "report", rep)
					} else {
						slog.Info("audit: periodic verify passed")
					}
				}
			}
		}
	})

	// ─── 月度成本报告 (M13 §1.1) ─────────────────────────────────────────────
	automation.StartMonthlyCostReport(ctx, layout.Reports, store.DB())

	// ─── 3. 策略引擎 (L0 PolicyGate) ────────────────────────────────────────
	// onKillSwitch: Cedar 评估连续失败 10 次 / FFI 超时累计 5 次触发（gate.go recordFailure/
	// evaluateCedar）。[2026-07-12 修复] 此前仅记录日志、未做任何实际动作——策略引擎本身持续
	// 故障（fail-closed 语义要求此时应收紧而非静默降级）从未真正驱动 KillSwitch Stage 1
	// 熔断（ks 在此处已构造，见上方 §0.35）。HITL 网关通知仍是已知缺口，留待网关初始化后
	// 补充（真正的通知投递需要 hitlGateway 实例，此处保留待办而非假装已完成）。
	gate := policy.NewGate(func() {
		slog.Error("polaris: POLICY GATE HITL TRIGGERED — human review required",
			"component", "policy_gate",
			"action", "hitl_callback",
			"note", "TODO: wire hitlGateway.Notify here after gateway initialization",
		)
		ks.ReportError()
	})

	switch strings.ToLower(cfg.Policy.CedarEnforceMode) {
	case "full":
		gate.WithCedarEnforceMode(policy.CedarEnforceFull)
	case "deny":
		gate.WithCedarEnforceMode(policy.CedarEnforceDeny)
	default:
		gate.WithCedarEnforceMode(policy.CedarShadow)
	}
	// 加载 Cedar 策略：优先使用 configs/ embed 内置策略；若配置了覆盖路径，从磁盘加载。
	// Cedar 加载失败不阻断启动——evaluate() 已有 Go 规则兜底。
	if cedarErr := loadGateCedarPolicies(gate, cfg.Policy); cedarErr != nil {
		slog.Warn("polaris: cedar policy load failed, using Go fallback rules",
			"err", cedarErr,
			"engine", cfg.Policy.Engine,
		)
	} else {
		slog.Info("polaris: cedar policy loaded", "policy_count", gate.CedarPolicyCount())
	}
	slog.Info("polaris: policy gate initialized (deny-by-default)")

	// ─── 3.5 信任发布者白名单 ─────────────────────────────────────────────────
	publisherTrustMap := config.ListTrustedPublishers(configs.FS, "extensions/trusted-publishers.yaml")

	// ─── 4. 推理路由器 (L0 M1) ──────────────────────────────────────────────
	dialer := network.NewSafeDialer(0, cfg.System.EgressAllowedDomains, cfg.Thresholds.M11Policy)
	safeHTTPClient := network.NewSafeHTTPClient(dialer)
	// M13 §1.2.2: EgressGateway 包裹 SafeDialer transport，添加域名白名单预检层
	allowedDomains := append(egress.DefaultAllowedDomains(), cfg.System.EgressAllowedDomains...)
	egressGW := egress.NewEgressGateway(safeHTTPClient.Transport, allowedDomains)
	safeHTTPClient.Transport = egressGW
	llmadapter.SetDefaultHTTPClient(safeHTTPClient)
	// 将 SafeDialer 注入下载器，使 GitHub Proxy 探测也经过 SSRF 过滤（XR-06）
	downloader.Configure(cfg.Download.GithubProxy, safeHTTPClient)

	reg := llm.NewProviderRegistry(cfg.Thresholds.M1Router)
	// env var 中的 API Key 写入 DB（INSERT OR IGNORE），由 LoadProvidersFromDB 统一加载。
	provider.SeedProvidersFromEnv(ctx, repo.NewSQLiteProviderRepository(store.DB()).WithVault(vault))

	// ─── 4.5~4.9 本地推理适配器（各 FeatureGate 门控）────────────────────────
	// ollamaHTTPClient 允许访问 loopback（127.x/::1），专用于系统级受控本地服务（Ollama）。
	// 其余私有 CIDR 仍受 SafeDialer SSRF 阻断；不得用于用户可控的出站请求。
	ollamaHTTPClient := network.NewLoopbackSafeHTTPClient(cfg.Thresholds.M11Policy)
	var embedder search.Embedder
	var qloraAdapter *llmadapter.QLoRAAdapter
	var prmAdapter *llmadapter.PRMAdapter
	var steeringAdapter *llmadapter.SteeringAdapter
	var cvStore *llmadapter.ControlVectorStore

	// 创建动态原子代理，瞬间点亮系统基础功能
	dynEmbedder := llm.NewDynamicEmbedder()

	// 透传给 dynEmbedder.EmbedBatch：若当前挂载的真实引擎（Ollama/OpenAI 兼容适配器）
	// 支持批量 API，则一次 HTTP 往返处理整个批次；否则内部自动降级为逐条 Embed。
	// 之前这里手写 for 循环逐条调用 dynEmbedder.Embed，导致 EmbeddingBatcher 攒批之后
	// 仍是 N 次串行调用，白白丢失了批处理收益。
	embedFn := func(ctx context.Context, texts []string, _ string) ([][]float32, error) {
		return dynEmbedder.EmbedBatch(ctx, texts)
	}
	batcher := search.NewEmbeddingBatcher(10*time.Millisecond, 100, embedFn)
	batcher.Start(context.Background())
	embedder = search.NewSyncBatcherAdapter(batcher)

	// 智能判定优先级的核心逻辑
	targetModel := ""
	if cfg.Embedding.Model != "" && cfg.Embedding.BaseURL == "" {
		targetModel = cfg.Embedding.Model
		slog.Info("polaris: Using explicit local embedding model", "model", targetModel)
	} else if autoConf != nil && autoConf.Gate.State(probe.FeatureLocalEmbedding) != probe.FeatureDisabled {
		targetModel = autoConf.Config.LocalEmbeddingModel
		if targetModel == "" {
			targetModel = "nomic-embed-text"
		}
		slog.Info("polaris: Using auto-detected local embedding model", "model", targetModel, "dim", autoConf.Config.LocalEmbeddingDim)
	}

	if targetModel != "" { //nolint:nestif
		// 1. 全自动免安装与异步自愈逻辑 (Zero-setup background boot)
		concurrent.SafeGo(context.Background(), "boot_substrate.ollama_lifecycle", func(ctxBg context.Context) {
			slog.Info("polaris: Starting background Ollama lifecycle manager...")
			binPath, err := ollamamgr.EnsureOllama(ctxBg, safeHTTPClient, layout.Bin)
			if err != nil {
				slog.Error("polaris: Failed to install local Ollama", "err", err)
				return
			}

			// Ensure service runs in background with a client that allows loopback polling
			loopbackClient := network.NewLoopbackSafeHTTPClient(cfg.Thresholds.M11Policy)
			_, err = ollamamgr.StartOllama(ctxBg, loopbackClient, binPath)
			if err != nil {
				slog.Error("polaris: Failed to start local Ollama", "err", err)
				return
			}

			if err := ollamamgr.EnsureModel(ctxBg, binPath, targetModel); err != nil {
				slog.Error("polaris: Failed to pull embedding model", "err", err)
				return
			}

			// 一切就绪，热更新引擎
			adapter := llmadapter.NewOllamaEmbeddingAdapter(targetModel, ollamaHTTPClient)
			dynEmbedder.Set(adapter)
			slog.Info("polaris: Dynamic embedding engine is now ACTIVE!", "model", targetModel)
		})
	} else if cfg.Embedding.BaseURL != "" {
		// 2. 远程 API 绝对兜底逻辑 (只在本地跑不起且强制配置时才用)
		apiKey := []byte(cfg.Embedding.APIKey)
		if len(apiKey) == 0 {
			apiKey = []byte(os.Getenv("POLARIS_EMBEDDING_API_KEY"))
		}
		adapter := llmadapter.NewOpenAICompatibleEmbeddingAdapter(
			cfg.Embedding.BaseURL,
			cfg.Embedding.Model,
			apiKey,
			safeHTTPClient,
		)
		dynEmbedder.Set(adapter)
		slog.Info("polaris: Remote OpenAI-compat embedding registered as fallback",
			"base_url", cfg.Embedding.BaseURL,
			"model", cfg.Embedding.Model,
		)
	}

	if autoConf != nil { //nolint:nestif
		if autoConf.Gate.State(probe.FeatureLocalInference) != probe.FeatureDisabled {
			localModel := autoConf.Config.LocalModelID
			if localModel == "" {
				localModel = "llama3.2"
			}
			reg.Register("ollama-local", "Local LLM", llmadapter.NewOllamaAdapter(localModel, ollamaHTTPClient, tbr))
			slog.Info("polaris: Ollama local inference registered", "model", localModel)

			// llama.cpp FFI 本地推理（P3-1）：与上面的 Ollama 路径并存，不互斥。
			// 惰性注册——不在启动时加载任何 GGUF 权重（无默认模型文件来源假设），
			// LoadModel 由调用方通过 reg.Get("llama-local") 类型断言为
			// protocol.LocalProvider 后显式触发（见 docs/arch/M01-Inference-Runtime.md §8）。
			// 仅当二进制以 --features tier1 构建（ffi.LlamaAvailable()）时才注册，
			// 避免 Tier-0/未编译 tier1 的二进制里出现一个必然报错的 Provider 条目。
			if ffi.LlamaAvailable() {
				reg.Register("llama-local", "Local LLM (llama.cpp FFI)", llmadapter.NewLocalAdapter())
				slog.Info("polaris: llama.cpp FFI local inference registered (unloaded, awaiting LoadModel)")
			}
		}

		if autoConf.Gate.State(probe.FeatureQLoRA) != probe.FeatureDisabled {
			qloraAdapter = llmadapter.NewQLoRAAdapter("", ollamaHTTPClient)
			slog.Info("polaris: QLoRA training adapter initialized")
		}
		if autoConf.Gate.State(probe.FeaturePRMTraining) != probe.FeatureDisabled {
			prmAdapter = llmadapter.NewPRMAdapter("", ollamaHTTPClient)
			slog.Info("polaris: PRM training adapter initialized")
		}
		if autoConf.Gate.State(probe.FeatureActivationSteer) != probe.FeatureDisabled {
			steeringAdapter = llmadapter.NewSteeringAdapter("", ollamaHTTPClient)
			cvStore = llmadapter.NewControlVectorStore()
			slog.Info("polaris: activation steering adapter initialized")
		}
		if autoConf.Gate.State(probe.FeatureLargeLocalLLM) != probe.FeatureDisabled {
			if largeModel, ok := probe.TierLocalModel(autoConf.Config.Tier); ok {
				reg.Register("ollama-large", "Large Local LLM", llmadapter.NewOllamaAdapter(largeModel, ollamaHTTPClient, tbr))
				slog.Info("polaris: large local LLM registered", "model", largeModel)
			}
		}

		// 2026-07-04 审计补齐（任务4）：注入 LocalModelUnloader，使 OSMemoryGuard
		// DegradationCritical 时能真正调用 UnloadModel 释放本地模型常驻内存，
		// 而不是只 Disable Gate + GC。*llm.ProviderRegistry 天然满足
		// observability.LocalModelUnloader 接口（Get(name) (protocol.Provider, bool)）。
		autoConf.WithLocalModelUnloader(reg)
		slog.Info("polaris: local model unloader wired into AutoConfig memory pressure callback")
	}

	gov := automation.NewResourceGovernor(cfg.System.MaxAgents, cfg.System.ResourceGovernor)
	if maxLLM := cfg.Thresholds.M13Interface.MaxConcurrentLLMCalls; maxLLM > 0 {
		gov.WithMaxConcurrentLLM(maxLLM)
	}

	var semanticCache *search.SemanticCache
	if surrealStore != nil {
		cacheStore := search.NewSurrealCacheStore(store, surrealStore)
		semanticCache = search.NewSemanticCache(
			cacheStore,
			embedder,
			"default", // namespace
			"",        // systemPromptHash (empty for default init, actual request keys have their own)
			cfg.Thresholds.M1Router.SemanticCacheSimilarity,
			cfg.Thresholds.M1Router.SemanticCacheMaxEntries,
			time.Duration(cfg.Thresholds.M1Router.SemanticCacheTTLHours)*time.Hour,
		)
	}

	routerOpts := []llm.RouterOption{llm.WithGovernor(gov)}
	if semanticCache != nil {
		routerOpts = append(routerOpts, llm.WithSemanticCache(semanticCache))
	}
	router := llm.NewInferenceRouter(reg, dialer, routerOpts...)
	slog.Info("polaris: inference router initialized")

	committed = true
	return &SubstrateBundle{
		Cfg:                      cfg,
		DataDir:                  dataDir,
		Layout:                   layout,
		AutoConf:                 autoConf,
		TBR:                      tbr,
		KS:                       ks,
		AuditTrail:               auditTrail,
		AuditChain:               auditChain,
		LogFile:                  logFile,
		LogStore:                 logStore,
		PromptMgr:                promptMgr,
		Store:                    store,
		SurrealStore:             surrealStore,
		StorageRouter:            storageRouter,
		DriftMonitor:             driftMonitor,
		DecisionLog:              decisionLog,
		Outbox:                   outboxWorker,
		DBWriter:                 dbWriter,
		DBWriterDone:             dbWriterDoneCh,
		Gate:                     gate,
		TrustMap:                 publisherTrustMap,
		Dialer:                   dialer,
		SafeHTTP:                 safeHTTPClient,
		InfReg:                   reg,
		Router:                   router,
		Embedder:                 embedder,
		DynEmbedder:              dynEmbedder,
		QLoRA:                    qloraAdapter,
		PRM:                      prmAdapter,
		Steering:                 steeringAdapter,
		CVStore:                  cvStore,
		Stop:                     stop,
		Vault:                    vault,
		RAGChunksTaintSerializer: ragChunksTaintSerializer,
	}, nil
}

// resolveDataDirBase 解析运行时数据根目录。
// 优先级：POLARIS_DATA_DIR env > cfg.System.DataDir > ~/.polarisagi/polaris
// 从 main.go 移入，消除 main.go 对 path/filepath/strings 的依赖。
func resolveDataDirBase(cfg *config.Config) (string, error) {
	dir := os.Getenv("POLARIS_DATA_DIR")
	if dir == "" && cfg != nil && cfg.System.DataDir != "" {
		dir = cfg.System.DataDir
	}
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", apperr.Wrap(apperr.CodeInternal,
				"cannot determine home directory; set POLARIS_DATA_DIR explicitly", err)
		}
		dir = filepath.Join(home, ".polarisagi/polaris")
	} else if strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", apperr.Wrap(apperr.CodeInternal,
				"cannot determine home directory for ~ expansion", err)
		}
		dir = filepath.Join(home, dir[2:])
	}
	return dir, nil
}

// loadGateCedarPolicies 读取 Cedar 策略内容并加载到 Gate。
// 优先级：磁盘覆盖路径（PolicyConfig.Hard/SoftConstraintsPath 非空时）> embed 内置默认策略。
// 三个策略文件均合并为一次 LoadPolicies 调用（Cedar 替换全局 PolicyStore）。
func loadGateCedarPolicies(gate *policy.Gate, cfg config.PolicyConfig) error {
	if cfg.Engine != "cedar" {
		return nil // 非 cedar 引擎，跳过
	}

	readContent := func(diskPath, embedPath string) (string, error) {
		if diskPath != "" {
			b, err := os.ReadFile(diskPath)
			if err != nil {
				return "", apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("cedar: read %s", diskPath), err)
			}
			return string(b), nil
		}
		b, err := configs.FS.ReadFile(embedPath)
		if err != nil {
			return "", apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("cedar: read embed %s", embedPath), err)
		}
		return string(b), nil
	}

	hard, err := readContent(cfg.HardConstraintsPath, "policy/hard_constraints.cedar")
	if err != nil {
		return err
	}
	soft, err := readContent(cfg.SoftConstraintsPath, "policy/soft_constraints.cedar")
	if err != nil {
		return err
	}
	// memory.cedar 仅从 embed 加载（无磁盘覆盖路径——内存工具权限不对外开放）
	memory, err := readContent("", "policy/memory.cedar")
	if err != nil {
		return err
	}

	combined := hard + "\n\n" + soft + "\n\n" + memory
	if err := gate.SyncCedarPolicies(combined); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "sync cedar policies failed", err)
	}
	return nil
}

// initSurrealStore 初始化 SurrealDB Core 认知存储。
//
// 内存分级策略（默认开启，按硬件自动降级）：
//
//	TotalRAM < 2GB  → 完全跳过 SurrealDB，cognitive axis 降级到纯 SQLite
//	2GB ≤ RAM < 4GB → 强制 "mem" 后端（无持久化，进程重启丢失）
//	TotalRAM ≥ 4GB  → 使用 config 配置值（默认 "rocksdb"，持久化）
//
// FeatureGate（可用 RAM < 512MB 时）提供运行时动态降级兜底。
func initSurrealStore(
	autoConf *observability.AutoConfig,
	cfg *config.Config,
	layout config.DataLayout,
) *sysstore.SurrealDBCoreStore {
	const (
		minRAMForSurreal   = 2 * 1024 * 1024 * 1024 // 2GB：以下完全跳过
		memBackendRAMLimit = 4 * 1024 * 1024 * 1024 // 4GB：以下强制 mem 后端
		largeRAMThreshold  = 8 * 1024 * 1024 * 1024 // 8GB：workerThreads 自动升档
	)

	if autoConf == nil {
		slog.Warn("polaris: AutoConfig 初始化失败，SurrealDB Core 跳过，cognitive axis → SQLite")
		return nil
	}

	// 硬件能力门控：< 2GB 物理内存直接跳过
	if autoConf.Probe.TotalRAM < minRAMForSurreal {
		slog.Info("polaris: SurrealDB Core skipped (TotalRAM < 2GB), cognitive axis → SQLite",
			"total_ram_mb", autoConf.Probe.TotalRAM/(1024*1024),
		)
		return nil
	}

	// FeatureGate 动态门控：可用内存压力时降级（运行时兜底）
	if autoConf.Gate.State(probe.FeatureSurrealDBCore) == probe.FeatureDisabled {
		slog.Info("polaris: SurrealDB Core disabled by FeatureGate (available memory pressure), cognitive axis → SQLite")
		return nil
	}

	// 确定实际后端：配置默认 "rocksdb"，低内存机器强制降级为 "mem"
	backend := cfg.Cognition.SurrealBackend
	if backend == "" {
		backend = "rocksdb"
	}
	if backend == "rocksdb" && autoConf.Probe.TotalRAM < memBackendRAMLimit {
		backend = "mem"
		slog.Info("polaris: SurrealDB backend downgraded rocksdb→mem (TotalRAM 2-4GB, no persistence)",
			"total_ram_mb", autoConf.Probe.TotalRAM/(1024*1024),
		)
	}

	dbPath := cfg.Cognition.SurrealDBPath
	if dbPath == "" {
		dbPath = layout.SurrealDB
	}

	vecDim := cfg.Inference.EmbedderDim
	// 本地 Ollama Embedding 启用时，向量维度由模型决定（768 或 1024），
	// 需覆盖 defaults.toml 中针对远程 API 设置的 1536。
	// LocalEmbeddingDim > 0 表示本地 Embedding 已选定，以其维度为 SurrealDB HNSW DIMENSION 权威值。
	if autoConf != nil && autoConf.Config.LocalEmbeddingDim > 0 {
		vecDim = autoConf.Config.LocalEmbeddingDim
	}
	if vecDim <= 0 {
		vecDim = 1536
	}

	// ≥ 8GB 时 workerThreads 自动升档以匹配 rocksdb I/O 并发需求
	workerThreads := cfg.Cognition.SurrealWorkerThreads
	if workerThreads <= 2 && autoConf.Probe.TotalRAM >= largeRAMThreshold {
		workerThreads = 0 // 0 = auto: min(CPU, 4)
	}

	st, err := sysstore.OpenSurrealDBCore(backend, dbPath, vecDim, workerThreads)
	if err != nil {
		slog.Warn("polaris: SurrealDB Core init failed, cognitive axis falls back to SQLite", "err", err)
		return nil
	}
	slog.Info("polaris: SurrealDB Core initialized",
		"backend", backend,
		"configured_backend", cfg.Cognition.SurrealBackend,
		"total_ram_gb", autoConf.Probe.TotalRAM/(1024*1024*1024),
		"vec_dim", vecDim,
		"worker_threads", workerThreads,
	)
	return st
}
