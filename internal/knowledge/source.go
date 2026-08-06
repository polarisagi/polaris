package knowledge

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/polarisagi/polaris/internal/store/search"
	"github.com/polarisagi/polaris/pkg/types"
)

// M10 §2.2 HybridRetrieverConfig 融合权重（SSoT：docs/arch/10-Knowledge-RAG-深度选型.md §2.2）。
// 与 M5 的权重体系刻意不同：知识库以向量语义为主、BM25 为辅、图遍历只做补充。
// 这三个常数是重构前 rrfThreeWay 的硬编码权重，下沉到统一融合管线后必须显式传入
// ——否则会退化成 M5 的默认权重（1.0/0.6/0.6），把 M10 的检索策略悄悄改掉。
const (
	knowledgeBM25Weight   = 0.3
	knowledgeVectorWeight = 0.6
	knowledgeGraphWeight  = 0.1
	knowledgeRRFk         = 60
	defaultKnowledgeTopK  = 10
)

// knowledgeDocumentSource 把 M10 的三路召回适配到 store/search 的统一融合管线
// （GD-13-002）。
//
// Source 字段语义：融合层按 ScoredFragment.Source 聚合去重，M10 的去重键必须是
// **chunk ID**（同一文档的不同分块是彼此独立的召回单元，按 SourceURI 聚合会把
// 它们错误地折叠成一条）。而对外输出的 Source 必须是 SourceURI（调用方据此做
// 引用溯源展示，重构前 toScoredFragments 即如此）。两个语义靠 uriByID 在融合
// 之后回填切换，见 retriever.go Search 尾部。
type knowledgeDocumentSource struct {
	hr      *HybridRetrieverImpl
	isMacro bool

	// uriByID 记录本次检索中出现过的 chunk ID → SourceURI 映射，供融合后回填。
	// 各路召回在并行 goroutine 中写入，必须经 setURI 加锁。
	uriByID map[string]string
	// macro 宏观查询命中的 Community 摘要，需在融合结果之前置顶（不参与 RRF）。
	macro []types.ScoredFragment

	mu sync.Mutex
}

var _ search.DocumentSource = (*knowledgeDocumentSource)(nil)

func (k *knowledgeDocumentSource) setURI(id, uri string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.uriByID == nil {
		k.uriByID = make(map[string]string)
	}
	k.uriByID[id] = uri
}

// resolveURI 把融合结果的 Source 从 chunk ID 还原为 SourceURI。
// 未登记（如宏观 Community 摘要伪 ID）时原样保留，不静默丢结果。
func (k *knowledgeDocumentSource) resolveURI(id string) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	if uri, ok := k.uriByID[id]; ok && uri != "" {
		return uri
	}
	return id
}

func (k *knowledgeDocumentSource) SearchBM25(ctx context.Context, query string, topK int) ([]types.ScoredFragment, error) {
	// 宏观查询：Community 摘要**不参与 RRF 融合**，而是在融合结果之前整体置顶
	// （重构前 `finalResults = append(macroResults, finalResults...)` 的语义）。
	// 若把它们混入 BM25 路，其 Score 为 0 会被排到该路末尾，"宏观查询优先看社区
	// 摘要"的 GraphRAG 行为就消失了。
	if k.isMacro {
		k.collectMacroSummaries(ctx, topK)
	}

	ftsRes, err := k.hr.searchFTS(ctx, query, topK)
	if err != nil {
		return nil, err //nolint:wrapcheck // 由 Search 统一包装为 CodeInternal
	}
	results := make([]types.ScoredFragment, 0, len(ftsRes))
	for _, c := range ftsRes {
		results = append(results, k.chunkToFragment(c))
	}
	return results, nil
}

