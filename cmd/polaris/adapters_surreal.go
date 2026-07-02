// adapters_surreal.go — SurrealDB / Knowledge / Embedder 桥接适配器。
// 将 *store.SurrealDBCoreStore 的具体类型适配为各子系统定义的接口，
// 在 cmd/ 层完成桥接以避免包循环（memory ↔ storage，knowledge ↔ storage）。
package main

import (
	"context"
	"encoding/json"

	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/extension/native"
	knowledgepkg "github.com/polarisagi/polaris/internal/knowledge"
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/internal/store/search"
)

// ─── surrealCognAdapter ────────────────────────────────────────────────────────
//
// 将 *store.SurrealDBCoreStore 适配为 memory.CognitiveSearcher。
// FTSIndex / VecUpsert / GraphRelate 签名完全一致，直接透传。
// VecKNN / FTSSearch 返回类型 store.ScoredID → types.CognitiveSearchResult 需转换。
type surrealCognAdapter struct{ s *store.SurrealDBCoreStore }

func (a *surrealCognAdapter) FTSIndex(docID, text string) error {
	return a.s.FTSIndex(docID, text)
}

func (a *surrealCognAdapter) FTSDelete(docID string) error {
	return a.s.FTSDelete(docID)
}

func (a *surrealCognAdapter) VecUpsert(id string, embedding []float32) error {
	return a.s.VecUpsert(id, embedding)
}

func (a *surrealCognAdapter) GraphRelate(fromID, edgeType, toID string, weight float64) error {
	return a.s.GraphRelate(fromID, edgeType, toID, weight)
}

func (a *surrealCognAdapter) VecKNN(query []float32, k int) ([]types.CognitiveSearchResult, error) {
	hits, err := a.s.VecKNN(query, k)
	if err != nil {
		return nil, err
	}
	out := make([]types.CognitiveSearchResult, len(hits))
	for i, h := range hits {
		out[i] = types.CognitiveSearchResult{ID: h.ID, Score: h.Score}
	}
	return out, nil
}

func (a *surrealCognAdapter) FTSSearch(query string, k int) ([]types.CognitiveSearchResult, error) {
	hits, err := a.s.FTSSearch(query, k)
	if err != nil {
		return nil, err
	}
	out := make([]types.CognitiveSearchResult, len(hits))
	for i, h := range hits {
		out[i] = types.CognitiveSearchResult{ID: h.ID, Score: h.Score}
	}
	return out, nil
}

// ─── knowledgeCognAdapter ─────────────────────────────────────────────────────
//
// 将 *store.SurrealDBCoreStore 适配为 knowledge.CognitiveSearcher。
// 返回类型 store.ScoredID → knowledgepkg.CognitiveSearchResult（两个包独立定义）。
// TODO(BUG-5): 与 surrealCognAdapter 合并，将 CognitiveSearchResult 统一到 internal/protocol。
type knowledgeCognAdapter struct{ s *store.SurrealDBCoreStore }

func (a *knowledgeCognAdapter) VecKNN(query []float32, k int) ([]knowledgepkg.CognitiveSearchResult, error) {
	hits, err := a.s.VecKNN(query, k)
	if err != nil {
		return nil, err
	}
	out := make([]knowledgepkg.CognitiveSearchResult, len(hits))
	for i, h := range hits {
		out[i] = knowledgepkg.CognitiveSearchResult{ID: h.ID, Score: h.Score}
	}
	return out, nil
}

func (a *knowledgeCognAdapter) FTSSearch(query string, k int) ([]knowledgepkg.CognitiveSearchResult, error) {
	hits, err := a.s.FTSSearch(query, k)
	if err != nil {
		return nil, err
	}
	out := make([]knowledgepkg.CognitiveSearchResult, len(hits))
	for i, h := range hits {
		out[i] = knowledgepkg.CognitiveSearchResult{ID: h.ID, Score: h.Score}
	}
	return out, nil
}

// ─── knowledgeEmbedderAdapter ─────────────────────────────────────────────────
//
// 将 search.Embedder 适配为 knowledge.VectorEmbedder。
// search.Embedder.Embed(text string) []float32 → Embed(ctx, text) ([]float32, error)。
type knowledgeEmbedderAdapter struct{ e search.Embedder }

