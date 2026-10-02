package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/llm"
	llmadapter "github.com/polarisagi/polaris/internal/llm/adapter"
	"github.com/polarisagi/polaris/internal/llm/ollamamgr"
	"github.com/polarisagi/polaris/internal/memory/retrieval"
	"github.com/polarisagi/polaris/internal/security/network"
	"github.com/polarisagi/polaris/internal/store/search"
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
// 3. backend = "auto" | "onnx" -> 进程内 ONNX 嵌入器 (P1 阶段尚未实现回退 none)
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
		return embedChoice{
			Kind:  cfg.Backend,
			Model: cfg.Model,
			Dim:   dim,
		}
	}

	// 3. 进程内 ONNX 嵌入器 (默认 auto 或显式 onnx)
	if cfg.Backend == "auto" || cfg.Backend == "onnx" || cfg.Backend == "" {
		if onnxAvailable {
			model := cfg.ONNXModel
			if model == "" {
				model = "auto"
			}
			return embedChoice{
				Kind:  "onnx",
				Model: model,
				Dim:   512,
			}
		}
		return embedChoice{
			Kind:  "none",
			Model: "",
			Dim:   0,
		}
	}

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
) (search.Embedder, *llm.DynamicEmbedder, *search.EmbeddingBatcher, embedChoice) {
	// P1 阶段 ONNX 嵌入器尚未实现
	const onnxAvailable = false
	choice := chooseEmbedding(cfg.Embedding, cfg.Inference.EmbedderDim, onnxAvailable)

	if cfg.Embedding.Backend == "onnx" && !onnxAvailable {
		slog.Warn("polaris: ONNX embedding backend requested but not yet available in P1, falling back to FTS")
	}

	// 只有 remote / ollama 两种引擎已接线；其余决议（none，以及尚未实现的 onnx / llama_server）
	// 一律不挂嵌入器，走 FTS。不得返回"有 DynamicEmbedder 却永远没有引擎"的半成品——
	// 那会让检索与重嵌每次都失败而非降级。
	if choice.Kind != "remote" && choice.Kind != "ollama" {
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

	case "ollama":
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

	return embedder, dynEmbedder, batcher, choice
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
