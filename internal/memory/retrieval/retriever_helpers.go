package retrieval

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/polarisagi/polaris/internal/memory/util"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// HybridRetriever 辅助检索函数（R7 拆分自 retriever.go）：
// 向量余弦相似度、Tier0 SQL 向量回退扫描、SurrealDB FTS 检索、Semantic Entities 检索。
// 主 Search 流程见 retriever.go；结构体与构造函数见 retriever_construct.go。
// ============================================================================

// bm25Score 计算 query 与 content 的 BM25 近似分（Tier 0 纯 Go，无 FTS5 扩展）。
// 算法: 命中词数/总词数 × IDF 近似（log(1+1/freq)）。

func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		v1 := float64(a[i])
		v2 := float64(b[i])
		dot += v1 * v2
		normA += v1 * v1
		normB += v2 * v2
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

func (hr *HybridRetrieverImpl) fetchVectorResultsFromSQL(ctx context.Context, db protocol.SQLQuerier, queryF32 []float32, scanLimit int) []types.ScoredFragment {
	var vectorResults []types.ScoredFragment
	if scanLimit <= 0 {
		scanLimit = 500
	}
	// 按时间倒序提取最近的 scanLimit 条带向量记录参与相似度计算
	rows, queryErr := db.QueryContext(ctx, fmt.Sprintf("SELECT content, embedding FROM episodic_events WHERE embedding IS NOT NULL ORDER BY id DESC LIMIT %d", scanLimit))
	if queryErr != nil {
		return vectorResults
	}
	defer rows.Close()

	for rows.Next() {
		var content string
		var embBlob []byte
		if scanErr := rows.Scan(&content, &embBlob); scanErr != nil {
			continue
		}

		vec := DecodeFloat16(embBlob)
		if vec == nil {
			continue
		}

		score := cosineSimilarity(queryF32, vec)
		if score > 0.5 {
			// Gap-D：向量相似度 > 0.85 → HighVector；否则 WeakSemantic
			evidType := types.EvidenceWeakSemantic
			if score >= 0.85 {
				evidType = types.EvidenceHighVector
			}
			// fetchVectorResultsFromSQL 来自 episodic_events：用户原始输入域，TaintHigh
			vectorResults = append(vectorResults, types.ScoredFragment{
				Content:      content,
				Score:        score,
				Source:       content,
				EvidenceType: evidType,
				TaintLevel:   types.TaintHigh,
			})
		}
	}
	return vectorResults
}

func (hr *HybridRetrieverImpl) searchCognitiveFTS(ctx context.Context, query string, finalTopK int, asOf int64) []types.ScoredFragment {
	var results []types.ScoredFragment
	if hits, ftsErr := hr.cognitive.FTSSearch(query, finalTopK*5+30); ftsErr == nil {
		for _, h := range hits {
			content, src, taint, ok := hr.resolveCognitiveHit(ctx, h.ID, asOf)
			if !ok {
				continue
			}
			results = append(results, types.ScoredFragment{
				Content:      content,
				Score:        h.Score,
				Source:       src,
				EvidenceType: types.EvidenceFTSKeyword,
				TaintLevel:   taint,
			})
		}
	}
	return results
}

func (hr *HybridRetrieverImpl) resolveCognitiveHit(ctx context.Context, hitID string, asOf int64) (content string, src string, taint types.TaintLevel, ok bool) {
	if strings.HasPrefix(hitID, "sement_") {
		return hr.resolveSemanticHit(ctx, hitID, asOf)
	}

	// Episodic event
	content = hitID
	if raw, kvErr := hr.store.Get(ctx, []byte("episodic:"+hitID)); kvErr == nil {
		var ev types.Event
		if jsonErr := json.Unmarshal(raw, &ev); jsonErr == nil {
			content = string(ev.Payload)
		}
	}
	src = "episodic:" + hitID
	return content, src, taintForSource(src), true
}

func (hr *HybridRetrieverImpl) resolveSemanticHit(ctx context.Context, hitID string, asOf int64) (string, string, types.TaintLevel, bool) {
	parts := strings.SplitN(hitID, "_", 3)
	if len(parts) != 3 || hr.semantic == nil {
		return "", "", 0, false
	}
	ent, err := hr.semantic.GetEntity(ctx, parts[1], parts[2])
	if err != nil || ent == nil {
		return "", "", 0, false
	}
	if asOf > 0 {
		if ent.ValidFrom > 0 && ent.ValidFrom > asOf {
			return "", "", 0, false
		}
		if ent.ValidUntil > 0 && ent.ValidUntil <= asOf {
			return "", "", 0, false
		}
	}
	var propStr string
	if b, merr := json.Marshal(ent.Properties); merr == nil {
		propStr = string(b)
	}
	return ent.Name + " " + propStr, ent.ID, ent.TaintLevel, true
}

func (hr *HybridRetrieverImpl) searchSemanticEntities(ctx context.Context, query string, asOf int64) []types.ScoredFragment {
	var semanticResults []types.ScoredFragment
	entities, err := hr.semantic.SearchEntities(ctx, query, 20, asOf)
	if err == nil {
		for _, ent := range entities {
			var propStr string
			if b, merr := json.Marshal(ent.Properties); merr == nil {
				propStr = string(b)
			}
			content := ent.Name + " " + propStr
			if s := util.Bm25Score(query, content); s > 0 {
				src := ent.ID
				semanticResults = append(semanticResults, types.ScoredFragment{
					Content:      content,
					Score:        s,
					Source:       src,
					EvidenceType: types.EvidenceFTSKeyword,
					TaintLevel:   ent.TaintLevel,
				})
			}
		}
	}
	return semanticResults
}
