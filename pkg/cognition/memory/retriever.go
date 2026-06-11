package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"sort"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol"
)

// ============================================================================
// HybridRetriever — BM25 + Dense Vector + Graph 三路融合检索（与 M10 共享）
// ============================================================================

// GraphTraverser consumer-side 接口：Tier1+ 图遍历路径（由 SurrealDBCoreStore 实现）。
// consumer-side 定义，防止包循环依赖。
type GraphTraverser interface {
	GraphTraverse(startID, edgeType string, maxDepth int) ([]string, error)
	GraphRelate(fromID, edgeType, toID string, weight float64) error
}

// CognitiveSearchResult 认知检索结果（consumer-side 类型，避免引入 substrate/storage 依赖）。
type CognitiveSearchResult struct {
	ID    string
	Score float64
}

// CognitiveSearcher consumer-side 接口：SurrealDB FTS + HNSW 向量检索与索引写入（Tier1+）。
// consumer-side 定义于 memory 包，防止与 substrate/storage 循环依赖。
// nil 时自动降级 Tier0 路径（纯 Go BM25 + SQLite BLOB 内存余弦）。
type CognitiveSearcher interface {
	FTSIndex(docID, text string) error
	VecUpsert(id string, embedding []float32) error
	VecKNN(query []float32, k int) ([]CognitiveSearchResult, error)
	FTSSearch(query string, k int) ([]CognitiveSearchResult, error)
}

type HybridRetrieverImpl struct {
	store         protocol.Store
	graph         GraphTraverser            // Tier1+：图遍历路径，nil 时跳过
	durative      *DurativeMemoryManager    // 第 5 路（temporal 查询激活），nil 时跳过
	reflectionMem protocol.ReflectionMemory // 第 4 路：SQL 实现优先，nil 时降级 KV 扫描
	embedder      Embedder                  // P0：稠密向量检索
	cognitive     CognitiveSearcher         // Tier1+：SurrealDB FTS+HNSW，nil 时降级 Tier0
}

// InjectEmbedder 注入 M1 Embedding 接口，激活向量检索路径
func (hr *HybridRetrieverImpl) InjectEmbedder(e Embedder) {
	hr.embedder = e
}

func NewHybridRetriever(store protocol.Store) *HybridRetrieverImpl {
	return &HybridRetrieverImpl{store: store}
}

// NewHybridRetrieverWithGraph 创建含图路径的 HybridRetriever（Tier1+）。
func NewHybridRetrieverWithGraph(store protocol.Store, graph GraphTraverser) *HybridRetrieverImpl {
	return &HybridRetrieverImpl{store: store, graph: graph}
}

// NewHybridRetrieverWithDurative 创建含 DurativeMemory 第 5 路的 HybridRetriever。
func NewHybridRetrieverWithDurative(store protocol.Store, graph GraphTraverser, durative *DurativeMemoryManager) *HybridRetrieverImpl {
	return &HybridRetrieverImpl{store: store, graph: graph, durative: durative}
}

// NewHybridRetrieverFull 创建全路径 HybridRetriever（Graph + Durative + ReflectionMem）。
// reflectionMem 非 nil 时第 4 路走 SQL 查询；nil 时降级到 KV 前缀扫描（兼容旧部署）。
func NewHybridRetrieverFull(store protocol.Store, graph GraphTraverser, durative *DurativeMemoryManager, reflectionMem protocol.ReflectionMemory) *HybridRetrieverImpl {
	return &HybridRetrieverImpl{store: store, graph: graph, durative: durative, reflectionMem: reflectionMem}
}

// NewHybridRetrieverWithCognitive 创建含 SurrealDB FTS+HNSW 路径的全功能 HybridRetriever（Tier1+）。
// cognitive 注入后：BM25 路径走 SurrealDB BM25 FTS；向量路径走 SurrealDB HNSW KNN。
// cognitive == nil 时自动降级为 Tier0（纯 Go BM25 + SQLite BLOB 余弦）。
func NewHybridRetrieverWithCognitive(store protocol.Store, graph GraphTraverser, durative *DurativeMemoryManager, reflectionMem protocol.ReflectionMemory, cognitive CognitiveSearcher) *HybridRetrieverImpl {
	return &HybridRetrieverImpl{store: store, graph: graph, durative: durative, reflectionMem: reflectionMem, cognitive: cognitive}
}

