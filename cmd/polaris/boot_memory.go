// boot_memory.go — §4.10~§5 启动阶段：
// M5 记忆系统 → OnlineReindexer → CascadeInvalidator → TemporalExpirer → MEMF。
// MemoryBundle 持有所有认知记忆产物，向 boot_tools/boot_knowledge/boot_agent 传递。
package main

import (
	"encoding/json"

	"github.com/polarisagi/polaris/internal/observability/probe"
	"github.com/polarisagi/polaris/pkg/concurrent"

	"context"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/prompt/optimizer"

	"github.com/polarisagi/polaris/internal/memory/graph"
	"github.com/polarisagi/polaris/internal/memory/retrieval"

	"github.com/polarisagi/polaris/internal/llm/modelregistry"
	"github.com/polarisagi/polaris/internal/memory"
	storerepo "github.com/polarisagi/polaris/internal/store/repo"
)

// MemoryBundle 持有 §4.10~§5 所有认知记忆产物。
type MemoryBundle struct {
	Mem                *memory.MemImpl
	CascadeInvalidator *retrieval.CascadeInvalidator
	WriteFilter        *retrieval.WriteFilter
	FallacyPool        *optimizer.FallacyMemoryPool
	Heuristics         *optimizer.HeuristicsMemory
	// ModelRegistry P3-2 ModelVersionRegistry：模型版本/废弃/兼容性评分管理。
	ModelRegistry *modelregistry.Registry
}

// bootMemory 执行 §4.10~§5 初始化，返回记忆系统 bundle。
func bootMemory(ctx context.Context, sb *SubstrateBundle) (*MemoryBundle, error) {
	// ─── §4.10 M5 记忆系统（SurrealDB 可用时走全路径，否则 Tier0 降级）─────
	var mem *memory.MemImpl
	if sb.SurrealStore != nil {
		// Tier0+(≥8GB)：FTS+HNSW+Graph+SQL 全路径
		cogn := &surrealCognAdapter{s: sb.SurrealStore}
		mem = memory.NewMemImplFull(sb.Store, sb.SurrealStore, cogn, sb.Store.DB())
		slog.Info("polaris: memory initialized with SurrealDB cognitive axis (Tier0+/≥8GB)")
	} else {
		// <8GB VPS 降级：纯 Go BM25 + SQLite BLOB 余弦
		mem = memory.NewMemImplWithDB(sb.Store, sb.Store.DB())
		slog.Info("polaris: memory initialized in fallback mode (SQLite-only, SurrealDB disabled, <8GB VPS)")
	}

	// 统一注入 Embedder 激活向量检索路径（P0-3 修复）
	if sb.Embedder != nil {
		embedModelName := "nomic-embed-text"
		if sb.AutoConf != nil && sb.AutoConf.Config.LocalEmbeddingModel != "" {
			embedModelName = sb.AutoConf.Config.LocalEmbeddingModel
		}
		mem.InjectEmbedder(&memEmbedderAdapter{e: sb.Embedder, model: embedModelName})
		slog.Info("polaris: vector retrieval path activated")
	}

	// ─── §4.10.5 OnlineReindexer（后台异步 Embedding 版本漂移修复）──────────
	// embedder 非 nil 时才启动（FeatureLocalEmbedding 已开启），否则 Tier0 走纯 BM25。
	reindexNow := startOnlineReindexer(ctx, sb)

	// ─── §9 P3-2 ModelVersionRegistry（模型版本/废弃/兼容性评分管理）────────
	// reindexNow 为 nil 时（Embedder 未启用）DeprecateModel 跳过重嵌唤醒，
	// 正确性不受影响——纯 BM25 路径本就不依赖 Embedding 版本。
	modelReg := modelregistry.NewRegistry(
		storerepo.NewSQLiteModelVersionRepository(sb.Store.DB()),
		modelregistry.WithReindexTrigger(func(triggerCtx context.Context, provider, modelID string) error {
			if reindexNow == nil {
				return nil
			}
			_, _, err := reindexNow(triggerCtx)
			return err
		}),
	)
	if err := modelReg.SeedFromStaticResolvers(ctx); err != nil {
		slog.Warn("polaris: model version registry seed failed", "err", err)
	} else {
		slog.Info("polaris: model version registry initialized (seeded from resolveXXXModel() static mappings)")
	}
	// Registry.RecordCallResult 此前虽已完整实现（连续失败计数 + FindPredecessor
	// 回退建议），但路由层从未持有过 Registry 实例，数据一直是空的。
	if sb.Router != nil {
		sb.Router.InjectModelRegistry(modelReg)
	}

	// ─── §4.10.6 CascadeInvalidator（belief revision 后级联失效，M5 §6）────
	cascadeInvalidator := retrieval.NewCascadeInvalidator(sb.Store.DB())
	slog.Info("polaris: cascade invalidator initialized")

	// ─── §4.10.7 TemporalExpirer（定期过期 valid_until 到期的语义实体，每小时）
	startTemporalExpirer(ctx, sb)
	slog.Info("polaris: inference router and memory initialized")

	// ─── §5 WriteFilter（LLM 驱动写入前价值评估，MemReader 2604.07877）───────
	// provider 可为 nil（WriteFilter 自动降级到启发式评估）
	writeFilter := retrieval.NewWriteFilter(sb.Router)

	// ─── §5.x MEMF + 启发式记忆（M9 内环基础）──────────────────────────────
	fallacyPool := optimizer.NewFallacyMemoryPool(sb.Store.DB())
	heuristics := optimizer.NewHeuristicsMemory(sb.Store.DB())
	slog.Info("polaris: MEMF and heuristics memory initialized")

	// 抑制未使用的 observability 引用（FeatureGate 在 autoConf 路径中已消费）
	_ = probe.FeatureLocalEmbedding

	// ─── G-1: SurrealDB mem 后端 boot 重放器 ─────────────────────────────────
	startCognitiveReplayerIfNeeded(ctx, sb)
	// ───────────────────────────────────────────────────────────────────────

	return &MemoryBundle{
		Mem:                mem,
		CascadeInvalidator: cascadeInvalidator,
		WriteFilter:        writeFilter,
		FallacyPool:        fallacyPool,
		Heuristics:         heuristics,
		ModelRegistry:      modelReg,
	}, nil
}