func (a *knowledgeEmbedderAdapter) Embed(_ context.Context, text string) ([]float32, error) {
	return a.e.Embed(text), nil
}

// ─── knowledgeBaseAdapter ─────────────────────────────────────────────────────
//
// 将 *knowledgepkg.KnowledgeBase 适配为 native.KnowledgeSearcher。
// SearchJSON 调用三阶段 RAG，将 []AugmentedContext 序列化为 JSON 返回给工具调用方。
type knowledgeBaseAdapter struct{ kb *knowledgepkg.KnowledgeBase }

func (a *knowledgeBaseAdapter) SearchJSON(ctx context.Context, query string, topK int, docScope string) ([]byte, error) {
	results, err := a.kb.Search(ctx, knowledgepkg.KnowledgeBaseSearchRequest{
		Query:    query,
		TopK:     topK,
		DocScope: docScope,
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(results)
}

// ─── fsmKnowledgeAdapter ──────────────────────────────────────────────────────
//
// 将 *knowledgepkg.KnowledgeBase 适配为 fsm.KnowledgeSearcher
type fsmKnowledgeAdapter struct{ kb *knowledgepkg.KnowledgeBase }

func (a *fsmKnowledgeAdapter) SearchRAG(ctx context.Context, query string, topK int) ([]fsm.KnowledgeResult, error) {
	if a.kb == nil {
		return nil, nil
	}
	results, err := a.kb.Search(ctx, knowledgepkg.KnowledgeBaseSearchRequest{
		Query: query,
		TopK:  topK,
	})
	if err != nil {
		return nil, err
	}
	out := make([]fsm.KnowledgeResult, len(results))
	for i, r := range results {
		out[i] = fsm.KnowledgeResult{
			Content: r.Primary.Content,
			Source:  r.Primary.SourceURI,
			Score:   1.0, // RAG 内部如果无分值则给1.0，或如果后续有分数可更新
		}
	}
	return out, nil
}

// ─── nativeCognAdapter ────────────────────────────────────────────────────────
//
// 将 *store.SurrealDBCoreStore 适配为 native.CognitiveSearcher。
type nativeCognAdapter struct{ s *store.SurrealDBCoreStore }

func (a nativeCognAdapter) FTSSearch(query string, k int) ([]native.ScoredResult, error) {
	if a.s == nil {
		return nil, nil
	}
	res, err := a.s.FTSSearch(query, k)
	if err != nil {
		return nil, err
	}
	out := make([]native.ScoredResult, len(res))
	for i, r := range res {
		out[i] = native.ScoredResult{ID: r.ID, Score: r.Score}
	}
	return out, nil
}

func (a nativeCognAdapter) VecKNN(query []float32, k int) ([]native.ScoredResult, error) {
	if a.s == nil {
		return nil, nil
	}
	res, err := a.s.VecKNN(query, k)
	if err != nil {
		return nil, err
	}
	out := make([]native.ScoredResult, len(res))
	for i, r := range res {
		out[i] = native.ScoredResult{ID: r.ID, Score: r.Score}
	}
	return out, nil
}

func (a nativeCognAdapter) GraphSpreadingActivation(startIDs []string, maxDepth int, energyDecay, dormancyThreshold float64, fanOutLimit int) ([]native.ScoredResult, error) {
	if a.s == nil {
		return nil, nil
	}
	res, err := a.s.GraphSpreadingActivation(startIDs, maxDepth, energyDecay, dormancyThreshold, fanOutLimit)
	if err != nil {
		return nil, err
	}
	out := make([]native.ScoredResult, len(res))
	for i, r := range res {
		out[i] = native.ScoredResult{ID: r.ID, Score: r.Score}
	}
	return out, nil
}

// pluginCognIndexAdapter 将 *store.SurrealDBCoreStore 适配为 plugin.CognitiveIndexer。
type pluginCognIndexAdapter struct{ s *store.SurrealDBCoreStore }

func (a *pluginCognIndexAdapter) FTSIndex(docID, text string) error {
	return a.s.FTSIndex(docID, text)
}

func (a *pluginCognIndexAdapter) VecUpsert(id string, embedding []float32) error {
	return a.s.VecUpsert(id, embedding)
}