func (hr *HybridRetrieverImpl) Search(ctx context.Context, query string, scope protocol.SearchScope, config protocol.RetrievalConfig) ([]protocol.ScoredFragment, error) { //nolint:gocyclo,nestif
	// Stage 0 — 确定扫描前缀（隐私门控由调用方 M11 注入，此处按 scope 路由）
	prefix := []byte("chunk:")
	if scope.Type == "memory" {
		prefix = []byte("episodic:")
	}

	// Stage 0.5 — 计算查询向量
	var queryF32 []float32
	if hr.embedder != nil && query != "" {
		if qVec, err := hr.embedder.Embed(ctx, query); err == nil {
			queryF32 = qVec
		}
	}

	// Stage 1 — 并行宽召回（BM25 + Simhash + Graph 三路）
	var bm25Results []protocol.ScoredFragment
	var simhashResults []protocol.ScoredFragment
	var graphResults []protocol.ScoredFragment
	var vectorResults []protocol.ScoredFragment

	// P0：向量检索 — Tier1+ 走 SurrealDB HNSW（O(log N)），Tier0 降级 SQLite BLOB 内存余弦
	if queryF32 != nil { //nolint:nestif
		if hr.cognitive != nil {
			// Tier1+：SurrealDB HNSW KNN（原生余弦，O(log N)）
			if hits, vecErr := hr.cognitive.VecKNN(queryF32, config.FinalTopK*3+30); vecErr == nil {
				for _, h := range hits {
					et := protocol.EvidenceWeakSemantic
					if h.Score >= 0.85 {
						et = protocol.EvidenceHighVector
					}
					// SurrealDB 返回 doc_id；尝试从 KV 取原文，失败时以 ID 占位
					content := h.ID
					if raw, kvErr := hr.store.Get(ctx, []byte("episodic:"+h.ID)); kvErr == nil {
						var ev protocol.Event
						if jsonErr := json.Unmarshal(raw, &ev); jsonErr == nil {
							content = string(ev.Payload)
						}
					}
					vectorResults = append(vectorResults, protocol.ScoredFragment{
						Content:      content,
						Score:        h.Score,
						Source:       "episodic:" + h.ID,
						EvidenceType: et,
					})
				}
			}
		} else if scope.Type == "memory" {
			// Tier0 降级：SQLite episodic_events float16 BLOB + Go 余弦
			if dba, ok := hr.store.(DBAccessor); ok {
				vectorResults = hr.fetchVectorResultsFromSQL(ctx, dba.DB(), queryF32)
			}
		}
	}

	// scanAndScore 扫描 KV 前缀：Tier1+ 时只计算 Simhash（BM25 由 FTSSearch 接管）；
	// Tier0 时同时计算 BM25 + Simhash。
	scanAndScore := func(scanPrefix []byte) {
		iter, err := hr.store.Scan(ctx, scanPrefix)
		if err != nil || iter == nil {
			return
		}
		defer iter.Close()
		queryFP := SimhashOf(query)
		for iter.Next() {
			content := string(iter.Value())
			src := string(iter.Key())

			// BM25 Tier0 路径：cognitive 有效时由 FTSSearch 接管，不在此重复计算
			if hr.cognitive == nil {
				if score := bm25Score(query, content); score > 0 {
					bm25Results = append(bm25Results, protocol.ScoredFragment{
						Content:      content,
						Score:        score,
						Source:       src,
						EvidenceType: protocol.EvidenceFTSKeyword,
					})
				}
			}

			contentFP := SimhashOf(content)
			if dist := queryFP.Hamming(contentFP); dist <= 16 {
				simScore := 1.0 - float64(dist)/64.0
				// Simhash 近似匹配：相似度高时归类 HighVector，否则 WeakSemantic
				evidType := protocol.EvidenceWeakSemantic
				if simScore >= 0.85 {
					evidType = protocol.EvidenceHighVector
				}
				simhashResults = append(simhashResults, protocol.ScoredFragment{
					Content:      content,
					Score:        simScore,
					Source:       src,
					EvidenceType: evidType,
				})
			}
		}
	}

	scanAndScore(prefix)

	// BM25 路径 — Tier1+：SurrealDB BM25 FTS（k1=1.2 b=0.75 原生）替换 Tier0 纯 Go 近似
	if hr.cognitive != nil && query != "" {
		bm25Results = append(bm25Results, hr.searchCognitiveFTS(ctx, query, config.FinalTopK)...)
	}

	// 第 5 路（temporal 查询激活）：DurativeMemory 持续性记忆簇
	var durativeResults []protocol.ScoredFragment
	if scope.Type == "memory" && hr.durative != nil && ClassifyQuery(query) == QueryTypeTemporal {
		groups := hr.durative.RetrieveGroups(ctx, query, 5)
		for _, g := range groups {
			content := g.Label + ": " + g.Summary
			durativeResults = append(durativeResults, protocol.ScoredFragment{
				Content:      content,
				Score:        bm25Score(query, content),
				Source:       "durative_group:" + g.ID,
				EvidenceType: protocol.EvidenceFTSKeyword,
			})
		}
	}

	// 第 4 路（M05 §7，权重 0.15）：跨会话 ReflectionMemory 召回
	// 优先通过接口走 SQL 查询（SQLReflectionMem）；接口未注入时降级 KV 前缀扫描（旧部署兼容）。
	var reflectionResults []protocol.ScoredFragment
	if scope.Type == "memory" { //nolint:nestif
		if hr.reflectionMem != nil {
			// SQL 路径：利用 idx_reflect_task_type 索引，避免全表扫描
			entries, rerr := hr.reflectionMem.QueryReflections(ctx, protocol.ReflectionQuery{
				Topic: query,
				K:     20,
			})
			if rerr == nil {
				for _, e := range entries {
					content := e.Decision + " " + e.Strategy
					if s := bm25Score(query, content); s > 0 {
						reflectionResults = append(reflectionResults, protocol.ScoredFragment{
							Content:      content,
							Score:        s,
							Source:       "reflection:" + e.ID,
							EvidenceType: protocol.EvidenceFTSKeyword,
						})
					}
				}
			}
		} else {
			// KV 降级路径（旧 ReflectionMem 兼容）
			rIter, err := hr.store.Scan(ctx, []byte("reflection:"))
			if err == nil && rIter != nil {
				defer rIter.Close()
				for rIter.Next() {
					content := string(rIter.Value())
					src := string(rIter.Key())
					if s := bm25Score(query, content); s > 0 {
						reflectionResults = append(reflectionResults, protocol.ScoredFragment{
							Content:      content,
							Score:        s,
							Source:       src,
							EvidenceType: protocol.EvidenceFTSKeyword,
						})
					}
				}
			}
		}
	}

	// Stage 1c — Graph 路径（Tier1+）：从 BM25 Top 结果的 source 出发做图遍历
	if hr.graph != nil && len(bm25Results) > 0 {
		top := bm25Results[0].Source // 以 BM25 最高分作为图遍历起点
		neighbors, err := hr.graph.GraphTraverse(top, "", 2)
		if err == nil {
			for rank, nb := range neighbors {
				// 图邻居按跳数衰减赋分：第1跳 0.7，第2跳 0.5
				score := 0.7 / float64(rank/2+1)
				graphResults = append(graphResults, protocol.ScoredFragment{
					Content:      nb, // 图路径用节点 ID 作为 Content 占位（调用方可二次 KV 取原文）
					Score:        score,
					Source:       nb,
					EvidenceType: protocol.EvidenceWeakSemantic, // 图路径：结构关联，非内容匹配
				})
			}
		}
	}

	// Stage 2 — RRF 融合 (k=60)
	// score(d) = Σ weight_i / (k + rank_i + 1)
	const rrfK = 60.0
	scoreMap := make(map[string]float64)                  // key → RRF 累计分
	contentMap := make(map[string]string)                 // key → content
	evidenceMap := make(map[string]protocol.EvidenceType) // key → 最高权重路径的证据类型

	addRRF := func(results []protocol.ScoredFragment, weight float64) {
		// 按 Score 降序排列后赋 rank
		sorted := make([]protocol.ScoredFragment, len(results))
		copy(sorted, results)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].Score > sorted[j].Score })
		for rank, frag := range sorted {
			contribution := weight / (rrfK + float64(rank) + 1)
			prev := scoreMap[frag.Source]
			scoreMap[frag.Source] += contribution
			// 保留贡献最大那一路的证据类型（首次出现或新路贡献更大时更新）
			if frag.EvidenceType != "" && (prev == 0 || contribution > prev) {
				evidenceMap[frag.Source] = frag.EvidenceType
			}
			contentMap[frag.Source] = frag.Content
		}
	}
	addRRF(bm25Results, 1.0)
	addRRF(simhashResults, 0.8)     // Simhash 路径权重略低于 BM25
	addRRF(vectorResults, 0.6)      // Vector 稠密向量召回
	addRRF(graphResults, 0.6)       // Graph 路径（Tier1+，仅有图时生效）
	addRRF(reflectionResults, 0.15) // 第 4 路：跨会话 ReflectionMem（M05 §7）
	addRRF(durativeResults, 0.3)    // 第 5 路：DurativeMemory（temporal 查询激活）

	// Stage 3 — 汇总 + BM25 精排（按 RRF 分降序即等效精排）
	var merged []protocol.ScoredFragment //nolint:prealloc
	for src, score := range scoreMap {
		merged = append(merged, protocol.ScoredFragment{
			Content:      contentMap[src],
			Score:        score,
			Source:       src,
			EvidenceType: evidenceMap[src],
		})
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Score > merged[j].Score })

	// Stage 4 — TopK 截断
	topK := config.FinalTopK
	if topK <= 0 {
		topK = 20
	}
	if len(merged) > topK {
		merged = merged[:topK]
	}
	return merged, nil
}

