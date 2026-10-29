package retrieval

import (
	"context"
	"sort"
	"sync"

	"github.com/polarisagi/polaris/internal/memory/util"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store/search"
	"github.com/polarisagi/polaris/pkg/types"
)

// graphSeedLimit Spreading Activation 的种子数上限（原实现：BM25 Top-3）。
const graphSeedLimit = 3

// memoryDocumentSource 把 M5 的七路召回适配到 store/search 的统一融合管线
// （GD-13-002）。
//
// 关键约束——**KV 前缀只扫一次**：重构前 `scanAndScore(prefix)` 一趟扫描同时
// 产出 Tier0 BM25 与 Simhash 两路结果。拆成 DocumentSource 后 BM25 / Simhash /
// Graph 种子分属三个并行方法，若各自 Scan 一遍，Tier0（2GB VPS，episodic 全量
// KV 前缀扫描）的读放大直接变 3 倍，且三趟扫描并发压在同一个 SQLite/Surreal
// 连接上。这里用 sync.Once 缓存单趟扫描结果，三路共享——并行调用方会自然阻塞
// 在同一个 Once 上，语义与重构前的"先串行扫描再分路"完全一致。
type memoryDocumentSource struct {
	hr           *HybridRetrieverImpl
	scope        types.SearchScope
	config       types.RetrievalConfig
	prefix       []byte
	semanticType QueryType

	scanOnce sync.Once
	scanned  []scannedDoc

	bm25Once sync.Once
	bm25     []types.ScoredFragment
}

// scannedDoc 一趟 KV 前缀扫描得到的原始文档。
type scannedDoc struct {
	src     string
	content string
}

var _ search.ExtendedDocumentSource = (*memoryDocumentSource)(nil)

// scanPrefix 惰性执行且**只执行一次**的 KV 前缀扫描。
func (m *memoryDocumentSource) scanPrefix(ctx context.Context) []scannedDoc {
	m.scanOnce.Do(func() {
		iter, err := m.hr.store.Scan(ctx, m.prefix)
		if err != nil || iter == nil {
			return
		}
		defer iter.Close()
		for iter.Next() {
			m.scanned = append(m.scanned, scannedDoc{
				src:     string(iter.Key()),
				content: string(iter.Value()),
			})
		}
	})
	return m.scanned
}

// bm25Once 保证 BM25 一路只算一次：SearchBM25 与 SearchGraph（取种子）共享
// 同一份结果，避免 Tier1 路径重复发起一次 FTS 查询、Tier0 路径重复打分。
func (m *memoryDocumentSource) bm25Results(ctx context.Context, query string, topK int) []types.ScoredFragment {
	m.bm25Once.Do(func() {
		// Tier1+：SurrealDB BM25 FTS（k1=1.2 b=0.75 原生）。
		// 召回条数沿用重构前的 config.FinalTopK（不是并行召回宽度 topK*3）——
		// FTSSearch 是索引查询，重构前后保持同一召回面，避免检索结果漂移。
		if m.hr.cognitive != nil && query != "" {
			k := m.config.FinalTopK
			if k <= 0 {
				k = topK
			}
			m.bm25 = m.hr.searchCognitiveFTS(ctx, query, k, m.config.AsOf)
			return
		}
		// Tier0 降级：纯 Go BM25 打分（复用单趟前缀扫描结果）。
		for _, d := range m.scanPrefix(ctx) {
			if score := util.Bm25Score(query, d.content); score > 0 {
				m.bm25 = append(m.bm25, types.ScoredFragment{
					Content:      d.content,
					Score:        score,
					Source:       d.src,
					EvidenceType: types.EvidenceFTSKeyword,
					TaintLevel:   taintForSource(d.src),
				})
			}
		}
	})
	return m.bm25
}

func (m *memoryDocumentSource) SearchBM25(ctx context.Context, query string, topK int) ([]types.ScoredFragment, error) {
	return m.bm25Results(ctx, query, topK), nil
}