// startOnlineReindexer 启动后台 5min 周期重嵌 goroutine，返回一个可供外部
// （P3-2 ModelVersionRegistry.DeprecateModel 唤醒重嵌）立即触发一次的闭包；
// Embedder 未启用时返回 nil（调用方需判空——纯 BM25 路径本就不需要重嵌）。
func startOnlineReindexer(ctx context.Context, sb *SubstrateBundle) func(context.Context) (int, bool, error) {
	if sb.Embedder == nil {
		return nil
	}
	embedModelName := "nomic-embed-text"
	if sb.AutoConf != nil && sb.AutoConf.Config.LocalEmbeddingModel != "" {
		embedModelName = sb.AutoConf.Config.LocalEmbeddingModel
	}
	var onlineReindexer *retrieval.OnlineReindexer
	if sb.SurrealStore != nil {
		onlineReindexer = retrieval.NewOnlineReindexerWithCognitive(
			sb.Store.DB(),
			&memEmbedderAdapter{e: sb.Embedder, model: embedModelName},
			&surrealCognAdapter{s: sb.SurrealStore},
		)
	} else {
		onlineReindexer = retrieval.NewOnlineReindexer(
			sb.Store.DB(),
			&memEmbedderAdapter{e: sb.Embedder, model: embedModelName},
		)
	}
	concurrent.SafeGo(ctx, "boot_memory.reindex_ticker", func(ctx context.Context) {
		reindexTicker := time.NewTicker(5 * time.Minute)
		defer reindexTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-reindexTicker.C:
				if _, _, err := onlineReindexer.Run(ctx); err != nil {
					slog.Warn("polaris: online reindexer failed", "err", err)
				}
			}
		}
	})
	slog.Info("polaris: online reindexer started", "model", embedModelName, "interval", "5m")
	return onlineReindexer.Run
}

func startTemporalExpirer(ctx context.Context, sb *SubstrateBundle) {
	temporalExpirer := graph.NewTemporalExpirer(sb.Store.DB())
	concurrent.SafeGo(ctx, "boot_memory.temporal_expirer", func(ctx context.Context) {
		expireTicker := time.NewTicker(1 * time.Hour)
		defer expireTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-expireTicker.C:
				if expired, err := temporalExpirer.ExpireStale(ctx); err != nil {
					slog.Warn("polaris: temporal expirer failed", "err", err)
				} else if expired > 0 {
					slog.Info("polaris: temporal expirer: expired entities", "count", expired)
				}
			}
		}
	})
	slog.Info("polaris: temporal expirer started", "interval", "1h")
}

func startCognitiveReplayerIfNeeded(ctx context.Context, sb *SubstrateBundle) {
	if sb.SurrealStore == nil {
		return
	}

	statsJSON, err := sb.SurrealStore.Stats()
	if err != nil {
		slog.Warn("polaris: failed to get SurrealStore stats for replayer check", "err", err)
		return
	}

	var stats map[string]interface{}
	if err := json.Unmarshal([]byte(statsJSON), &stats); err != nil {
		return
	}

	// 键名与 Rust surreal_stats 返回的 JSON 对齐（doc_count，见 fts.rs surreal_stats）。
	// 注意 backend 字段恒为 "surreal"（不区分 mem/rocksdb），不能作为判据；
	// 统一判据：认知轴 FTS 为空 && SQLite 真相源非空 → 需要重放
	// （覆盖 kv-mem 重启丢失与 rocksdb 空库两种场景，FTSIndex 为幂等 UPSERT，重放安全）。
	docCountF, _ := stats["doc_count"].(float64)

	shouldReplay := false
	if docCountF == 0 {
		var episodicCount int
		if err := sb.Store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM episodic_events").Scan(&episodicCount); err == nil && episodicCount > 0 {
			shouldReplay = true
		}
	}

	if shouldReplay {
		slog.Info("polaris: starting cognitive replayer for surrogate index recovery")
		replayer := retrieval.NewCognitiveReplayer(sb.Store.DB(), &surrealCognAdapter{s: sb.SurrealStore})
		if err := replayer.Start(ctx); err != nil {
			slog.Error("polaris: failed to start CognitiveReplayer", "err", err)
		}
	}
}
