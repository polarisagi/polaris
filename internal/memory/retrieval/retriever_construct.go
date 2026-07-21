package retrieval

import (
	"context"
	"time"

	"github.com/polarisagi/polaris/internal/memory/store"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// DriftAnchorRecorder/DriftGate — 消费方（本包，L1）本地声明的漂移编排接口
// （HE-3：接口在调用方定义）。架构分层 R1.7 禁止 internal/memory（L1）反向
// import internal/learning（L2），故不直接引用 *surprise.DriftDetector/
// *surprise.DriftDowngradeRegistry 具体类型；cmd/polaris 组合根构造具体实例后，
// 以满足这两个接口的方式注入（*surprise.DriftDetector.RecordAnchor 与
// *surprise.DriftDowngradeRegistry.IsDowngraded 方法签名与此精确匹配）。

// DriftAnchorRecorder 漂移检测 anchor 采样接口。
type DriftAnchorRecorder interface {
	RecordAnchor(taskType, query string, embedding []float32, expected []string)
}

// DriftGate 漂移降级状态查询接口。
type DriftGate interface {
	IsDowngraded(taskType string) bool
}

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

	// driftDetector/driftRegistry — M05 §12.3 Embedding 漂移响应编排（2026-07-21
	// deadcode 审查补齐）。两者均为可选注入，nil 时 Search 完全跳过漂移采样/
	// 降级判断，行为与接入前一致（fail-open，不影响现有检索路径）。
	driftDetector   DriftAnchorRecorder
	driftRegistry   DriftGate
	driftSampleRate float64 // M5MemoryThresholds.DriftAnchorSampleRate，0 时等效未注入
}

// InjectDriftDetector 注入漂移检测器与采样率，激活 Search() 内的 anchor 采样。
// 未注入（dd==nil）或 sampleRate<=0 时 Search 不做任何漂移相关行为（nil-safe）。
func (hr *HybridRetrieverImpl) InjectDriftDetector(dd DriftAnchorRecorder, sampleRate float64) {
	hr.driftDetector = dd
	hr.driftSampleRate = sampleRate
}

// InjectDriftRegistry 注入降级状态注册表，激活 Search() 内按 task_type 降级
// VectorWeight 的判断。未注入时 Search 不做任何降级（nil-safe）。
func (hr *HybridRetrieverImpl) InjectDriftRegistry(r DriftGate) {
	hr.driftRegistry = r
}

// InjectEmbedder 注入 M1 Embedding 接口，激活向量检索路径。
// 同时异步预热语义查询分类器原型向量（失败自动降级 Tier-0 关键词路径）。
func (hr *HybridRetrieverImpl) InjectEmbedder(e Embedder) {
	hr.embedder = e
	cls := hr.queryClassifier()
	if e != nil {
		concurrent.SafeGo(context.Background(), "retriever.init_prototypes", func(ctx context.Context) {
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			// 原型初始化失败不致命：ClassifyQuerySemantic 内部自动降级关键词分类
			_ = cls.InitPrototypes(ctx, e)
		})
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

// 2026-07-14（ADR-0051）：NewHybridRetrieverWithGraph/NewHybridRetrieverWithDurative
// 删除——graph-without-durative/durative-without-reflectionMem 是幽灵 Tier 档位：
// graph 与 cognitive/durative 同源自 sb.SurrealStore，实际启动分级逻辑中不存在
// "只有 graph 没有其余能力"的组合，全仓零调用点。生产唯一使用 NewHybridRetriever
// （基础）/NewHybridRetrieverFull/NewHybridRetrieverWithCognitive。

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
