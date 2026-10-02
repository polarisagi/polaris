package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/llm"
	llmadapter "github.com/polarisagi/polaris/internal/llm/adapter"
	"github.com/polarisagi/polaris/internal/llm/embedassets"
	"github.com/polarisagi/polaris/internal/llm/embedonnx"
	"github.com/polarisagi/polaris/internal/llm/ollamamgr"
	"github.com/polarisagi/polaris/internal/memory/retrieval"
	"github.com/polarisagi/polaris/internal/observability/probe"
	"github.com/polarisagi/polaris/internal/security/network"
	"github.com/polarisagi/polaris/internal/store/search"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

type embedChoice struct {
	Kind  string // remote | ollama | llama_server | onnx | none
	Model string
	Dim   int
}

// chooseEmbedding 决定当前启动配置下使用的向量化引擎（ADR-0109 D1 唯一事实源）。
// 选择顺序严格为：
// 1. base_url 非空 -> remote (最高优先级，绝不再启动本地服务)
// 2. backend = "ollama" | "llama_server" 且 model 非空 -> 显式本地服务
// 3. backend = "auto" | "onnx" 且 onnxAvailable -> 进程内 ONNX 嵌入器 (默认 512 维)
// 4. 其他情况或 ONNX 不可用 -> none (纯 FTS 模式)
func chooseEmbedding(cfg config.EmbeddingConfig, inferDim int, onnxAvailable bool) embedChoice {
	if cfg.Backend == "none" {
		return embedChoice{Kind: "none", Model: "", Dim: 0}
	}

	// 1. 远程 API 绝对优先 (ADR-0109 D1)
	if cfg.BaseURL != "" {
		dim := inferDim
		if dim <= 0 {
			dim = 1536
		}
		return embedChoice{
			Kind:  "remote",
			Model: cfg.Model,
			Dim:   dim,
		}
	}

	// 2. 显式本地服务 (需显式指定 backend 为 ollama 或 llama_server)
	if cfg.Backend == "ollama" || cfg.Backend == "llama_server" {
		if cfg.Model == "" {
			return embedChoice{Kind: "none", Model: "", Dim: 0}
		}
		dim := cfg.Dim
		if dim <= 0 {
			dim = inferDim
		}
		if dim <= 0 {
			dim = 1024
		}
		return embedChoice{
			Kind:  cfg.Backend,
			Model: cfg.Model,
			Dim:   dim,
		}
	}

	// 3. 进程内 ONNX 嵌入器 (默认 auto 且未配远程时，或显式 onnx)
	if (cfg.Backend == "auto" || cfg.Backend == "onnx") && onnxAvailable {
		modelName := cfg.ONNXModel
		if modelName == "" || modelName == "auto" {
			modelName = "auto"
		}
		return embedChoice{
			Kind:  "onnx",
			Model: modelName,
			Dim:   512,
		}
	}

	// 4. 不挂嵌入器，检索降级 FTS
	return embedChoice{Kind: "none", Model: "", Dim: 0}
}

// localBackendEnabled 判断当前配置是否显式启用了本地推理后端 (ollama 或 llama_server)。
func localBackendEnabled(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	b := cfg.Embedding.Backend
	return b == "ollama" || b == "llama_server"
}

