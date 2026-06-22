// boot_substrate.go — §0.5~§4 启动阶段：
// 配置加载 → 数据目录 → 硬件探针 → KillSwitch → 日志 → 存储层 → 策略引擎 → 推理路由器。
// SubstrateBundle 持有所有 L0 基础设施产物，向上层 boot 函数传递。
//
// resolveDataDirBase 和 initSurrealStore 从 main.go 移入，消除 main.go 对 storage/observability 的直接依赖。
package main

import (
	"github.com/polarisagi/polaris/internal/security/policy"

	"github.com/polarisagi/polaris/internal/security/network"

	"github.com/polarisagi/polaris/internal/observability/probe"

	"github.com/polarisagi/polaris/internal/observability/metrics"

	"context"
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
	"github.com/polarisagi/polaris/internal/eval"
	"github.com/polarisagi/polaris/internal/gateway/egress"
	"github.com/polarisagi/polaris/internal/gateway/server"
	"github.com/polarisagi/polaris/internal/gateway/server/provider"
	"github.com/polarisagi/polaris/internal/llm"
	llmadapter "github.com/polarisagi/polaris/internal/llm/adapter"
	"github.com/polarisagi/polaris/internal/observability"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/internal/security"
	sysstore "github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/internal/store/audit"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/internal/store/search"
	"github.com/polarisagi/polaris/internal/sysmgr/downloader"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// SubstrateBundle 持有 §0.5~§4 所有 L0 基础设施产物。
// 向所有上层 boot 函数传递，避免大量参数列表。
type SubstrateBundle struct {
	// 配置与数据目录
	Cfg     *config.Config
	DataDir string
	Layout  config.DataLayout

	// 硬件探针与可观测性（AutoConf 可 nil，Tier0 降级）
	AutoConf *observability.AutoConfig
	TBR      *metrics.TokenBurnRate

	// 安全三件套
	KS         *security.KillSwitch
	AuditTrail *security.AuditTrail

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

	// 策略引擎
	Gate     *policy.Gate
	TrustMap map[string]int

	// 网络安全层
	Dialer   *network.SafeDialer
	SafeHTTP *http.Client

	// 推理层
	InfReg   *llm.ProviderRegistry
	Router   *llm.InferenceRouter
	Embedder search.Embedder // 可 nil（FeatureLocalEmbedding 未启用）

	// 训练适配器（门控，M9 流水线消费；当前作占位）
	QLoRA    *llmadapter.QLoRAAdapter
	PRM      *llmadapter.PRMAdapter
	Steering *llmadapter.SteeringAdapter

	// OTA restart fn 捕获：取消主 ctx + 等待 dbWriter + 关闭 store
	Stop context.CancelFunc
}

// bootSubstrate 执行 §0.5~§4 初始化，返回 L0 基础设施 bundle。
// stop 来自 run() 的 signal.NotifyContext，注入 bundle 供 OTA restart fn 使用。
func bootSubstrate(ctx context.Context, stop context.CancelFunc) (*SubstrateBundle, error) { //nolint:gocyclo
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

	// ─── 0.3.5 Thresholds 覆盖加载 ──────────────────────────────────────────
	thresholds, err := config.LoadThresholds(dataDir)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "config.LoadThresholds", err)
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
	ks.StateChangeCallback = func(newState security.KillState, _ string) {
		metrics.GlobalKillswitchStage.Store(int32(newState))
	}

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
	}

	// ─── 0.6 TBR 心跳 goroutine ─────────────────────────────────────────────
	go func() {
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
	}()

	// ─── 0.7 内存压力监控（每 5s 轮询，驱动 FeatureGate 运行时降级）──────────
	go autoConf.RunMemoryWatcher(ctx)
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
	outboxWorker := sysstore.NewOutboxWorker(store.DB(), 5, 3)
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

	// ─── 2.8 DatabaseWriter（AI 核心数据单写者）────────────────────────────
	dbWriter := sysstore.NewDatabaseWriter(store.DB(), nil)
	dbWriterDoneCh := make(chan struct{})
	go func() {
		dbWriter.Run(ctx)
		close(dbWriterDoneCh)
	}()
	eventLog := audit.NewSQLiteEventLog(dbWriter)
	decisionLog := audit.NewSQLiteDecisionLog(dbWriter)
	_ = eventLog    // 待 M4 Agent Kernel 注入
	_ = decisionLog // 待 M3 观测层注入
	slog.Info("polaris: mutation bus (database writer) started")

	// ─── 2.9 AuditTrail ──────────────────────────────────────────────────────
	auditRepo := repo.NewSQLiteAuditRepository(store.DB())
	auditTrail := security.NewAuditTrail(auditRepo, layout.AuditArchive)
	if err := auditTrail.RecoverOnStartup(); err != nil {
		slog.Error("polaris: AuditTrail recovery failed", "err", err)
		return nil, err
	}
	slog.Info("polaris: audit trail recovered and initialized")

	// ─── 月度成本报告 (M13 §1.1) ─────────────────────────────────────────────
	automation.StartMonthlyCostReport(ctx, layout.Reports, store.DB())

	// ─── 3. 策略引擎 (L0 PolicyGate) ────────────────────────────────────────
	gate := policy.NewGate(func() {
		slog.Error("polaris: POLICY GATE HITL TRIGGERED — human review required",
			"component", "policy_gate",
			"action", "hitl_callback",
			"note", "wire hitlGateway.Notify here after gateway initialization",
		)
	})
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
	publisherTrustMap := config.LoadTrustedPublishers(configs.FS, "extensions/trusted-publishers.yaml")

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
	provider.SeedProvidersFromEnv(ctx, repo.NewSQLiteProviderRepository(store.DB()))

	// ─── 4.5~4.9 本地推理适配器（各 FeatureGate 门控）────────────────────────
	var embedder search.Embedder
	var qloraAdapter *llmadapter.QLoRAAdapter
	var prmAdapter *llmadapter.PRMAdapter
	var steeringAdapter *llmadapter.SteeringAdapter
	if autoConf != nil { //nolint:nestif
		if autoConf.Gate.State(probe.FeatureLocalInference) != probe.FeatureDisabled {
			localModel := autoConf.Config.LocalModelID
			if localModel == "" {
				localModel = "llama3.2"
			}
			reg.Register("ollama-local", "Local LLM", llmadapter.NewOllamaAdapter(localModel, safeHTTPClient, tbr))
			slog.Info("polaris: Ollama local inference registered", "model", localModel)
		}
		if autoConf.Gate.State(probe.FeatureLocalEmbedding) != probe.FeatureDisabled {
			embedModel := autoConf.Config.LocalEmbeddingModel
			if embedModel == "" {
				embedModel = "nomic-embed-text"
			}
			embedder = llmadapter.NewOllamaEmbeddingAdapter(embedModel, safeHTTPClient)
			slog.Info("polaris: Ollama embedding registered", "model", embedModel)
		}
		if autoConf.Gate.State(probe.FeatureQLoRA) != probe.FeatureDisabled {
			qloraAdapter = llmadapter.NewQLoRAAdapter("", safeHTTPClient)
			slog.Info("polaris: QLoRA training adapter initialized")
		}
		if autoConf.Gate.State(probe.FeaturePRMTraining) != probe.FeatureDisabled {
			prmAdapter = llmadapter.NewPRMAdapter("", safeHTTPClient)
			slog.Info("polaris: PRM training adapter initialized")
		}
		if autoConf.Gate.State(probe.FeatureActivationSteer) != probe.FeatureDisabled {
			steeringAdapter = llmadapter.NewSteeringAdapter("", safeHTTPClient)
			slog.Info("polaris: activation steering adapter initialized")
		}
		if autoConf.Gate.State(probe.FeatureLargeLocalLLM) != probe.FeatureDisabled {
			if largeModel, ok := probe.TierLocalModel(autoConf.Config.Tier); ok {
				reg.Register("ollama-large", "Large Local LLM", llmadapter.NewOllamaAdapter(largeModel, safeHTTPClient, tbr))
				slog.Info("polaris: large local LLM registered", "model", largeModel)
			}
		}
	}

	router := llm.NewInferenceRouter(reg, dialer)
	slog.Info("polaris: inference router initialized")

	committed = true
	return &SubstrateBundle{
		Cfg:           cfg,
		DataDir:       dataDir,
		Layout:        layout,
		AutoConf:      autoConf,
		TBR:           tbr,
		KS:            ks,
		AuditTrail:    auditTrail,
		LogFile:       logFile,
		LogStore:      logStore,
		Store:         store,
		SurrealStore:  surrealStore,
		StorageRouter: storageRouter,
		Outbox:        outboxWorker,
		DBWriter:      dbWriter,
		DBWriterDone:  dbWriterDoneCh,
		Gate:          gate,
		TrustMap:      publisherTrustMap,
		Dialer:        dialer,
		SafeHTTP:      safeHTTPClient,
		InfReg:        reg,
		Router:        router,
		Embedder:      embedder,
		QLoRA:         qloraAdapter,
		PRM:           prmAdapter,
		Steering:      steeringAdapter,
		Stop:          stop,
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
	return gate.LoadCedarPolicies(combined)
}

