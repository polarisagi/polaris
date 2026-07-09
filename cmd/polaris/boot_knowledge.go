// boot_knowledge.go — §7~§7.7 启动阶段：
// 知识 RAG → GraphBuildPipeline → KnowledgeBase → PII 检测器。
// KnowledgeBundle 持有所有知识层产物，向 run() 的 printStartupSummary 传递。
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/observability/probe"
	"github.com/polarisagi/polaris/internal/protocol"

	"github.com/polarisagi/polaris/internal/extension/native"
	knowledgepkg "github.com/polarisagi/polaris/internal/knowledge"
	"github.com/polarisagi/polaris/internal/knowledge/graphrag"
	"github.com/polarisagi/polaris/internal/observability/budget"
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/internal/store/search"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// KnowledgeBundle 持有 §7~§7.7 所有知识 RAG 产物。
type KnowledgeBundle struct {
	Ingester      *knowledgepkg.DefaultIngestionPipeline
	Retriever     knowledgepkg.HybridRetriever
	KnowledgeBase *knowledgepkg.KnowledgeBase
}

// bootKnowledge 执行 §7~§7.7 初始化，返回知识 RAG bundle。
func bootKnowledge(ctx context.Context, sb *SubstrateBundle, mb *MemoryBundle, tb *ToolBundle) (*KnowledgeBundle, error) { //nolint:gocyclo
	// ─── §7 Knowledge RAG (L2 M10) ───────────────────────────────────────────
	// ApproximateColBERTReranker（2026-07-04 审计补齐，任务2）：按 FeatureDeepRAG
	// 门控启用，复用 sb.Embedder。包一层 SafeRerank 提供超时+panic 保护。
	// FeatureDeepRAG 已存在（MinTier:Tier0, MinMemoryMB:1024），无需新增 Feature。
	var colbertReranker search.Reranker
	if sb.AutoConf != nil && sb.AutoConf.Gate.State(probe.FeatureDeepRAG) != probe.FeatureDisabled && sb.Embedder != nil {
		colbertReranker = search.NewSafeRerank(search.NewApproximateColBERTReranker(sb.Embedder, 3), 2*time.Second)
		slog.Info("polaris: ApproximateColBERTReranker enabled (FeatureDeepRAG)")
	}

	var retriever knowledgepkg.HybridRetriever
	if sb.SurrealStore != nil { //nolint:nestif // 原因：启动期组合根（composition root）按 Tier 装配双检索栈 + ColBERT 重排 + CorpusStats 持久化，属一次性初始化分支树而非热路径业务逻辑，拆分为多个私有函数收益有限且会打散启动顺序的可读性，参考 internal/automation/hitl/gateway.go Prompt() 的既有豁免先例。
		// Tier0+(≥8GB)：SQLite FTS5 + SurrealDB HNSW 双路检索
		var knowledgeEmb knowledgepkg.VectorEmbedder
		if sb.Embedder != nil {
			knowledgeEmb = &knowledgeEmbedderAdapter{e: sb.Embedder}
		}
		retriever = knowledgepkg.NewHybridRetrieverWithCognitive(sb.Store.DB(), knowledgeEmb, &knowledgeCognAdapter{s: sb.SurrealStore})
		// SurrealDB 路径使用 protocol.Reranker 接口（含 error 返回），通过
		// colbertRerankerAdapter 适配同一个 colbertReranker 实例，两条检索栈
		// 共用一套重排逻辑，不重复实例化 Embedder 调用链。
		if colbertReranker != nil {
			if hri, ok := retriever.(*knowledgepkg.HybridRetrieverImpl); ok {
				hri.SetReranker(&colbertRerankerAdapter{inner: colbertReranker})
			}
		}
		slog.Info("polaris: knowledge RAG initialized (FTS5 + SurrealDB HNSW, Tier0+/≥8GB)")
	} else {
		defaultRetriever := knowledgepkg.NewDefaultHybridRetriever(sb.StorageRouter, sb.Embedder, colbertReranker)
		retriever = defaultRetriever
		slog.Info("polaris: knowledge RAG initialized (StorageRouter only, <8GB VPS)")

		// CorpusStats 持久化接线（2026-07-04 审计补齐，任务18）：启动时恢复历史
		// BM25 语料库统计，之后每 5 分钟增量落盘一次（dirty 标记为 false 时
		// FlushTo 直接 no-op，不产生无谓 IO）。仅 StorageRouter-only 路径使用
		// search.CorpusStats/bm25Score；SurrealDB 路径走独立的 FTS5/HNSW 评分，
		// 不涉及本套统计。
		statsDB := sb.Store.DB()
		if statsDB != nil {
			stats := defaultRetriever.Engine().Stats()
			if err := stats.RestoreStatsFromDB(ctx, statsDB); err != nil {
				slog.Warn("polaris: CorpusStats.LoadFrom failed, starting from empty stats", "err", err)
			} else {
				slog.Info("polaris: CorpusStats restored from corpus_stats table")
			}
			concurrent.SafeGo(ctx, "corpus-stats-flush", func(ctx context.Context) {
				ticker := time.NewTicker(5 * time.Minute)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						if err := stats.FlushTo(ctx, statsDB); err != nil {
							slog.Warn("polaris: CorpusStats.FlushTo failed", "err", err)
						}
					}
				}
			})
		}
	}

	var searchEngine *search.HybridSearchEngine
	if dr, ok := retriever.(*knowledgepkg.DefaultHybridRetriever); ok {
		searchEngine = dr.Engine()
	}
	ingester := knowledgepkg.NewDefaultIngestionPipeline(sb.StorageRouter, sb.Router, sb.Outbox, searchEngine)

	// ─── §7.5 知识图谱构建管线（GraphBuildPipeline，M10 §2.7）────────────────
	var graphLLMClient graphrag.LLMClient
	if sb.Router != nil {
		graphLLMClient = graphrag.NewProviderLLMClient(sb.Router, "")
	}
	graphTier := 0
	if sb.AutoConf != nil {
		graphTier = int(sb.AutoConf.Config.Tier)
	}
	var graphPipeline *graphrag.GraphBuildPipeline
	if sb.AutoConf != nil && sb.AutoConf.Gate.State(probe.FeatureGraphRAGFull) != probe.FeatureDisabled {
		graphPipeline = graphrag.NewGraphBuildPipeline(graphLLMClient, graphTier, mb.Mem.Semantic())
		var graphGuard *probe.OSMemoryGuard
		var graphGate *probe.FeatureGate
		if sb.AutoConf != nil {
			graphGuard = sb.AutoConf.Guard
			graphGate = sb.AutoConf.Gate
		}
		graphPipeline.WithBackgroundGate(budget.NewResourceBudget(sb.TBR, graphGuard, graphGate))
		// 2026-07-08 恢复接线：CommunityGenerativeSummarizer 是完整实现且有专属测试覆盖
		// 的功能，此前仅其注入口 WithSummarizer 被误判死代码删除，导致永久不可达。
		// 详见 local_playground/reports/phase4-hard-dep-and-deadcode-followup-20260708.md。
		if sb.Router != nil {
			graphPipeline.WithSummarizer(graphrag.NewCommunityGenerativeSummarizer(sb.Router))
		}
		slog.Info("polaris: knowledge graph pipeline initialized", "tier", graphTier)
	} else {
		slog.Info("polaris: GraphRAG pipeline disabled by FeatureGate (<8GB VPS or memory pressure, 1024MB min)")
	}
	if graphPipeline != nil {
		sb.Outbox.RegisterHandler(protocol.TopicGraphBuild, func(ctx context.Context, rec *store.OutboxRecord) error {
			var payload struct {
				DocID string `json:"doc_id"`
			}
			if err := json.Unmarshal(rec.Payload, &payload); err != nil {
				return err
			}
			return graphPipeline.Run(ctx, payload.DocID)
		})
		sb.Outbox.RegisterHandler(graphrag.EventTypeRAGDocIngested, graphrag.NewGraphBuildOutboxHandler(graphPipeline).Handle)
		summaryGenHandler := graphrag.NewSummaryGenOutboxHandler(sb.Store.DB(), sb.Router)
		sb.Outbox.RegisterHandler(graphrag.EventTypeRAGDocSummaryNeeded, summaryGenHandler.Handle)
		slog.Info("polaris: SummaryGenOutboxHandler registered for rag_doc_summary_needed")
		slog.Info("polaris: GraphBuildPipeline registered to outbox for graph_build and rag_doc_ingested")
	}

	// ─── §7.6 KnowledgeBase（三阶段 DeepRAG 统一入口，M10 §2.5）──────────────
	// FeatureDeepRAG（Tier 0+，≥8GB）：QueryPlanner + StructuredNavigator 激活。
	var knowledgeBase *knowledgepkg.KnowledgeBase
	{
		expander := knowledgepkg.NewContextExpander(sb.StorageRouter)
		var navigator *knowledgepkg.StructuredNavigator
		var planner *knowledgepkg.QueryPlanner
		if sb.AutoConf != nil && sb.AutoConf.Gate.State(probe.FeatureDeepRAG) != probe.FeatureDisabled {
			navigator = knowledgepkg.NewStructuredNavigator(sb.StorageRouter)
			if sb.Router != nil {
				planner = knowledgepkg.NewQueryPlanner(sb.Router)
			}
			slog.Info("polaris: KnowledgeBase initialized with DeepRAG (QueryPlanner + StructuredNavigator)")
		} else {
			slog.Info("polaris: KnowledgeBase initialized (basic mode, DeepRAG disabled, <8GB VPS)")
		}
		var kbGate interface {
			IsEnabled(probe.Feature) bool
		}
		if sb.AutoConf != nil {
			kbGate = sb.AutoConf.Gate
		}
		knowledgeBase = knowledgepkg.NewKnowledgeBase(retriever, expander, navigator, planner, nil, kbGate)
	}

	// 将 knowledgeBase 注册为 "knowledge_search" builtin 工具（M10 §2.5）
	kbAdapter := &knowledgeBaseAdapter{kb: knowledgeBase}
	if err := native.RegisterExtensionTool(tb.InProcSandbox, tb.ToolReg, "knowledge_search", native.MakeKnowledgeSearchFn(kbAdapter)); err != nil {
		slog.Warn("polaris: knowledge_search tool registration failed", "err", err)
	} else {
		slog.Info("polaris: knowledge_search tool registered (three-phase DeepRAG)")
	}

	// ctx 仅供将来异步初始化路径使用，当前同步完成
	_ = ctx

	return &KnowledgeBundle{
		Ingester:      ingester,
		Retriever:     retriever,
		KnowledgeBase: knowledgeBase,
	}, nil
}