// bm25Score 计算 query 与 content 的 BM25 近似分（Tier 0 纯 Go，无 FTS5 扩展）。
// 算法: 命中词数/总词数 × IDF 近似（log(1+1/freq)）。
func bm25Score(query, content string) float64 {
	if query == "" {
		return 1.0 // 空 query 全召回
	}
	queryTokens := tokenize(query)
	contentTokens := tokenize(content)
	if len(contentTokens) == 0 {
		return 0
	}
	// 构建内容词频 map
	freq := make(map[string]int, len(contentTokens))
	for _, t := range contentTokens {
		freq[t]++
	}
	var score float64
	for _, qt := range queryTokens {
		if f, ok := freq[qt]; ok {
			// TF × IDF 近似
			tf := float64(f) / float64(len(contentTokens))
			idf := 1.0 + 1.0/float64(f+1)
			score += tf * idf
		}
		// 子串命中（BM25 降级）
		if score == 0 && strings.Contains(strings.ToLower(content), strings.ToLower(qt)) {
			score += 0.1
		}
	}
	return score
}

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

func (hr *HybridRetrieverImpl) fetchVectorResultsFromSQL(ctx context.Context, db *sql.DB, queryF32 []float32) []protocol.ScoredFragment {
	var vectorResults []protocol.ScoredFragment
	// 按时间倒序提取最近的 500 条带向量记录参与相似度计算
	rows, queryErr := db.QueryContext(ctx, "SELECT content, embedding FROM episodic_events WHERE embedding IS NOT NULL ORDER BY id DESC LIMIT 500")
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
			evidType := protocol.EvidenceWeakSemantic
			if score >= 0.85 {
				evidType = protocol.EvidenceHighVector
			}
			vectorResults = append(vectorResults, protocol.ScoredFragment{
				Content:      content,
				Score:        score,
				Source:       content,
				EvidenceType: evidType,
			})
		}
	}
	return vectorResults
}
func (hr *HybridRetrieverImpl) searchCognitiveFTS(ctx context.Context, query string, finalTopK int) []protocol.ScoredFragment {
	var results []protocol.ScoredFragment
	if hits, ftsErr := hr.cognitive.FTSSearch(query, finalTopK*5+30); ftsErr == nil {
		for _, h := range hits {
			content := h.ID
			if raw, kvErr := hr.store.Get(ctx, []byte("episodic:"+h.ID)); kvErr == nil {
				var ev protocol.Event
				if jsonErr := json.Unmarshal(raw, &ev); jsonErr == nil {
					content = string(ev.Payload)
				}
			}
			results = append(results, protocol.ScoredFragment{
				Content:      content,
				Score:        h.Score,
				Source:       "episodic:" + h.ID,
				EvidenceType: protocol.EvidenceFTSKeyword,
			})
		}
	}
	return results
}