// collectMacroSummaries 检索 level>=1 的 Community 实体摘要（宏观查询专用）。
func (k *knowledgeDocumentSource) collectMacroSummaries(ctx context.Context, topK int) {
	rows, err := k.hr.db.QueryContext(ctx, `
		SELECT id, properties
		FROM semantic_entities
		WHERE entity_type = 'Community' AND json_extract(properties, '$.level') >= 1
		ORDER BY json_extract(properties, '$.level') DESC
		LIMIT ?`, topK)
	if err != nil {
		slog.WarnContext(ctx, "knowledge: macro community query failed, skipping summary pinning", "err", err)
		return
	}
	defer rows.Close()

	var macro []types.ScoredFragment
	for rows.Next() {
		var id int
		var props string
		if scanErr := rows.Scan(&id, &props); scanErr != nil {
			slog.WarnContext(ctx, "knowledge: macro community row scan failed, skipping row", "err", scanErr)
			continue
		}
		macro = append(macro, types.ScoredFragment{
			Source:      fmt.Sprintf("macro-community-%d", id),
			Content:     "【社区摘要】" + props,
			ExplainBits: types.BitGraph,
		})
	}
	if err := rows.Err(); err != nil {
		slog.WarnContext(ctx, "knowledge: macro community rows iteration failed", "err", err)
	}

	k.mu.Lock()
	k.macro = macro
	k.mu.Unlock()
}

// macroResults 返回置顶的宏观摘要（并发安全读）。
func (k *knowledgeDocumentSource) macroResults() []types.ScoredFragment {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.macro
}

func (k *knowledgeDocumentSource) SearchVector(ctx context.Context, embedding []float32, topK int) ([]types.ScoredFragment, error) {
	if k.hr.embedder == nil {
		return nil, nil
	}

	var vecResults []Chunk
	var err error
	if k.hr.cognitive != nil {
		hits, vecErr := k.hr.cognitive.VecKNN(embedding, topK)
		if vecErr == nil {
			vecResults, err = k.hr.fetchCognitiveHits(ctx, hits)
		} else {
			slog.WarnContext(ctx, "knowledge: SurrealDB VecKNN failed, degrading to linear scan", "err", vecErr)
			vecResults, err = k.hr.searchVectorFallback(ctx, embedding, topK)
		}
	} else {
		vecResults, err = k.hr.searchVectorFallback(ctx, embedding, topK)
	}
	if err != nil {
		// 向量路失败只降级（重构前 `if err == nil { vecResults = vr }` 的语义），
		// 但必须可观测——此前是彻底静默的 `_`。
		slog.WarnContext(ctx, "knowledge: vector recall failed, degrading to BM25/graph only", "err", err)
		return nil, nil
	}

	results := make([]types.ScoredFragment, 0, len(vecResults))
	for _, c := range vecResults {
		results = append(results, k.chunkToFragment(c))
	}
	return results, nil
}

func (k *knowledgeDocumentSource) SearchGraph(ctx context.Context, query string, topK int) ([]types.ScoredFragment, error) {
	if k.hr.graph == nil {
		return nil, nil
	}
	gr, err := k.hr.graph.TraverseChunks(ctx, query, topK)
	if err != nil {
		return nil, err //nolint:wrapcheck // 融合层降级处理并告警
	}
	results := make([]types.ScoredFragment, 0, len(gr))
	for _, c := range gr {
		results = append(results, k.chunkToFragment(c))
	}
	return results, nil
}

// chunkToFragment 融合期表示：Source=chunk ID（去重键），并登记 ID→URI 供融合后回填。
//
// Score 刻意保持 0：三路召回结果本身已按各自的相关性排好序（FTS rank /
// 向量相似度 / 图遍历序），融合层用 SliceStable 保留该次序作为 RRF rank，
// 与重构前 rrfThreeWay 直接以入参下标为 rank 的行为完全一致。
func (k *knowledgeDocumentSource) chunkToFragment(c Chunk) types.ScoredFragment {
	k.setURI(c.ID, c.SourceURI)
	return types.ScoredFragment{
		Source:     c.ID,
		Content:    c.Content,
		TaintLevel: types.TaintLevel(c.TaintLevel),
	}
}
