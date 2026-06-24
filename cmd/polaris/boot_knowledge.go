// boot_knowledge.go — §7~§7.7 启动阶段：
// 知识 RAG → GraphBuildPipeline → KnowledgeBase → PII 检测器。
// KnowledgeBundle 持有所有知识层产物，向 run() 的 printStartupSummary 传递。
package main

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/polarisagi/polaris/internal/observability/probe"

	"github.com/polarisagi/polaris/internal/extension/native"
	knowledgepkg "github.com/polarisagi/polaris/internal/knowledge"
	"github.com/polarisagi/polaris/internal/knowledge/graphrag"
	"github.com/polarisagi/polaris/internal/observability/budget"
	"github.com/polarisagi/polaris/internal/security/guard"
	"github.com/polarisagi/polaris/internal/store"
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
	ingester := knowledgepkg.NewDefaultIngestionPipeline(sb.StorageRouter, sb.Router, sb.Outbox)
	var retriever knowledgepkg.HybridRetriever
	if sb.SurrealStore != nil {
		// Tier0+(≥8GB)：SQLite FTS5 + SurrealDB HNSW 双路检索
		var knowledgeEmb knowledgepkg.VectorEmbedder
		if sb.Embedder != nil {
			knowledgeEmb = &knowledgeEmbedderAdapter{e: sb.Embedder}
		}
		retriever = knowledgepkg.NewHybridRetrieverWithCognitive(sb.Store.DB(), knowledgeEmb, &knowledgeCognAdapter{s: sb.SurrealStore})
		slog.Info("polaris: knowledge RAG initialized (FTS5 + SurrealDB HNSW, Tier0+/≥8GB)")
	} else {
		retriever = knowledgepkg.NewDefaultHybridRetriever(sb.StorageRouter, sb.Embedder)
		slog.Info("polaris: knowledge RAG initialized (StorageRouter only, <8GB VPS)")
	}

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
		graphPipeline.WithBackgroundGate(&budget.ResourceBudget{})
		slog.Info("polaris: knowledge graph pipeline initialized", "tier", graphTier)
	} else {
		slog.Info("polaris: GraphRAG pipeline disabled by FeatureGate (<8GB VPS or memory pressure, 1024MB min)")
	}
	if graphPipeline != nil {
		sb.Outbox.RegisterHandler("graph_build", func(ctx context.Context, rec *store.OutboxRecord) error {
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

	// ─── §7.7 PII 检测器（M11 §5.1）─────────────────────────────────────────
	var piiDetector *guard.PIIDetector
	if sb.AutoConf != nil && sb.AutoConf.Gate.State(probe.FeaturePresidioPII) != probe.FeatureDisabled {
		piiDetector = guard.NewPIIDetectorWithPresidio("http://localhost:3000/analyze", sb.SafeHTTP)
		slog.Info("polaris: PII detector initialized (Presidio sidecar)")
	} else {
		piiDetector = guard.NewPIIDetector()
		slog.Info("polaris: PII detector initialized (Go regex Tier 0)")
	}
	_ = piiDetector

	// ctx 仅供将来异步初始化路径使用，当前同步完成
	_ = ctx

	return &KnowledgeBundle{
		Ingester:      ingester,
		Retriever:     retriever,
		KnowledgeBase: knowledgeBase,
	}, nil
}
