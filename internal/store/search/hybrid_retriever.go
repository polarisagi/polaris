package search

import (
	"context"
	"log/slog"
	"sort"
	"sync"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
	"time"
)

// ============================================================================
// HybridSearch — M5(Memory) 与 M10(Knowledge) 共享的多路召回 + RRF 融合管线
// （GD-13-002 去重下沉）。
//
// 职责边界（HE-3 可组合原语）：本文件**只做算法**，不做策略。
//   - 权重、TopK、RRFk 的领域默认值由**调用方**解析后传入，本函数按字面值使用。
//     这条边界不是洁癖：memory 的 M05 §12.3 漂移降级正是通过把 VectorWeight
//     显式置 0 来关闭向量路的，若本函数"好心"把 0 当未设置补成默认 0.6，
//     降级开关就会被静默架空（本文件初版即有此缺陷）。
//   - 各路召回的取数逻辑由 DocumentSource 实现方提供，本函数不感知存储细节。
//
// 确定性（HE-4 Eval 可复现）：融合结果的排序必须与 map 迭代顺序无关。
// 因此内部维护 insertion order，并用 SliceStable + Source 兜底比较。
// 同理 addRRF 内部排名用 SliceStable：知识域的 Chunk 召回结果本身已由
// FTS rank / 向量相似度排好序而 Score 全为 0，非稳定排序会把这个已有次序
// 打成随机排列（本文件初版即有此缺陷）。
// ============================================================================

// DocumentSource 抽象各领域的文档召回能力。
//
// 实现方约定：
//   - 数据源不支持某一路时返回 (nil, nil)，不要返回错误。
//   - SearchBM25 是主路：它返回错误会中止整次检索（与重构前 knowledge
//     `ftsErr != nil → return` 语义一致）；vector/graph/extra 返回错误只降级
//     并告警（与重构前 memory / knowledge 对这三路的静默降级语义一致）。
type DocumentSource interface {
	SearchBM25(ctx context.Context, query string, topK int) ([]types.ScoredFragment, error)
	SearchVector(ctx context.Context, embedding []float32, topK int) ([]types.ScoredFragment, error)
	SearchGraph(ctx context.Context, query string, topK int) ([]types.ScoredFragment, error)
}

// ExtendedDocumentSource 允许提供额外的召回路（Memory 的 Simhash / Reflection /
// Durative / Semantic 四路）。三路是 M10 的形态，M5 实际是七路，用可选接口扩展
// 而非把四个 Memory 专属方法塞进 DocumentSource，避免污染 Knowledge 实现。
type ExtendedDocumentSource interface {
	DocumentSource
	SearchExtraPaths(ctx context.Context, query string, embedding []float32, topK int) ([]ExtraPath, error)
}

// ExtraPath 一条附加召回路及其融合权重与 ExplainBits 归因位。
type ExtraPath struct {
	Results []types.ScoredFragment
	Weight  float64
	Bit     uint8
}

// HybridSearchConfig 多路融合检索配置。
// 权重按字面值生效：<=0 表示**关闭该路**（不是"用默认值"）。
type HybridSearchConfig struct {
	BM25Weight   float64
	VectorWeight float64
	GraphWeight  float64
	RRFk         int // <=0 时取 60（RRF 标准常数，与 state.yaml 默认一致）
	TopK         int // <=0 表示不截断；领域默认值由调用方解析
	RecallWidth  int // 各路召回条数；<=0 时取 TopK*3
	EnableRerank bool
	Reranker     Reranker // 可选
}

// Reranker Cross-encoder 重排接口（consumer-side 定义）。
type Reranker interface {
	Rerank(ctx context.Context, query string, docs []types.ScoredFragment) ([]types.ScoredFragment, error)
}

// fusionState RRF 融合累加器。按 Source 聚合，同时维护首次出现顺序。
type fusionState struct {
	rrfK     float64
	order    []string
	score    map[string]float64
	content  map[string]string
	evidence map[string]types.EvidenceType
	taint    map[string]types.TaintLevel
	explain  map[string]uint8
}

func newFusionState(rrfK float64) *fusionState {
	return &fusionState{
		rrfK:     rrfK,
		score:    make(map[string]float64),
		content:  make(map[string]string),
		evidence: make(map[string]types.EvidenceType),
		taint:    make(map[string]types.TaintLevel),
		explain:  make(map[string]uint8),
	}
}

// add 把一路召回结果按 RRF 公式累加：score(d) += weight / (k + rank + 1)。
// weight <= 0 时整路跳过——包括不打 ExplainBits，因为该路已被显式关闭，
// 标记"命中过该路"会让线上排障看到不存在的召回来源。
func (f *fusionState) add(results []types.ScoredFragment, weight float64, bit uint8) {
	if weight <= 0 || len(results) == 0 {
		return
	}
	sorted := make([]types.ScoredFragment, len(results))
	copy(sorted, results)
	// SliceStable：Score 全为 0 的召回路（Knowledge 的 Chunk 路）必须保留
	// 调用方给定的原始次序（FTS rank / 向量相似度序）作为 RRF rank。
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Score > sorted[j].Score })

	for rank, frag := range sorted {
		contribution := weight / (f.rrfK + float64(rank) + 1)
		prev, seen := f.score[frag.Source]
		if !seen {
			f.order = append(f.order, frag.Source)
		}
		f.score[frag.Source] += contribution
		// 保留贡献最大那一路的证据类型（首次出现或新路贡献更大时更新）
		if frag.EvidenceType != "" && (!seen || contribution > prev) {
			f.evidence[frag.Source] = frag.EvidenceType
		}
		f.content[frag.Source] = frag.Content
		// 污点传播：只升不降（ADR-0007 PropagateTaint）
		if frag.TaintLevel > f.taint[frag.Source] {
			f.taint[frag.Source] = frag.TaintLevel
		}
		f.explain[frag.Source] |= bit
	}
}

