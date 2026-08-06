package knowledge

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/polarisagi/polaris/internal/store/search"
	"github.com/polarisagi/polaris/pkg/types"
)

type knowledgeDocumentSource struct {
	hr      *HybridRetrieverImpl
	isMacro bool
}

var _ search.DocumentSource = (*knowledgeDocumentSource)(nil)

func (k *knowledgeDocumentSource) SearchBM25(ctx context.Context, query string, topK int) ([]types.ScoredFragment, error) {
	var results []types.ScoredFragment

	if k.isMacro {
		// Macro community search
		rows, err := k.hr.db.QueryContext(ctx, `
			SELECT id, properties 
			FROM semantic_entities 
			WHERE entity_type = 'Community' AND json_extract(properties, '$.level') >= 1 
			ORDER BY json_extract(properties, '$.level') DESC 
			LIMIT ?`, topK)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var id int
				var props string
				if err := rows.Scan(&id, &props); err == nil {
					results = append(results, types.ScoredFragment{
						Source:  fmt.Sprintf("macro-community-%d", id),
						Content: "【社区摘要】" + props,
					})
				}
			}
		}
	}

	ftsRes, err := k.hr.searchFTS(ctx, query, topK)
	if err != nil {
		return nil, err
	}
	for _, c := range ftsRes {
		results = append(results, chunkToFragment(c))
	}
	return results, nil
}

func (k *knowledgeDocumentSource) SearchVector(ctx context.Context, embedding []float32, topK int) ([]types.ScoredFragment, error) {
	if k.hr.embedder == nil {
		return nil, nil
	}

	// embedding array from config isn't needed here if we rely on searchVector fallback,
	// but since HybridSearch passes the embedding, we can reuse searchVector fallback logic.
	// We'll just call searchVector but avoiding the embed call inside it (or we can just reuse hr.searchVector for now).
	// But searchVector takes string! So we modify searchVector or we just call it with the query string from context if we keep it,
	// wait, we don't have query string here. We can just use the embedding.

	var vecResults []Chunk
	if k.hr.cognitive != nil {
		if hits, vecErr := k.hr.cognitive.VecKNN(embedding, topK); vecErr == nil {
			vecResults, _ = k.hr.fetchCognitiveHits(ctx, hits)
		} else {
			slog.Warn("knowledge: SurrealDB VecKNN failed, degrading to linear scan", "err", vecErr)
			vecResults, _ = k.hr.searchVectorFallback(ctx, embedding, topK)
		}
	} else {
		vecResults, _ = k.hr.searchVectorFallback(ctx, embedding, topK)
	}

	var results []types.ScoredFragment
	for _, c := range vecResults {
		results = append(results, chunkToFragment(c))
	}
	return results, nil
}

func (k *knowledgeDocumentSource) SearchGraph(ctx context.Context, query string, topK int) ([]types.ScoredFragment, error) {
	if k.hr.graph == nil {
		return nil, nil
	}
	gr, err := k.hr.graph.TraverseChunks(ctx, query, topK)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}
	var results []types.ScoredFragment
	for _, c := range gr {
		results = append(results, chunkToFragment(c))
	}
	return results, nil
}

func chunkToFragment(c Chunk) types.ScoredFragment {
	return types.ScoredFragment{
		Source:     c.ID, // c.ID as Source since ID is unique for chunks
		Content:    c.Content,
		TaintLevel: types.TaintLevel(c.TaintLevel),
		// Original toScoredFragments maps ID back to URI/TaintLevel etc.
		// If we set Source to ID, RRFFuse will group by ID, which is correct.
	}
}