// initEmbedding 根据 chooseEmbedding 决议装配 Embedder、DynamicEmbedder 与 EmbeddingBatcher。
func initEmbedding(
	ctx context.Context,
	cfg *config.Config,
	layout config.DataLayout,
	safeHTTPClient network.SafeHTTPClient,
	backoff *retrieval.EmbedBackoff,
	db *sql.DB,
) (search.Embedder, *llm.DynamicEmbedder, *search.EmbeddingBatcher, embedChoice) {
	_, onnxAvailable := embedassets.ORTLibAsset(runtime.GOOS, runtime.GOARCH)
	choice := chooseEmbedding(cfg.Embedding, cfg.Inference.EmbedderDim, onnxAvailable)

	if cfg.Embedding.Backend == "onnx" && !onnxAvailable {
		slog.Warn("polaris: ONNX embedding backend requested but not supported on platform, falling back to FTS",
			"goos", runtime.GOOS, "goarch", runtime.GOARCH)
	}

	if choice.Kind != "remote" && choice.Kind != "ollama" && choice.Kind != "onnx" {
		if choice.Kind != "none" {
			slog.Warn("polaris: embedding backend not wired yet, running in pure FTS mode",
				"kind", choice.Kind)
		}
		slog.Info("polaris: embedding backend is none, running in pure FTS mode",
			"backend", cfg.Embedding.Backend)
		return nil, nil, nil, embedChoice{Kind: "none"}
	}

	dynEmbedder := llm.NewDynamicEmbedder()
	if backoff != nil {
		dynEmbedder.OnSet(backoff.Reset)
	}

	embedFn := func(ctxBg context.Context, texts []string, _ string) ([][]float32, error) {
		return dynEmbedder.EmbedBatch(ctxBg, texts)
	}

	m1 := cfg.Thresholds.M1Router
	batcher := search.NewEmbeddingBatcher(time.Duration(m1.EmbedBatchWindowMs)*time.Millisecond, m1.EmbedHighMaxBatchSize, embedFn).
		WithLaneLimits(m1.EmbedLowMaxBatchSize, time.Duration(m1.EmbedCallTimeoutSeconds)*time.Second)
	batcher.Start(context.WithoutCancel(ctx))
	embedder := search.NewSyncBatcherAdapter(batcher)

	switch choice.Kind {
	case "remote":
		setupRemoteEmbedding(cfg, choice, safeHTTPClient, dynEmbedder)
	case "ollama":
		setupOllamaEmbedding(cfg, choice, layout, safeHTTPClient, dynEmbedder)
	case "onnx":
		concurrent.SafeGo(context.WithoutCancel(ctx), "boot_embedding.onnx_init", func(ctxBg context.Context) {
			if err := runONNXEmbedding(ctxBg, cfg, layout, safeHTTPClient.Client, dynEmbedder, db, false); err != nil {
				slog.Warn("polaris: ONNX embedding background initialization failed", "err", err)
			}
		})
	}

	return embedder, dynEmbedder, batcher, choice
}

func setupRemoteEmbedding(
	cfg *config.Config,
	choice embedChoice,
	safeHTTPClient network.SafeHTTPClient,
	dynEmbedder *llm.DynamicEmbedder,
) {
	apiKey := cfg.Embedding.APIKey
	if apiKey == "" {
		apiKey = os.Getenv(config.EnvPolarisEmbeddingAPIKey)
	}
	var embedKeys []string
	if apiKey != "" {
		embedKeys = []string{apiKey}
	}
	adapter := llmadapter.NewOpenAICompatibleEmbeddingAdapter(
		cfg.Embedding.BaseURL,
		cfg.Embedding.Model,
		llm.NewCredentialPool(embedKeys, llm.StrategyFillFirst),
		safeHTTPClient.Client,
	)
	dynEmbedder.Set(adapter)
	slog.Info("polaris: Remote OpenAI-compatible embedding registered",
		"base_url", cfg.Embedding.BaseURL,
		"model", cfg.Embedding.Model,
		"dim", choice.Dim,
	)
}

func setupOllamaEmbedding(
	cfg *config.Config,
	choice embedChoice,
	layout config.DataLayout,
	safeHTTPClient network.SafeHTTPClient,
	dynEmbedder *llm.DynamicEmbedder,
) {
	targetModel := choice.Model
	ollamaHTTPClient := network.NewLoopbackSafeHTTPClient(cfg.Thresholds.M11Policy)
	concurrent.SafeGo(context.Background(), "boot_embedding.ollama_lifecycle", func(ctxBg context.Context) {
		slog.Info("polaris: Starting background Ollama lifecycle manager for explicit embedding...", "model", targetModel)
		binPath, err := ollamamgr.EnsureOllama(ctxBg, safeHTTPClient.Client, layout.Bin)
		if err != nil {
			slog.Error("polaris: Failed to install local Ollama", "err", err)
			return
		}
		loopbackClient := network.NewLoopbackSafeHTTPClient(cfg.Thresholds.M11Policy)
		_, err = ollamamgr.StartOllama(ctxBg, loopbackClient.Client, binPath)
		if err != nil {
			slog.Error("polaris: Failed to start local Ollama", "err", err)
			return
		}
		if err := ollamamgr.EnsureModel(ctxBg, binPath, targetModel); err != nil {
			slog.Error("polaris: Failed to pull embedding model", "err", err)
			return
		}
		adapter := llmadapter.NewOllamaEmbeddingAdapter(targetModel, ollamaHTTPClient.Client).
			WithNumThread(ollamamgr.RecommendedThreads())
		dynEmbedder.Set(adapter)
		slog.Info("polaris: Dynamic embedding engine is now ACTIVE!", "model", targetModel)
	})
}

// backgroundEmbedder 返回低优先级嵌入器，供批量/周期性后台工作使用。
// 合批器缺席时回退到默认 Embedder（可能为 nil，调用方本就需判空）。
func backgroundEmbedder(sb *SubstrateBundle) search.Embedder {
	if sb == nil || sb.EmbedBatcher == nil {
		if sb == nil {
			return nil
		}
		return sb.Embedder
	}
	return search.NewBackgroundEmbedder(sb.EmbedBatcher)
}