func (m *memoryDocumentSource) SearchVector(ctx context.Context, embedding []float32, topK int) ([]types.ScoredFragment, error) {
	// Tier1+：SurrealDB HNSW KNN（原生余弦，O(log N)）。
	if m.hr.cognitive != nil {
		return m.vectorFromCognitive(ctx, embedding, topK), nil
	}
	// Tier0 降级：SQLite episodic_events float16 BLOB + Go 余弦。
	if m.scope.Type != "memory" {
		return nil, nil
	}
	sqlStore, ok := m.hr.store.(protocol.SQLQuerier)
	if !ok {
		return nil, nil
	}
	return m.hr.fetchVectorResultsFromSQL(ctx, sqlStore, embedding, m.config.Tier0VectorScanLimit), nil
}

// vectorFromCognitive Tier1+ HNSW 召回。
//
// 召回条数：重构前为 config.FinalTopK*3+30；topK 已是并行召回宽度
// （FinalTopK*3），故此处 +30 即还原原召回面，不能再乘 3（那会变成 9 倍，
// 在 Tier1 HNSW 上是纯粹的无谓开销）。
func (m *memoryDocumentSource) vectorFromCognitive(ctx context.Context, embedding []float32, topK int) []types.ScoredFragment {
	hits, vecErr := m.hr.cognitive.VecKNN(embedding, topK+30)
	if vecErr != nil {
		return nil
	}
	var out []types.ScoredFragment
	for _, h := range hits {
		content, src, taint, ok := m.hr.resolveCognitiveHit(ctx, h.ID, m.config.AsOf)
		if !ok {
			continue
		}
		et := types.EvidenceWeakSemantic
		if h.Score >= 0.85 {
			et = types.EvidenceHighVector
		}
		out = append(out, types.ScoredFragment{
			Content:      content,
			Score:        h.Score,
			Source:       src,
			EvidenceType: et,
			TaintLevel:   taint,
		})
	}
	return out
}

// SearchGraph Stage 1c：以 BM25 Top-3 为种子做 Spreading Activation 多种子能量扩散。
//
// 种子必须是**按 BM25 分降序的 Top-3**，而非扫描顺序的前 3 条：
// searchGraphSpreadingActivation 只取入参前 graphSeedLimit 个 Source，
// 若传入未排序的全量命中集，Tier0 路径下种子就退化成 KV 键序的任意 3 条，
// 图扩散的起点与查询相关性脱钩（本文件初版即有此缺陷）。
func (m *memoryDocumentSource) SearchGraph(ctx context.Context, query string, topK int) ([]types.ScoredFragment, error) {
	if m.hr.graph == nil {
		return nil, nil
	}
	seeds := m.bm25Results(ctx, query, topK)
	if len(seeds) == 0 {
		return nil, nil
	}
	sorted := make([]types.ScoredFragment, len(seeds))
	copy(sorted, seeds)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Score > sorted[j].Score })
	if len(sorted) > graphSeedLimit {
		sorted = sorted[:graphSeedLimit]
	}
	return m.hr.searchGraphSpreadingActivation(ctx, sorted), nil
}

// SearchExtraPaths M5 专属的第 2/4/5/6 路：Simhash / Reflection / Durative / Semantic。
// 权重与重构前 addRRF 调用点逐一对齐（bw*0.8 / 0.15 / 0.3 / 0.9）。
func (m *memoryDocumentSource) SearchExtraPaths(ctx context.Context, query string, _ []float32, _ int) ([]search.ExtraPath, error) {
	extra := make([]search.ExtraPath, 0, 4)
	extra = append(extra,
		search.ExtraPath{Results: m.simhashPath(ctx, query), Weight: m.bm25Weight() * 0.8, Bit: types.BitSimhash},
		search.ExtraPath{Results: m.reflectionPath(ctx, query), Weight: 0.15, Bit: types.BitReflection},
		search.ExtraPath{Results: m.durativePath(ctx, query), Weight: 0.3, Bit: types.BitDurative},
		search.ExtraPath{Results: m.semanticPath(ctx, query), Weight: 0.9, Bit: types.BitSemantic},
	)
	return extra, nil
}

