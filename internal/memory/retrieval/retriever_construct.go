package retrieval

import (
	"context"
	"time"

	"github.com/polarisagi/polaris/internal/memory/store"
	"github.com/polarisagi/polaris/internal/protocol"
)

// ============================================================================
// HybridRetriever — 结构体定义、构造函数与分类器注入（R7 拆分自 retriever.go）
// Search 主检索逻辑见 retriever.go；辅助检索函数见 retriever_helpers.go。
// ============================================================================

type HybridRetrieverImpl struct {
	store         protocol.Store
	graph         protocol.GraphTraverser      // Tier1+：图遍历路径，nil 时跳过
	durative      *store.DurativeMemoryManager // 第 5 路（temporal 查询激活），nil 时跳过
	reflectionMem protocol.ReflectionMemory    // 第 4 路：SQL 实现优先，nil 时降级 KV 扫描
	embedder      Embedder                     // P0：稠密向量检索
	cognitive     protocol.CognitiveSearcher   // Tier1+：SurrealDB FTS+HNSW，nil 时降级 Tier0
	semantic      protocol.SemanticMemory      // P0-2：第 6 路（semantic_entities）
	classifier    *SemanticQueryClassifier     // 查询意图分类（temporal 激活第 5 路）；实例持有，禁全局
}

// InjectEmbedder 注入 M1 Embedding 接口，激活向量检索路径。
// 同时异步预热语义查询分类器原型向量（失败自动降级 Tier-0 关键词路径）。
func (hr *HybridRetrieverImpl) InjectEmbedder(e Embedder) {
	hr.embedder = e
	cls := hr.queryClassifier()
	if e != nil {
		//nolint:bare-goroutine // 历史代码暂留，需结合上下文梳理 ctx 传递链路，后续重构替换
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			// 原型初始化失败不致命：ClassifyQuerySemantic 内部自动降级关键词分类
			_ = cls.InitPrototypes(ctx, e)
		}()
	}
}

// queryClassifier 返回分类器实例（惰性构造；原型未初始化时内部自动走 Tier-0 降级）。
func (hr *HybridRetrieverImpl) queryClassifier() *SemanticQueryClassifier {
	if hr.classifier == nil {
		hr.classifier = NewSemanticQueryClassifier()
	}
	return hr.classifier
}

func NewHybridRetriever(store protocol.Store) *HybridRetrieverImpl {
	return &HybridRetrieverImpl{store: store}
}

// NewHybridRetrieverWithGraph 创建含图路径的 HybridRetriever（Tier1+）。
func NewHybridRetrieverWithGraph(store protocol.Store, graph protocol.GraphTraverser) *HybridRetrieverImpl {
	return &HybridRetrieverImpl{store: store, graph: graph}
}

// NewHybridRetrieverWithDurative 创建含 DurativeMemory 第 5 路的 HybridRetriever。
func NewHybridRetrieverWithDurative(store protocol.Store, graph protocol.GraphTraverser, durative *store.DurativeMemoryManager) *HybridRetrieverImpl {
	return &HybridRetrieverImpl{store: store, graph: graph, durative: durative}
}

// NewHybridRetrieverFull 创建全路径 HybridRetriever（Graph + Durative + ReflectionMem）。
// reflectionMem 非 nil 时第 4 路走 SQL 查询；nil 时降级到 KV 前缀扫描（兼容旧部署）。
func NewHybridRetrieverFull(store protocol.Store, graph protocol.GraphTraverser, durative *store.DurativeMemoryManager, reflectionMem protocol.ReflectionMemory) *HybridRetrieverImpl {
	return &HybridRetrieverImpl{store: store, graph: graph, durative: durative, reflectionMem: reflectionMem}
}

// NewHybridRetrieverWithCognitive 创建含 SurrealDB FTS+HNSW 路径的全功能 HybridRetriever（Tier1+）。
// cognitive 注入后：BM25 路径走 SurrealDB BM25 FTS；向量路径走 SurrealDB HNSW KNN。
// cognitive == nil 时自动降级为 Tier0（纯 Go BM25 + SQLite BLOB 余弦）。
func NewHybridRetrieverWithCognitive(store protocol.Store, graph protocol.GraphTraverser, durative *store.DurativeMemoryManager, reflectionMem protocol.ReflectionMemory, cognitive protocol.CognitiveSearcher, semantic protocol.SemanticMemory) *HybridRetrieverImpl {
	return &HybridRetrieverImpl{store: store, graph: graph, durative: durative, reflectionMem: reflectionMem, cognitive: cognitive, semantic: semantic}
}