func triggerRebench(ctx context.Context, sb *SubstrateBundle) error {
	if sb == nil || sb.DynEmbedder == nil {
		return apperr.New(apperr.CodeInvalidInput, "embedding engine not initialized")
	}
	concurrent.SafeGo(context.WithoutCancel(ctx), "embedding.rebench", func(ctxBg context.Context) {
		if err := runONNXEmbedding(ctxBg, sb.Cfg, sb.Layout, sb.SafeHTTP, sb.DynEmbedder, sb.Store.DB(), true); err != nil {
			slog.Warn("polaris: embedding rebench failed", "err", err)
		}
	})
	return nil
}

func tryReuseSavedBench(
	ctx context.Context,
	db *sql.DB,
	httpClient *http.Client,
	embedDir string,
	api *embedonnx.ORTApi,
	dynEmbedder *llm.DynamicEmbedder,
) (bool, error) {
	saved, ok := loadSavedBench(ctx, db)
	if !ok {
		return false, nil
	}
	slog.Info("polaris: reusing existing embedding benchmark result", "tier", saved.Tier, "p95_ms", saved.P95Ms)
	switch saved.Tier {
	case embedonnx.TierBalanced:
		return true, loadGemma(ctx, httpClient, embedDir, api, dynEmbedder)
	case embedonnx.TierLight:
		return true, loadBGE(ctx, httpClient, embedDir, api, dynEmbedder)
	case embedonnx.TierNone:
		slog.Info("polaris: embedding benchmark previously determined FTS mode", "p95_ms", saved.P95Ms)
		return true, nil
	default:
		return false, nil
	}
}

func loadSavedBench(ctx context.Context, db *sql.DB) (embedonnx.TierBenchResult, bool) {
	if db == nil {
		return embedonnx.TierBenchResult{}, false
	}
	var val string
	row := db.QueryRowContext(ctx, "SELECT value FROM preferences WHERE key = ?", embedonnx.PrefKeyTierBench)
	if scanErr := row.Scan(&val); scanErr != nil || val == "" {
		return embedonnx.TierBenchResult{}, false
	}
	var saved embedonnx.TierBenchResult
	if jsonErr := json.Unmarshal([]byte(val), &saved); jsonErr != nil || saved.RetryOnStart {
		return embedonnx.TierBenchResult{}, false
	}
	return saved, true
}

func benchGemma(ctx context.Context, httpClient *http.Client, embedDir string, api *embedonnx.ORTApi) (float64, error) {
	gemmaModelPath, gemmaTokPath, err := embedassets.EnsureGemma(ctx, httpClient, embedDir)
	if err != nil {
		slog.Warn("polaris: failed to ensure Gemma assets for bench", "err", err)
		return 0, apperr.Wrap(apperr.CodeInternal, "polaris: ensure Gemma assets for bench failed", err)
	}
	gemmaSession, err := embedonnx.NewOrtSession(api, gemmaModelPath, true)
	if err != nil {
		slog.Warn("polaris: failed to create Gemma session for bench", "err", err)
		return 0, apperr.Wrap(apperr.CodeInternal, "polaris: create Gemma session for bench failed", err)
	}
	defer gemmaSession.Close()

	gemmaTok, err := embedonnx.NewSentencePieceTokenizer(gemmaTokPath)
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "polaris: create Gemma tokenizer for bench failed", err)
	}
	gemmaEngine := embedonnx.NewGemmaEngine(gemmaSession, gemmaTok)
	p95, err := embedonnx.MeasureP95(ctx, func(c context.Context, text string) error {
		vecs, err := gemmaEngine.EmbedBatch(c, []string{text})
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "gemma embed bench", err)
		}
		if len(vecs) == 0 {
			return apperr.New(apperr.CodeInternal, "empty vector in bench")
		}
		return nil
	})
	if err != nil {
		slog.Warn("polaris: Gemma bench measurement failed", "err", err)
		return 0, apperr.Wrap(apperr.CodeInternal, "polaris: Gemma bench measurement failed", err)
	}
	return p95, nil
}

