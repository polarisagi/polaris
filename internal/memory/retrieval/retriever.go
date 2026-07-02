package retrieval

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"strings"

	"github.com/polarisagi/polaris/internal/memory/store"
	"github.com/polarisagi/polaris/internal/memory/util"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// HybridRetriever — BM25 + Dense Vector + Graph 三路融合检索（与 M10 共享）
// ============================================================================

type HybridRetrieverImpl struct {
	store         protocol.Store
	graph         protocol.GraphTraverser      // Tier1+：图遍历路径，nil 时跳过
	durative      *store.DurativeMemoryManager // 第 5 路（temporal 查询激活），nil 时跳过
	reflectionMem protocol.ReflectionMemory    // 第 4 路：SQL 实现优先，nil 时降级 KV 扫描
	embedder      Embedder                     // P0：稠密向量检索
	cognitive     protocol.CognitiveSearcher   // Tier1+：SurrealDB FTS+HNSW，nil 时降级 Tier0
	semantic      protocol.SemanticMemory      // P0-2：第 6 路（semantic_entities）
}

// InjectEmbedder 注入 M1 Embedding 接口，激活向量检索路径
func (hr *HybridRetrieverImpl) InjectEmbedder(e Embedder) {
	hr.embedder = e
}

func NewHybridRetriever(store protocol.Store) *HybridRetrieverImpl {
	return &HybridRetrieverImpl{store: store}
}

// NewHybridRetrieverWithGraph 创建含图路径的 HybridRetriever（Tier1+）。
func NewHybridRetrieverWithGraph(store protocol.Store, graph protocol.GraphTraverser) *HybridRetrieverImpl {
	return &HybridRetrieverImpl{store: store, graph: graph}
}

// NewHybridRetrieverWithDurative 创建含 DurativeMemory 第 5 路的 HybridRetriever。
func NewHybridRetrieverWithDurative(store protocol.Store, graph protocol.GraphTraverser, durative *store.DurativeMemoryManager) *HybridRetrieverImpl {
	return &HybridRetrieverImpl{store: store, graph: graph, durative: durative}
}

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