// bm25Weight Simhash 路权重基于 BM25 权重缩放，需与融合层解析出的同一默认值一致。
func (m *memoryDocumentSource) bm25Weight() float64 {
	return resolveWeight(m.config.BM25Weight, defaultBM25Weight)
}

// simhashPath 第 2 路：Simhash 近似匹配（复用单趟前缀扫描结果）。
func (m *memoryDocumentSource) simhashPath(ctx context.Context, query string) []types.ScoredFragment {
	docs := m.scanPrefix(ctx)
	if len(docs) == 0 {
		return nil
	}
	queryFP := util.SimhashOf(query)
	var out []types.ScoredFragment
	for _, d := range docs {
		dist := queryFP.Hamming(util.SimhashOf(d.content))
		if dist > 16 {
			continue
		}
		simScore := 1.0 - float64(dist)/64.0
		evidType := types.EvidenceWeakSemantic
		if simScore >= 0.85 {
			evidType = types.EvidenceHighVector
		}
		out = append(out, types.ScoredFragment{
			Content:      d.content,
			Score:        simScore,
			Source:       d.src,
			EvidenceType: evidType,
			TaintLevel:   taintForSource(d.src),
		})
	}
	return out
}

// reflectionPath 第 4 路（M05 §7，权重 0.15）：跨会话 ReflectionMemory 召回。
// 优先走 SQL（命中 idx_reflect_task_type 索引）；接口未注入时降级 KV 前缀扫描。
func (m *memoryDocumentSource) reflectionPath(ctx context.Context, query string) []types.ScoredFragment {
	if m.scope.Type != "memory" {
		return nil
	}
	if m.hr.reflectionMem != nil {
		entries, rerr := m.hr.reflectionMem.ListReflections(ctx, types.ReflectionQuery{Topic: query, K: 20})
		if rerr != nil {
			return nil
		}
		var out []types.ScoredFragment
		for _, e := range entries {
			content := e.Decision + " " + e.Strategy
			if s := util.Bm25Score(query, content); s > 0 {
				out = append(out, newBM25Fragment("reflection:"+e.ID, content, s))
			}
		}
		return out
	}

	rIter, err := m.hr.store.Scan(ctx, []byte("reflection:"))
	if err != nil || rIter == nil {
		return nil
	}
	defer rIter.Close()
	var out []types.ScoredFragment
	for rIter.Next() {
		content := string(rIter.Value())
		if s := util.Bm25Score(query, content); s > 0 {
			out = append(out, newBM25Fragment(string(rIter.Key()), content, s))
		}
	}
	return out
}

// durativePath 第 5 路（temporal 查询激活，权重 0.3）：DurativeMemory 持续性记忆簇。
func (m *memoryDocumentSource) durativePath(ctx context.Context, query string) []types.ScoredFragment {
	if m.scope.Type != "memory" || m.hr.durative == nil || m.semanticType != QueryTypeTemporal {
		return nil
	}
	var out []types.ScoredFragment
	for _, g := range m.hr.durative.ListGroups(ctx, query, 5) {
		content := g.Label + ": " + g.Summary
		out = append(out, newBM25Fragment("durative_group:"+g.ID, content, util.Bm25Score(query, content)))
	}
	return out
}

// semanticPath 第 6 路（权重 0.9）：Semantic Entities 事实类记忆。
func (m *memoryDocumentSource) semanticPath(ctx context.Context, query string) []types.ScoredFragment {
	if m.scope.Type != "memory" && m.scope.Type != "semantic" {
		return nil
	}
	if m.hr.semantic == nil {
		return nil
	}
	return m.hr.searchSemanticEntities(ctx, query, m.config.AsOf, m.semanticType)
}

func newBM25Fragment(src, content string, score float64) types.ScoredFragment {
	return types.ScoredFragment{
		Content:      content,
		Score:        score,
		Source:       src,
		EvidenceType: types.EvidenceFTSKeyword,
		TaintLevel:   taintForSource(src),
	}
}
