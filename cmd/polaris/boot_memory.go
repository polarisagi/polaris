// boot_memory.go — §4.10~§5 启动阶段：
// M5 记忆系统 → OnlineReindexer → CascadeInvalidator → TemporalExpirer → MEMF。
// MemoryBundle 持有所有认知记忆产物，向 boot_tools/boot_knowledge/boot_agent 传递。
package main

import (
	"github.com/polarisagi/polaris/internal/observability/probe"

	"context"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/prompt/optimizer"

	"github.com/polarisagi/polaris/internal/memory/graph"
	"github.com/polarisagi/polaris/internal/memory/retrieval"

	"github.com/polarisagi/polaris/internal/memory"
)

// MemoryBundle 持有 §4.10~§5 所有认知记忆产物。
type MemoryBundle struct {
	Mem                *memory.MemImpl
	CascadeInvalidator *retrieval.CascadeInvalidator
	WriteFilter        *retrieval.WriteFilter
	FallacyPool        *optimizer.FallacyMemoryPool
	Heuristics         *optimizer.HeuristicsMemory
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
	startOnlineReindexer(ctx, sb)

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

	return &MemoryBundle{
		Mem:                mem,
		CascadeInvalidator: cascadeInvalidator,
		WriteFilter:        writeFilter,
		FallacyPool:        fallacyPool,
		Heuristics:         heuristics,
	}, nil
}

func startOnlineReindexer(ctx context.Context, sb *SubstrateBundle) {
	if sb.Embedder == nil {
		return
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
	go func() {
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
	}()
	slog.Info("polaris: online reindexer started", "model", embedModelName, "interval", "5m")
}

func startTemporalExpirer(ctx context.Context, sb *SubstrateBundle) {
	temporalExpirer := graph.NewTemporalExpirer(sb.Store.DB())
	go func() {
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
	}()
	slog.Info("polaris: temporal expirer started", "interval", "1h")
}
