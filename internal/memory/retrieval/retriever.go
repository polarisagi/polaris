package retrieval

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"

	"github.com/polarisagi/polaris/internal/memory/util"
	"github.com/polarisagi/polaris/internal/prompt/optimizer"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// 位常量重导出自 pkg/types（唯一权威源，Batch8 ExplainBits 归因修复时上提，
// 见 pkg/types/models_memory.go 注释）。保留这些别名是为了不必改动本文件内
// 已有的所有 BitXxx 引用点。
const (
	BitBM25       = types.BitBM25
	BitSimhash    = types.BitSimhash
	BitVector     = types.BitVector
	BitGraph      = types.BitGraph
	BitReflection = types.BitReflection
	BitDurative   = types.BitDurative
	BitSemantic   = types.BitSemantic
)

// ============================================================================
// HybridRetriever — BM25 + Dense Vector + Graph 三路融合检索（与 M10 共享）
// 结构体定义与构造函数见 retriever_construct.go；辅助检索函数见 retriever_helpers.go（R7 拆分）。
// ============================================================================

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

	// Stage 0.6 — 计算 task_type（M05 §12.3 漂移降级判断用；不写回 RetrievalConfig，
	// 内部计算内部消费，避免改动调用方签名，见 ADR-0053 设计讨论）。
	taskType := optimizer.ExtractTaskType(query)

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
					content, src, taint, ok := hr.resolveCognitiveHit(ctx, h.ID, config.AsOf)
					if !ok {
						continue
					}
					// Vector hits could upgrade the taint level if the source was high taint,
					// resolveCognitiveHit already handles taint level appropriately.
					vectorResults = append(vectorResults, types.ScoredFragment{
						Content:      content,
						Score:        h.Score,
						Source:       src,
						EvidenceType: et,
						TaintLevel:   taint,
					})
				}
			}
		} else if scope.Type == "memory" {
			// Tier0 降级：SQLite episodic_events float16 BLOB + Go 余弦
			if sqlStore, ok := hr.store.(protocol.SQLQuerier); ok {
				vectorResults = hr.fetchVectorResultsFromSQL(ctx, sqlStore, queryF32, config.Tier0VectorScanLimit)
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
		bm25Results = append(bm25Results, hr.searchCognitiveFTS(ctx, query, config.FinalTopK, config.AsOf)...)
	}

	// 第 5 路（temporal 查询激活）：DurativeMemory 持续性记忆簇
	var durativeResults []types.ScoredFragment
	if scope.Type == "memory" && hr.durative != nil && hr.queryClassifier().ClassifyQuerySemantic(ctx, query, hr.embedder) == QueryTypeTemporal {
		groups := hr.durative.ListGroups(ctx, query, 5)
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
			entries, rerr := hr.reflectionMem.ListReflections(ctx, types.ReflectionQuery{
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

	// 第 6 路（P0-2）：Semantic Entities 召回。
	// scope "semantic"（memory_search layer=semantic）时同样生效——事实类记忆是该 layer 的主体。
	var semanticResults []types.ScoredFragment
	if (scope.Type == "memory" || scope.Type == "semantic") && hr.semantic != nil {
		semanticResults = hr.searchSemanticEntities(ctx, query, config.AsOf)
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
				var content string
				if raw, kvErr := hr.store.Get(ctx, []byte("episodic:"+n.ID)); kvErr == nil {
					var ev types.Event
					if jsonErr := json.Unmarshal(raw, &ev); jsonErr == nil {
						content = string(ev.Payload)
					} else {
						slog.Debug("polaris: skipping graph node due to json unmarshal error", "node_id", n.ID, "err", jsonErr)
						continue
					}
				} else {
					slog.Debug("polaris: skipping graph node due to kv missing", "node_id", n.ID, "err", kvErr)
					continue
				}
				graphResults = append(graphResults, types.ScoredFragment{
					Content:      content,
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
	explainMap := make(map[string]uint8)               // key → 解释位图

	addRRF := func(results []types.ScoredFragment, weight float64, bit uint8) {
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
	// M05 §12.3 降级表：Embedding DriftDetector 检测到漂移 → 该 task_type 降级
	// 纯 BM25，其余 task_type 不受影响。Blue-Green 重嵌完成后 DriftOrchestrator
	// 清除降级标记，此处自动恢复正常权重（无需额外代码路径）。
	if hr.driftRegistry != nil && hr.driftRegistry.IsDowngraded(taskType) {
		vw = 0
	}
	gw := config.GraphWeight
	if gw <= 0 {
		gw = 0.6
	}

	addRRF(bm25Results, bw, BitBM25)
	addRRF(simhashResults, bw*0.8, BitSimhash)     // Simhash 路径权重基于 BM25 缩放
	addRRF(vectorResults, vw, BitVector)           // Vector 稠密向量召回
	addRRF(graphResults, gw, BitGraph)             // Graph 路径（Tier1+，仅有图时生效）
	addRRF(reflectionResults, 0.15, BitReflection) // 第 4 路：跨会话 ReflectionMem（M05 §7）
	addRRF(durativeResults, 0.3, BitDurative)      // 第 5 路：DurativeMemory（temporal 查询激活）
	addRRF(semanticResults, 0.9, BitSemantic)      // 第 6 路：Semantic Entities（事实类记忆，权重较高）

	// Stage 3 — 汇总 + BM25 精排（按 RRF 分降序即等效精排）
	var merged []types.ScoredFragment //nolint:prealloc
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
	sort.Slice(merged, func(i, j int) bool { return merged[i].Score > merged[j].Score })

	// Stage 4 — TopK 截断
	topK := config.FinalTopK
	if topK <= 0 {
		topK = 20
	}
	if len(merged) > topK {
		merged = merged[:topK]
	}

	// 记录最终合并结果的位图指标
	recordExplainBitMetrics(ctx, merged)

	// Stage 5 — 漂移检测 anchor 采样（M05 §12.3，见 retriever_helpers.go sampleDriftAnchor）
	hr.sampleDriftAnchor(taskType, query, queryF32, merged)

	return merged, nil
}