func benchBGE(ctx context.Context, httpClient *http.Client, embedDir string, api *embedonnx.ORTApi) (float64, error) {
	bgeModelPath, bgeVocabPath, err := embedassets.EnsureBGE(ctx, httpClient, embedDir)
	if err != nil {
		slog.Warn("polaris: failed to ensure BGE assets for bench", "err", err)
		return 0, apperr.Wrap(apperr.CodeInternal, "polaris: ensure BGE assets for bench failed", err)
	}
	bgeSession, err := embedonnx.NewOrtSession(api, bgeModelPath, false)
	if err != nil {
		slog.Warn("polaris: failed to create BGE session for bench", "err", err)
		return 0, apperr.Wrap(apperr.CodeInternal, "polaris: create BGE session for bench failed", err)
	}
	defer bgeSession.Close()

	bgeTok, err := embedonnx.NewWordPieceTokenizer(bgeVocabPath)
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "polaris: create BGE tokenizer for bench failed", err)
	}
	bgeEngine := embedonnx.NewBGEEngine(bgeSession, bgeTok)
	p95, err := embedonnx.MeasureP95(ctx, func(c context.Context, text string) error {
		vecs, err := bgeEngine.EmbedBatch(c, []string{text})
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "bge embed bench", err)
		}
		if len(vecs) == 0 {
			return apperr.New(apperr.CodeInternal, "empty vector in bench")
		}
		return nil
	})
	if err != nil {
		slog.Warn("polaris: BGE bench measurement failed", "err", err)
		return 0, apperr.Wrap(apperr.CodeInternal, "polaris: BGE bench measurement failed", err)
	}
	return p95, nil
}

func runONNXEmbedding(
	ctx context.Context,
	cfg *config.Config,
	layout config.DataLayout,
	httpClient *http.Client,
	dynEmbedder *llm.DynamicEmbedder,
	db *sql.DB,
	forceRebench bool,
) error {
	if !forceRebench {
		// 启动后延迟 60 秒开始（避开启动期与语音预置的 45 秒窗口，ADR-0109 D8）
		select {
		case <-ctx.Done():
			return apperr.Wrap(apperr.CodeInternal, "polaris: context cancelled waiting for ONNX embedder start", ctx.Err())
		case <-time.After(60 * time.Second):
		}
	}

	embedDir := filepath.Join(layout.Models, "embed")
	dylibPath, err := embedassets.EnsureORT(ctx, httpClient, embedDir, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		slog.Warn("polaris: failed to ensure ORT library for embedding", "err", err)
		return apperr.Wrap(apperr.CodeInternal, "polaris: ensure ORT library failed", err)
	}
	api, err := embedonnx.OpenORT(dylibPath)
	if err != nil {
		slog.Warn("polaris: failed to load ORT library for embedding", "err", err)
		return apperr.Wrap(apperr.CodeInternal, "polaris: load ORT library failed", err)
	}

	// 1. 如果用户显式指定了模型，跳过基准测定直接加载 (PROMPT.md §3 P2-7)
	if cfg.Embedding.ONNXModel == "embeddinggemma" {
		return loadGemma(ctx, httpClient, embedDir, api, dynEmbedder)
	}
	if cfg.Embedding.ONNXModel == "bge-small-zh" {
		return loadBGE(ctx, httpClient, embedDir, api, dynEmbedder)
	}

	// 2. 自动选档：如果已有落库基准且非 forceRebench 且无需重试，直接复用 (PROMPT.md §2 第 5 条)
	if !forceRebench {
		reused, reuseErr := tryReuseSavedBench(ctx, db, httpClient, embedDir, api, dynEmbedder)
		if reused {
			return reuseErr
		}
	}

	// 3. 执行基准测定
	slog.Info("polaris: starting ONNX embedding tier benchmark...")
	totalRAM, availRAM := probe.MemoryProbe()
	memTotalMB := totalRAM / (1024 * 1024)
	memAvailMB := availRAM / (1024 * 1024)

	// 内存下限检查：总内存 < 1.5GB 或可用 < 600MB → 直接轻量档
	if (memTotalMB > 0 && memTotalMB < 1536) || (memAvailMB > 0 && memAvailMB < 600) {
		slog.Info("polaris: memory below lower bound, selecting lightweight tier directly", "total_mb", memTotalMB, "avail_mb", memAvailMB)
		recordBenchResult(ctx, db, embedonnx.TierBenchResult{
			Tier:         embedonnx.TierLight,
			ModelVersion: embedonnx.ModelVersionBGE,
			MeasuredAt:   time.Now(),
		})
		return loadBGE(ctx, httpClient, embedDir, api, dynEmbedder)
	}

	return executeBenchmarkAndApply(ctx, httpClient, embedDir, api, dynEmbedder, db, memTotalMB, memAvailMB)
}

