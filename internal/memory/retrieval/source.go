package retrieval

import (
	"context"

	"github.com/polarisagi/polaris/internal/memory/util"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store/search"
	"github.com/polarisagi/polaris/pkg/types"
)

type memoryDocumentSource struct {
	hr           *HybridRetrieverImpl
	scope        types.SearchScope
	config       types.RetrievalConfig
	prefix       []byte
	semanticType QueryType
}

var _ search.ExtendedDocumentSource = (*memoryDocumentSource)(nil)

func (m *memoryDocumentSource) SearchBM25(ctx context.Context, query string, topK int) ([]types.ScoredFragment, error) {
	var bm25Results []types.ScoredFragment
	// If cognitive is available, FTSSearch handles BM25
	if m.hr.cognitive != nil && query != "" {
		bm25Results = m.hr.searchCognitiveFTS(ctx, query, topK, m.config.AsOf)
	} else {
		// Tier0 fallback
		iter, err := m.hr.store.Scan(ctx, m.prefix)
		if err == nil && iter != nil {
			defer iter.Close()
			for iter.Next() {
				content := string(iter.Value())
				src := string(iter.Key())
				if score := util.Bm25Score(query, content); score > 0 {
					bm25Results = append(bm25Results, types.ScoredFragment{
						Content:      content,
						Score:        score,
						Source:       src,
						EvidenceType: types.EvidenceFTSKeyword,
						TaintLevel:   taintForSource(src),
					})
				}
			}
		}
	}
	return bm25Results, nil
}

//nolint:gocyclo,nestif
func (m *memoryDocumentSource) SearchVector(ctx context.Context, embedding []float32, topK int) ([]types.ScoredFragment, error) {
	var vectorResults []types.ScoredFragment
	if m.hr.cognitive != nil {
		if hits, vecErr := m.hr.cognitive.VecKNN(embedding, topK*3+30); vecErr == nil {
			for _, h := range hits {
				et := types.EvidenceWeakSemantic
				if h.Score >= 0.85 {
					et = types.EvidenceHighVector
				}
				content, src, taint, ok := m.hr.resolveCognitiveHit(ctx, h.ID, m.config.AsOf)
				if !ok {
					continue
				}
				vectorResults = append(vectorResults, types.ScoredFragment{
					Content:      content,
					Score:        h.Score,
					Source:       src,
					EvidenceType: et,
					TaintLevel:   taint,
				})
			}
		}
	} else if m.scope.Type == "memory" {
		if sqlStore, ok := m.hr.store.(protocol.SQLQuerier); ok {
			vectorResults = m.hr.fetchVectorResultsFromSQL(ctx, sqlStore, embedding, m.config.Tier0VectorScanLimit)
		}
	}
	return vectorResults, nil
}

func (m *memoryDocumentSource) SearchGraph(ctx context.Context, query string, topK int) ([]types.ScoredFragment, error) {
	// 这是一个设计上的妥协：原设计的 Graph 检索需要 BM25 的结果作为种子，
	// 但在 HybridSearch 抽象中，各路是并行独立执行的。
	// 为了兼容，我们在 ExtraPaths 里利用传入的 query 做特殊处理，而将此内置 Graph 路留空，
	// 或者在这里直接重复计算一点 BM25（通常很快）作为种子。
	// 原版: if hr.graph != nil && len(bm25Results) > 0 { hr.searchGraphSpreadingActivation(...) }

	if m.hr.graph == nil {
		return nil, nil
	}

	// 快速获取种子
	var seeds []types.ScoredFragment
	if m.hr.cognitive != nil && query != "" {
		seeds = m.hr.searchCognitiveFTS(ctx, query, 3, m.config.AsOf)
	} else {
		iter, err := m.hr.store.Scan(ctx, m.prefix)
		if err == nil && iter != nil {
			defer iter.Close()
			for iter.Next() {
				content := string(iter.Value())
				src := string(iter.Key())
				if score := util.Bm25Score(query, content); score > 0 {
					seeds = append(seeds, types.ScoredFragment{
						Source: src, Score: score,
					})
				}
			}
		}
	}
	if len(seeds) > 0 {
		return m.hr.searchGraphSpreadingActivation(ctx, seeds), nil
	}
	return nil, nil
}

