package search

import (
	"context"
	"sort"
	"sync"

	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// DocumentSource 抽象各领域的文档召回能力。
type DocumentSource interface {
	SearchBM25(ctx context.Context, query string, topK int) ([]types.ScoredFragment, error)
	SearchVector(ctx context.Context, embedding []float32, topK int) ([]types.ScoredFragment, error)
	SearchGraph(ctx context.Context, query string, topK int) ([]types.ScoredFragment, error)
}

// ExtendedDocumentSource 允许提供额外的召回路径（如 Memory 的 Simhash/Durative/Reflection/Semantic）。
// 设计补偿：由于原始设计仅包含三路，而 Memory 实际包含七路，我们通过可选接口扩展支持。
type ExtendedDocumentSource interface {
	DocumentSource
	SearchExtraPaths(ctx context.Context, query string, embedding []float32, topK int) ([]ExtraPath, error)
}

type ExtraPath struct {
	Results []types.ScoredFragment
	Weight  float64
	Bit     uint8
}

type HybridSearchConfig struct {
	BM25Weight   float64
	VectorWeight float64
	GraphWeight  float64
	RRFk         int
	TopK         int
	EnableRerank bool
	Reranker     Reranker // 可选
}

type Reranker interface {
	Rerank(ctx context.Context, query string, docs []types.ScoredFragment) ([]types.ScoredFragment, error)
}

//nolint:gocyclo,nestif
func HybridSearch(
	ctx context.Context,
	source DocumentSource,
	query string,
	embedding []float32,
	config HybridSearchConfig,
) ([]types.ScoredFragment, error) {
	var wg sync.WaitGroup
	var bm25Results, vectorResults, graphResults []types.ScoredFragment
	var extraPaths []ExtraPath
	var searchErr error
	var errMu sync.Mutex

	wg.Add(1)
	concurrent.SafeGo(ctx, "hybrid_search.bm25", func(ctx context.Context) {
		defer wg.Done()
		res, err := source.SearchBM25(ctx, query, config.TopK*3)
		if err != nil {
			errMu.Lock()
			searchErr = err
			errMu.Unlock()
			return
		}
		bm25Results = res
	})

	if len(embedding) > 0 {
		wg.Add(1)
		concurrent.SafeGo(ctx, "hybrid_search.vector", func(ctx context.Context) {
			defer wg.Done()
			res, err := source.SearchVector(ctx, embedding, config.TopK*3)
			if err != nil {
				errMu.Lock()
				searchErr = err
				errMu.Unlock()
				return
			}
			vectorResults = res
		})
	}

	wg.Add(1)
	concurrent.SafeGo(ctx, "hybrid_search.graph", func(ctx context.Context) {
		defer wg.Done()
		res, err := source.SearchGraph(ctx, query, config.TopK*3)
		if err != nil {
			errMu.Lock()
			searchErr = err
			errMu.Unlock()
			return
		}
		graphResults = res
	})

	if ext, ok := source.(ExtendedDocumentSource); ok {
		wg.Add(1)
		concurrent.SafeGo(ctx, "hybrid_search.extra", func(ctx context.Context) {
			defer wg.Done()
			paths, err := ext.SearchExtraPaths(ctx, query, embedding, config.TopK*3)
			if err != nil {
				errMu.Lock()
				searchErr = err
				errMu.Unlock()
				return
			}
			extraPaths = paths
		})
	}

	wg.Wait()
	if searchErr != nil {
		return nil, searchErr
	}

	rrfK := float64(config.RRFk)
	if rrfK <= 0 {
		rrfK = 60.0
	}

	scoreMap := make(map[string]float64)
	contentMap := make(map[string]string)
	evidenceMap := make(map[string]types.EvidenceType)
	taintMap := make(map[string]types.TaintLevel)
	explainMap := make(map[string]uint8)

	addRRF := func(results []types.ScoredFragment, weight float64, bit uint8) {
		sorted := make([]types.ScoredFragment, len(results))
		copy(sorted, results)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].Score > sorted[j].Score })
		for rank, frag := range sorted {
			contribution := weight / (rrfK + float64(rank) + 1)
			prev := scoreMap[frag.Source]
			scoreMap[frag.Source] += contribution
			if frag.EvidenceType != "" && (prev == 0 || contribution > prev) {
				evidenceMap[frag.Source] = frag.EvidenceType
			}
			contentMap[frag.Source] = frag.Content
			if frag.TaintLevel > taintMap[frag.Source] {
				taintMap[frag.Source] = frag.TaintLevel
			}
			explainMap[frag.Source] |= bit
		}
	}

	bw := config.BM25Weight
	if bw <= 0 {
		bw = 1.0
	}
	vw := config.VectorWeight
	if vw <= 0 {
		vw = 0.6
	}
	gw := config.GraphWeight
	if gw <= 0 {
		gw = 0.6
	}

	addRRF(bm25Results, bw, types.BitBM25)
	addRRF(vectorResults, vw, types.BitVector)
	addRRF(graphResults, gw, types.BitGraph)

	for _, ep := range extraPaths {
		addRRF(ep.Results, ep.Weight, ep.Bit)
	}

	var merged []types.ScoredFragment
	for src, score := range scoreMap {
		merged = append(merged, types.ScoredFragment{
			Content:      contentMap[src],
			Score:        score,
			Source:       src,
			EvidenceType: evidenceMap[src],
			TaintLevel:   taintMap[src],
			ExplainBits:  explainMap[src],
		})
	}

	sort.Slice(merged, func(i, j int) bool {
		return merged[i].Score > merged[j].Score
	})

	if config.EnableRerank && config.Reranker != nil {
		if reranked, err := config.Reranker.Rerank(ctx, query, merged); err == nil {
			merged = reranked
		}
	}

	if config.TopK > 0 && len(merged) > config.TopK {
		merged = merged[:config.TopK]
	}

	return merged, nil
}