func executeBenchmarkAndApply(
	ctx context.Context,
	httpClient *http.Client,
	embedDir string,
	api *embedonnx.ORTApi,
	dynEmbedder *llm.DynamicEmbedder,
	db *sql.DB,
	memTotalMB, memAvailMB uint64,
) error {
	cpuSampler := probe.NewCPUSampler()
	cpuBefore := cpuSampler.Usage()
	idleOK := embedonnx.WaitForCPUIdle(ctx, cpuSampler.Usage, 10*time.Minute)
	contended := !idleOK

	gemmaP95, err := benchGemma(ctx, httpClient, embedDir, api)
	if err != nil {
		return err
	}

	bgeP95, err := benchBGE(ctx, httpClient, embedDir, api)
	if err != nil {
		return err
	}

	cpuAfter := cpuSampler.Usage()
	decision := embedonnx.DecideTier(memTotalMB, memAvailMB, gemmaP95, bgeP95, contended)
	decision.CPUBefore = cpuBefore
	decision.CPUAfter = cpuAfter

	slog.Info("polaris: embedding tier benchmark completed",
		"tier", decision.Tier,
		"gemma_p95_ms", gemmaP95,
		"bge_p95_ms", bgeP95,
		"contended", decision.Contended,
		"retry_on_start", decision.RetryOnStart,
	)

	recordBenchResult(ctx, db, decision)

	switch decision.Tier {
	case embedonnx.TierBalanced:
		return loadGemma(ctx, httpClient, embedDir, api, dynEmbedder)
	case embedonnx.TierLight:
		return loadBGE(ctx, httpClient, embedDir, api, dynEmbedder)
	default:
		slog.Info("polaris: benchmark exceeded latency limits, running in FTS mode")
		return nil
	}
}

func loadGemma(
	ctx context.Context,
	httpClient *http.Client,
	embedDir string,
	api *embedonnx.ORTApi,
	dynEmbedder *llm.DynamicEmbedder,
) error {
	modelPath, tokPath, err := embedassets.EnsureGemma(ctx, httpClient, embedDir)
	if err != nil {
		slog.Warn("polaris: failed to ensure Gemma assets", "err", err)
		return apperr.Wrap(apperr.CodeInternal, "polaris: ensure Gemma assets failed", err)
	}
	managed := embedonnx.NewManagedEngine(embedonnx.ModelVersionGemma, func(c context.Context) (*embedonnx.EmbedEngine, error) {
		session, sessErr := embedonnx.NewOrtSession(api, modelPath, true)
		if sessErr != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "polaris: create Gemma session failed", sessErr)
		}
		tok, tokErr := embedonnx.NewSentencePieceTokenizer(tokPath)
		if tokErr != nil {
			session.Close()
			return nil, apperr.Wrap(apperr.CodeInternal, "polaris: create Gemma tokenizer failed", tokErr)
		}
		return embedonnx.NewGemmaEngine(session, tok), nil
	})
	dynEmbedder.Set(managed)
	slog.Info("polaris: Dynamic embedding engine ACTIVE", "model", embedonnx.ModelVersionGemma, "dim", 512)
	return nil
}

func loadBGE(
	ctx context.Context,
	httpClient *http.Client,
	embedDir string,
	api *embedonnx.ORTApi,
	dynEmbedder *llm.DynamicEmbedder,
) error {
	modelPath, vocabPath, err := embedassets.EnsureBGE(ctx, httpClient, embedDir)
	if err != nil {
		slog.Warn("polaris: failed to ensure BGE assets", "err", err)
		return apperr.Wrap(apperr.CodeInternal, "polaris: ensure BGE assets failed", err)
	}
	managed := embedonnx.NewManagedEngine(embedonnx.ModelVersionBGE, func(c context.Context) (*embedonnx.EmbedEngine, error) {
		session, sessErr := embedonnx.NewOrtSession(api, modelPath, false)
		if sessErr != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "polaris: create BGE session failed", sessErr)
		}
		tok, tokErr := embedonnx.NewWordPieceTokenizer(vocabPath)
		if tokErr != nil {
			session.Close()
			return nil, apperr.Wrap(apperr.CodeInternal, "polaris: create BGE tokenizer failed", tokErr)
		}
		return embedonnx.NewBGEEngine(session, tok), nil
	})
	dynEmbedder.Set(managed)
	slog.Info("polaris: Dynamic embedding engine ACTIVE", "model", embedonnx.ModelVersionBGE, "dim", 512)
	return nil
}

func recordBenchResult(ctx context.Context, db *sql.DB, result embedonnx.TierBenchResult) {
	if db == nil {
		return
	}
	b, err := json.Marshal(result)
	if err != nil {
		return
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO preferences (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		embedonnx.PrefKeyTierBench, string(b),
	); err != nil {
		slog.Warn("polaris: failed to persist embedding bench result", "err", err)
	}
}