// initSurrealStore 初始化 SurrealDB Core 认知存储，提取独立函数以降低嵌套复杂度。
// backend: "mem"（默认，256MB+ 可用）/ "rocksdb"（持久化，推荐大内存服务器）。
// workerThreads: <= 0 = auto（min(CPU, 4)）；VPS 推荐在 config 中设 2 以节省内存。
// 从 main.go 移入，与调用方 bootSubstrate 共存于同一文件。
func initSurrealStore(
	autoConf *observability.AutoConfig,
	cfg *config.Config,
	layout config.DataLayout,
) *sysstore.SurrealDBCoreStore {
	if autoConf == nil {
		slog.Warn("polaris: AutoConfig 初始化失败，SurrealDB Core 跳过，cognitive axis → SQLite")
		return nil
	}
	if autoConf.Gate.State(probe.FeatureSurrealDBCore) == probe.FeatureDisabled {
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

	// TotalRAM ≥ 8GB 自动从 mem 升级到 rocksdb——持久化认知轴，无需手动改配置。
	const rocksdbAutoThreshold = 8 * 1024 * 1024 * 1024 // 8 GB
	autoBackend := backend
	if backend == "mem" && autoConf.Probe.TotalRAM >= rocksdbAutoThreshold {
		autoBackend = "rocksdb"
		slog.Info("polaris: SurrealDB backend auto-upgraded mem→rocksdb (TotalRAM ≥ 8GB)",
			"total_ram_gb", autoConf.Probe.TotalRAM/(1024*1024*1024),
			"db_path", dbPath,
		)
	}

	vecDim := cfg.Inference.EmbedderDim
	if vecDim <= 0 {
		vecDim = 1536
	}
	workerThreads := cfg.Cognition.SurrealWorkerThreads
	if workerThreads <= 2 && autoConf.Probe.TotalRAM >= rocksdbAutoThreshold {
		workerThreads = 0 // 0 = auto: min(CPU, 4)
	}
	st, err := sysstore.OpenSurrealDBCore(autoBackend, dbPath, vecDim, workerThreads)
	if err != nil {
		slog.Warn("polaris: SurrealDB Core init failed, cognitive axis falls back to SQLite", "err", err)
		return nil
	}
	slog.Info("polaris: SurrealDB Core initialized",
		"backend", autoBackend,
		"configured_backend", backend,
		"vec_dim", vecDim,
		"worker_threads", workerThreads,
	)
	return st
}
