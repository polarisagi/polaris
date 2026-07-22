// boot_memory.go — §4.10~§5 启动阶段：
// M5 记忆系统 → OnlineReindexer → CascadeInvalidator → TemporalExpirer → MEMF。
// MemoryBundle 持有所有认知记忆产物，向 boot_tools/boot_knowledge/boot_agent 传递。
package main

import (
	"encoding/json"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/observability/probe"
	"github.com/polarisagi/polaris/pkg/concurrent"

	"context"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/prompt/optimizer"

	"github.com/polarisagi/polaris/internal/memory/graph"
	"github.com/polarisagi/polaris/internal/memory/retrieval"

	"github.com/polarisagi/polaris/internal/learning/surprise"
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
	// BlindZoneDetector V8-S4 认知盲区探测器；同一实例同时注入 FallacyPool
	// （MarkResolved 侧）与每个 Agent（RecordProduction/IsBlindZone 侧）。
	// 2026-07-21 deadcode 审查发现：NewBlindZoneDetector/InjectBlindZoneDetector
	// 两条注入点此前从未被任何生产代码调用，boot_memory.go 只建了 fallacyPool/
	// heuristics 却漏了这一步（S_PLAN 生产盲区强制 System2 路由从未真正生效）。
	BlindZoneDetector *optimizer.BlindZoneDetector
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

	// ─── §12.3 DriftDetector 漂移响应编排器（2026-07-21 deadcode 审查补齐）───
	// embedder 未启用时整套编排跳过：anchor 需要重新 Embed 查询才能算余弦距离，
	// 纯 BM25 部署（<8GB VPS 降级）本就不存在"向量空间漂移"这个问题域。
	if sb.Embedder != nil {
		th := sb.Cfg.Thresholds.M5Memory
		driftDetector := surprise.NewDriftDetector(int64(th.DriftCheckIntervalHours), th.DriftThreshold, sb.Embedder)
		driftRegistry := surprise.NewDriftDowngradeRegistry()
		mem.InjectDriftDetector(driftDetector, th.DriftAnchorSampleRate)
		mem.InjectDriftRegistry(driftRegistry)
		orchestrator := surprise.NewDriftOrchestrator(
			driftDetector, driftRegistry, reindexNow,
			time.Duration(th.DriftCheckIntervalHours)*time.Hour,
		)
		orchestrator.Start(ctx)
		slog.Info("polaris: drift detector orchestrator wired", "check_interval_hours", th.DriftCheckIntervalHours)
	}

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
	blindZoneDetector := optimizer.NewBlindZoneDetector(sb.Store.DB())
	fallacyPool.InjectBlindZoneDetector(blindZoneDetector)
	slog.Info("polaris: MEMF and heuristics memory initialized")

	// 抑制未使用的 observability 引用（FeatureGate 在 autoConf 路径中已消费）
	_ = probe.FeatureLocalEmbedding

	// ─── G-1: SurrealDB mem 后端 boot 重放器 ─────────────────────────────────
	startCognitiveReplayerIfNeeded(ctx, sb)
	// ───────────────────────────────────────────────────────────────────────

	// ─── polaris_surrealdb_index_size_mb 周期上报（HE-Rule-1）───────────────
	startSurrealIndexSizeReporter(ctx, sb)
	// ───────────────────────────────────────────────────────────────────────

	return &MemoryBundle{
		Mem:                mem,
		CascadeInvalidator: cascadeInvalidator,
		WriteFilter:        writeFilter,
		FallacyPool:        fallacyPool,
		Heuristics:         heuristics,
		BlindZoneDetector:  blindZoneDetector,
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

// startSurrealIndexSizeReporter 周期性调用 SurrealStore.Stats() 读取
// index_size_mb（HNSW+BM25+图索引的粗粒度估算，见 rust/substrate/src/
// surreal_store/fts.rs surreal_stats 的常量注释），上报到
// polaris_surrealdb_index_size_mb Gauge（HE-Rule-1 一等公民可观测性）。
// 此前该 Gauge 的 Setter（metrics.ReportSurrealDBIndexSize）从未被任何调用方
// 触发，恒为零值，本函数是唯一的生产者。
func startSurrealIndexSizeReporter(ctx context.Context, sb *SubstrateBundle) {
	if sb.SurrealStore == nil {
		return
	}
	const reportInterval = 60 * time.Second
	concurrent.SafeGo(ctx, "boot_memory.surreal_index_size_reporter", func(ctx context.Context) {
		ticker := time.NewTicker(reportInterval)
		defer ticker.Stop()
		reportOnce := func() {
			statsJSON, err := sb.SurrealStore.Stats()
			if err != nil {
				slog.Warn("polaris: surreal_index_size_reporter: Stats() failed", "err", err)
				return
			}
			var stats struct {
				IndexSizeMB float64 `json:"index_size_mb"`
			}
			if err := json.Unmarshal([]byte(statsJSON), &stats); err != nil {
				slog.Warn("polaris: surreal_index_size_reporter: unmarshal stats failed", "err", err)
				return
			}
			metrics.ReportSurrealDBIndexSize(int64(stats.IndexSizeMB))
		}
		reportOnce() // 启动时立即上报一次，不等第一个 tick（避免 /metrics 长期显示 0）
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				reportOnce()
			}
		}
	})
	slog.Info("polaris: surreal index size reporter started", "interval", reportInterval.String())
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