//nolint:gocyclo,nestif
func (m *memoryDocumentSource) SearchExtraPaths(ctx context.Context, query string, embedding []float32, topK int) ([]search.ExtraPath, error) {
	extra := make([]search.ExtraPath, 0, 4)

	// 1. Simhash
	var simhashResults []types.ScoredFragment
	iter, err := m.hr.store.Scan(ctx, m.prefix)
	if err == nil && iter != nil {
		queryFP := util.SimhashOf(query)
		for iter.Next() {
			content := string(iter.Value())
			src := string(iter.Key())
			contentFP := util.SimhashOf(content)
			if dist := queryFP.Hamming(contentFP); dist <= 16 {
				simScore := 1.0 - float64(dist)/64.0
				evidType := types.EvidenceWeakSemantic
				if simScore >= 0.85 {
					evidType = types.EvidenceHighVector
				}
				simhashResults = append(simhashResults, types.ScoredFragment{
					Content:      content,
					Score:        simScore,
					Source:       src,
					EvidenceType: evidType,
					TaintLevel:   taintForSource(src),
				})
			}
		}
		iter.Close()
	}
	bw := m.config.BM25Weight
	if bw <= 0 {
		bw = 1.0
	}
	extra = append(extra, search.ExtraPath{
		Results: simhashResults,
		Weight:  bw * 0.8,
		Bit:     types.BitSimhash,
	})

	// 2. Reflection
	var reflectionResults []types.ScoredFragment
	if m.scope.Type == "memory" {
		if m.hr.reflectionMem != nil {
			entries, rerr := m.hr.reflectionMem.ListReflections(ctx, types.ReflectionQuery{
				Topic: query,
				K:     20,
			})
			if rerr == nil {
				for _, e := range entries {
					content := e.Decision + " " + e.Strategy
					if s := util.Bm25Score(query, content); s > 0 {
						src := "reflection:" + e.ID
						reflectionResults = append(reflectionResults, types.ScoredFragment{
							Content:      content,
							Score:        s,
							Source:       src,
							EvidenceType: types.EvidenceFTSKeyword,
							TaintLevel:   taintForSource(src),
						})
					}
				}
			}
		} else {
			rIter, err := m.hr.store.Scan(ctx, []byte("reflection:"))
			if err == nil && rIter != nil {
				for rIter.Next() {
					content := string(rIter.Value())
					src := string(rIter.Key())
					if s := util.Bm25Score(query, content); s > 0 {
						reflectionResults = append(reflectionResults, types.ScoredFragment{
							Content:      content,
							Score:        s,
							Source:       src,
							EvidenceType: types.EvidenceFTSKeyword,
							TaintLevel:   taintForSource(src),
						})
					}
				}
				rIter.Close()
			}
		}
	}
	extra = append(extra, search.ExtraPath{
		Results: reflectionResults,
		Weight:  0.15,
		Bit:     types.BitReflection,
	})

	// 3. Durative
	var durativeResults []types.ScoredFragment
	if m.scope.Type == "memory" && m.hr.durative != nil && m.semanticType == QueryTypeTemporal {
		groups := m.hr.durative.ListGroups(ctx, query, 5)
		for _, g := range groups {
			content := g.Label + ": " + g.Summary
			src := "durative_group:" + g.ID
			durativeResults = append(durativeResults, types.ScoredFragment{
				Content:      content,
				Score:        util.Bm25Score(query, content),
				Source:       src,
				EvidenceType: types.EvidenceFTSKeyword,
				TaintLevel:   taintForSource(src),
			})
		}
	}
	extra = append(extra, search.ExtraPath{
		Results: durativeResults,
		Weight:  0.3,
		Bit:     types.BitDurative,
	})

	// 4. Semantic
	var semanticResults []types.ScoredFragment
	if (m.scope.Type == "memory" || m.scope.Type == "semantic") && m.hr.semantic != nil {
		semanticResults = m.hr.searchSemanticEntities(ctx, query, m.config.AsOf, m.semanticType)
	}
	extra = append(extra, search.ExtraPath{
		Results: semanticResults,
		Weight:  0.9,
		Bit:     types.BitSemantic,
	})

	return extra, nil
}