// merged 输出按 RRF 分降序、同分按首次出现顺序（确定性）的融合结果。
func (f *fusionState) merged() []types.ScoredFragment {
	out := make([]types.ScoredFragment, 0, len(f.order))
	for _, src := range f.order {
		out = append(out, types.ScoredFragment{
			Content:      f.content[src],
			Score:        f.score[src],
			Source:       src,
			EvidenceType: f.evidence[src],
			TaintLevel:   f.taint[src],
			ExplainBits:  f.explain[src],
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// recallResults 一次并行召回的产物。
type recallResults struct {
	bm25   []types.ScoredFragment
	vector []types.ScoredFragment
	graph  []types.ScoredFragment
	extra  []ExtraPath
	// bm25Err 主路错误（中止检索）；其余路错误只降级告警，不出现在这里。
	bm25Err error
}

// recall 并行执行各路召回。
//
// 各路结果写入独立字段、读取发生在 wg.Wait() 之后，wg 建立 happens-before，
// 无需额外加锁；bm25Err 同理。
func recall(ctx context.Context, source DocumentSource, query string,
	embedding []float32, width int) *recallResults {
	r := &recallResults{}
	var wg sync.WaitGroup

	wg.Add(1)
	concurrent.SafeGo(ctx, "hybrid_search.bm25", func(ctx context.Context) {
		defer wg.Done()
		startBM25 := time.Now()
		res, err := source.SearchBM25(ctx, query, width)
		metrics.RecordRetrievalLatency(ctx, "bm25", time.Since(startBM25).Milliseconds())
		if err != nil {
			r.bm25Err = err
			return
		}
		r.bm25 = res
	})

	if len(embedding) > 0 {
		wg.Add(1)
		concurrent.SafeGo(ctx, "hybrid_search.vector", func(ctx context.Context) {
			defer wg.Done()
			startVector := time.Now()
			res, err := source.SearchVector(ctx, embedding, width)
			metrics.RecordRetrievalLatency(ctx, "vector", time.Since(startVector).Milliseconds())
			if err != nil {
				slog.WarnContext(ctx, "hybrid_search: vector recall failed, degrading", "err", err)
				return
			}
			r.vector = res
		})
	}

	wg.Add(1)
	concurrent.SafeGo(ctx, "hybrid_search.graph", func(ctx context.Context) {
		defer wg.Done()
		res, err := source.SearchGraph(ctx, query, width)
		if err != nil {
			slog.WarnContext(ctx, "hybrid_search: graph recall failed, degrading", "err", err)
			return
		}
		r.graph = res
	})

	if ext, ok := source.(ExtendedDocumentSource); ok {
		wg.Add(1)
		concurrent.SafeGo(ctx, "hybrid_search.extra", func(ctx context.Context) {
			defer wg.Done()
			paths, err := ext.SearchExtraPaths(ctx, query, embedding, width)
			if err != nil {
				slog.WarnContext(ctx, "hybrid_search: extra recall failed, degrading", "err", err)
				return
			}
			r.extra = paths
		})
	}

	wg.Wait()
	return r
}

// HybridSearch 执行多路并行召回 → RRF 融合 → 可选 Rerank → TopK 截断。
func HybridSearch(
	ctx context.Context,
	source DocumentSource,
	query string,
	embedding []float32,
	config HybridSearchConfig,
) ([]types.ScoredFragment, error) {
	startTotal := time.Now()
	defer func() {
		metrics.RecordRetrievalLatency(ctx, "fused", time.Since(startTotal).Milliseconds())
	}()

	width := config.RecallWidth
	if width <= 0 {
		width = config.TopK * 3
	}
	if width <= 0 {
		width = 30
	}

	r := recall(ctx, source, query, embedding, width)
	if r.bm25Err != nil {
		return nil, r.bm25Err //nolint:wrapcheck // 由调用方按各自领域语义包装
	}

	rrfK := float64(config.RRFk)
	if rrfK <= 0 {
		rrfK = 60.0
	}

	f := newFusionState(rrfK)
	f.add(r.bm25, config.BM25Weight, types.BitBM25)
	f.add(r.vector, config.VectorWeight, types.BitVector)
	f.add(r.graph, config.GraphWeight, types.BitGraph)
	for _, ep := range r.extra {
		f.add(ep.Results, ep.Weight, ep.Bit)
	}

	merged := f.merged()

	if config.EnableRerank && config.Reranker != nil {
		if reranked, err := config.Reranker.Rerank(ctx, query, merged); err == nil {
			merged = reranked
		} else {
			slog.WarnContext(ctx, "hybrid_search: rerank failed, keeping RRF order", "err", err)
		}
	}

	if config.TopK > 0 && len(merged) > config.TopK {
		merged = merged[:config.TopK]
	}
	return merged, nil
}