func (hr *HybridRetrieverImpl) Search(ctx context.Context, query string, scope types.SearchScope, config types.RetrievalConfig) ([]types.ScoredFragment, error) { //nolint:gocyclo,nestif
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
	var bm25Results []types.ScoredFragment
	var simhashResults []types.ScoredFragment
	var graphResults []types.ScoredFragment
	var vectorResults []types.ScoredFragment

	// 根据 scope 决定向量搜索模式（内部聚合分析用近似模式，实时问答用精确模式）
	if ext, ok := hr.store.(protocol.StoreExtVector); ok {
		if scope.Type == "memory" && config.FinalTopK > 10 {
			_ = ext.VecSetMode(1) // 近似 HNSW
		} else {
			_ = ext.VecSetMode(0) // 精确匹配
		}
	}

	// P0：向量检索 — Tier1+ 走 SurrealDB HNSW（O(log N)），Tier0 降级 SQLite BLOB 内存余弦
	if queryF32 != nil { //nolint:nestif
		if hr.cognitive != nil {
			// Tier1+：SurrealDB HNSW KNN（原生余弦，O(log N)）
			if hits, vecErr := hr.cognitive.VecKNN(queryF32, config.FinalTopK*3+30); vecErr == nil {
				for _, h := range hits {
					et := types.EvidenceWeakSemantic
					if h.Score >= 0.85 {
						et = types.EvidenceHighVector
					}
					// SurrealDB 返回 doc_id；尝试从 KV 取原文，失败时以 ID 占位
					content := h.ID
					if raw, kvErr := hr.store.Get(ctx, []byte("episodic:"+h.ID)); kvErr == nil {
						var ev types.Event
						if jsonErr := json.Unmarshal(raw, &ev); jsonErr == nil {
							content = string(ev.Payload)
						}
					}
					src := "episodic:" + h.ID
					vectorResults = append(vectorResults, types.ScoredFragment{
						Content:      content,
						Score:        h.Score,
						Source:       src,
						EvidenceType: et,
						TaintLevel:   taintForSource(src),
					})
				}
			}
		} else if scope.Type == "memory" {
			// Tier0 降级：SQLite episodic_events float16 BLOB + Go 余弦
			if sqlStore, ok := hr.store.(protocol.SQLQuerier); ok {
				vectorResults = hr.fetchVectorResultsFromSQL(ctx, sqlStore, queryF32)
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
		queryFP := util.SimhashOf(query)
		for iter.Next() {
			content := string(iter.Value())
			src := string(iter.Key())

			// BM25 Tier0 路径：cognitive 有效时由 FTSSearch 接管，不在此重复计算
			if hr.cognitive == nil {
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

			contentFP := util.SimhashOf(content)
			if dist := queryFP.Hamming(contentFP); dist <= 16 {
				simScore := 1.0 - float64(dist)/64.0
				// Simhash 近似匹配：相似度高时归类 HighVector，否则 WeakSemantic
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
	}

	scanAndScore(prefix)

	// BM25 路径 — Tier1+：SurrealDB BM25 FTS（k1=1.2 b=0.75 原生）替换 Tier0 纯 Go 近似
	if hr.cognitive != nil && query != "" {
		bm25Results = append(bm25Results, hr.searchCognitiveFTS(ctx, query, config.FinalTopK)...)
	}

	// 第 5 路（temporal 查询激活）：DurativeMemory 持续性记忆簇
	var durativeResults []types.ScoredFragment
	if scope.Type == "memory" && hr.durative != nil && ClassifyQuery(query) == QueryTypeTemporal {
		groups := hr.durative.RetrieveGroups(ctx, query, 5)
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

	// 第 4 路（M05 §7，权重 0.15）：跨会话 ReflectionMemory 召回
	// 优先通过接口走 SQL 查询（SQLReflectionMem）；接口未注入时降级 KV 前缀扫描（旧部署兼容）。
	var reflectionResults []types.ScoredFragment
	if scope.Type == "memory" { //nolint:nestif
		if hr.reflectionMem != nil {
			// SQL 路径：利用 idx_reflect_task_type 索引，避免全表扫描
			entries, rerr := hr.reflectionMem.QueryReflections(ctx, types.ReflectionQuery{
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
			// KV 降级路径（旧 ReflectionMem 兼容）
			rIter, err := hr.store.Scan(ctx, []byte("reflection:"))
			if err == nil && rIter != nil {
				defer rIter.Close()
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
			}
		}
	}

	// 第 6 路（P0-2）：Semantic Entities 召回
	var semanticResults []types.ScoredFragment
	if scope.Type == "memory" && hr.semantic != nil {
		entities, err := hr.semantic.SearchEntities(ctx, query, 20)
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
	}

	// Stage 1c — Graph 路径（Tier1+）：Spreading Activation 多种子能量扩散
	// 设计决策：用 SA 替代原 BFS + 硬编码衰减系数。
	//   - 取 BM25 Top-3 作为种子，比单节点覆盖更广
	//   - SA energy 按边权重传播，分数有物理意义，无需外部硬编码
	//   - 参数：maxDepth=3, energyDecay=0.7, dormancy=0.05, fanOut=10
	if hr.graph != nil && len(bm25Results) > 0 {
		const (
			saMaxSeeds          = 3
			saMaxDepth          = 3
			saEnergyDecay       = 0.7
			saDormancyThreshold = 0.05
			saFanOutLimit       = 10
		)
		seedIDs := make([]string, 0, saMaxSeeds)
		seenSeed := make(map[string]struct{}, saMaxSeeds)
		for _, r := range bm25Results {
			if len(seedIDs) >= saMaxSeeds {
				break
			}
			if _, dup := seenSeed[r.Source]; !dup && r.Source != "" {
				seenSeed[r.Source] = struct{}{}
				seedIDs = append(seedIDs, r.Source)
			}
		}
		if nodes, err := hr.graph.SpreadingActivation(seedIDs, saMaxDepth, saEnergyDecay, saDormancyThreshold, saFanOutLimit); err == nil {
			for _, n := range nodes {
				graphResults = append(graphResults, types.ScoredFragment{
					Content:      n.ID, // 节点 ID 作为 Content 占位；上层可二次 KV 取原文
					Score:        n.Score,
					Source:       n.ID,
					EvidenceType: types.EvidenceWeakSemantic,
					TaintLevel:   taintForSource(n.ID),
				})
			}
		}
	}

	// Stage 2 — RRF 融合 (k=config.RRFK)
	// score(d) = Σ weight_i / (k + rank_i + 1)
	rrfK := float64(config.RRFK)
	if rrfK <= 0 {
		rrfK = 60.0 // 默认值：与 state.yaml 默认一致
	}
	scoreMap := make(map[string]float64)               // key → RRF 累计分
	contentMap := make(map[string]string)              // key → content
	evidenceMap := make(map[string]types.EvidenceType) // key → 最高权重路径的证据类型
	taintMap := make(map[string]types.TaintLevel)      // key → 最高污点等级（只升不降，ADR-0007）

	addRRF := func(results []types.ScoredFragment, weight float64) {
		// 按 Score 降序排列后赋 rank
		sorted := make([]types.ScoredFragment, len(results))
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
			// 污点传播：只升不降（ADR-0007 PropagateTaint）
			if frag.TaintLevel > taintMap[frag.Source] {
				taintMap[frag.Source] = frag.TaintLevel
			}
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

	addRRF(bm25Results, bw)
	addRRF(simhashResults, bw*0.8)  // Simhash 路径权重基于 BM25 缩放
	addRRF(vectorResults, vw)       // Vector 稠密向量召回
	addRRF(graphResults, gw)        // Graph 路径（Tier1+，仅有图时生效）
	addRRF(reflectionResults, 0.15) // 第 4 路：跨会话 ReflectionMem（M05 §7）
	addRRF(durativeResults, 0.3)    // 第 5 路：DurativeMemory（temporal 查询激活）
	addRRF(semanticResults, 0.9)    // 第 6 路：Semantic Entities（事实类记忆，权重较高）

	// Stage 3 — 汇总 + BM25 精排（按 RRF 分降序即等效精排）
	var merged []types.ScoredFragment //nolint:prealloc
	for src, score := range scoreMap {
		merged = append(merged, types.ScoredFragment{
			Content:      contentMap[src],
			Score:        score,
			Source:       src,
			EvidenceType: evidenceMap[src],
			TaintLevel:   taintMap[src],
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

// taintForSource 根据数据来源前缀推断污点等级（ADR-0007）。
// episodic / durative_group = TaintHigh（原始用户输入域）；
// reflection / chunk 及其余 = TaintMedium（LLM 摘要输出地板）。
func taintForSource(source string) types.TaintLevel {
	if strings.HasPrefix(source, "episodic:") || strings.HasPrefix(source, "durative_group:") {
		return types.TaintHigh
	}
	return types.TaintMedium
}

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

func (hr *HybridRetrieverImpl) fetchVectorResultsFromSQL(ctx context.Context, db protocol.SQLQuerier, queryF32 []float32) []types.ScoredFragment {
	var vectorResults []types.ScoredFragment
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
func (hr *HybridRetrieverImpl) searchCognitiveFTS(ctx context.Context, query string, finalTopK int) []types.ScoredFragment {
	var results []types.ScoredFragment
	if hits, ftsErr := hr.cognitive.FTSSearch(query, finalTopK*5+30); ftsErr == nil {
		for _, h := range hits {
			content := h.ID
			if raw, kvErr := hr.store.Get(ctx, []byte("episodic:"+h.ID)); kvErr == nil {
				var ev types.Event
				if jsonErr := json.Unmarshal(raw, &ev); jsonErr == nil {
					content = string(ev.Payload)
				}
			}
			src := "episodic:" + h.ID
			results = append(results, types.ScoredFragment{
				Content:      content,
				Score:        h.Score,
				Source:       src,
				EvidenceType: types.EvidenceFTSKeyword,
				TaintLevel:   taintForSource(src),
			})
		}
	}
	return results
}
