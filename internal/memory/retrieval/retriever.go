package retrieval

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/polarisagi/polaris/internal/prompt"
	"github.com/polarisagi/polaris/internal/store/search"

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
//
// 融合算法自 GD-13-002 起下沉至 internal/store/search.HybridSearch（M5/M10 共用）；
// 本包只负责 M5 领域的召回实现（memoryDocumentSource，见 source.go）与下述默认值。
// ============================================================================

// M5 检索默认值（SSoT：docs/arch/spec/state.yaml §retrieval）。
// store/search 融合层按字面值使用权重，默认值必须在本领域层解析——
// 因为"权重 0"在 M05 §12.3 漂移降级场景下是有意义的显式取值，不是"未设置"。
const (
	defaultBM25Weight   = 1.0
	defaultVectorWeight = 0.6
	defaultGraphWeight  = 0.6
	defaultMemoryTopK   = 20
)

// resolveWeight 把"未设置（<=0）"解析为领域默认值。
func resolveWeight(configured, fallback float64) float64 {
	if configured <= 0 {
		return fallback
	}
	return configured
}

// searchGraphSpreadingActivation 执行 Stage 1c 图路径召回：取 BM25 Top-3 作为种子节点，
// 通过 Spreading Activation 多种子能量扩散找到关联的 episodic 记忆（从 Search 拆出，
// nestif 治理，行为不变）。调用方需保证 hr.graph != nil && len(bm25Results) > 0。
func (hr *HybridRetrieverImpl) searchGraphSpreadingActivation(ctx context.Context, bm25Results []types.ScoredFragment) []types.ScoredFragment {
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

	nodes, err := hr.graph.SpreadingActivation(seedIDs, saMaxDepth, saEnergyDecay, saDormancyThreshold, saFanOutLimit)
	if err != nil {
		return nil
	}

	var graphResults []types.ScoredFragment
	for _, n := range nodes {
		raw, kvErr := hr.store.Get(ctx, []byte("episodic:"+n.ID))
		if kvErr != nil {
			slog.Debug("polaris: skipping graph node due to kv missing", "node_id", n.ID, "err", kvErr)
			continue
		}
		var ev types.Event
		if jsonErr := json.Unmarshal(raw, &ev); jsonErr != nil {
			slog.Debug("polaris: skipping graph node due to json unmarshal error", "node_id", n.ID, "err", jsonErr)
			continue
		}
		graphResults = append(graphResults, types.ScoredFragment{
			Content:      string(ev.Payload),
			Score:        n.Score,
			Source:       n.ID,
			EvidenceType: types.EvidenceWeakSemantic,
			TaintLevel:   taintForSource(n.ID),
		})
	}
	return graphResults
}

func (hr *HybridRetrieverImpl) Search(ctx context.Context, query string, scope types.SearchScope, config types.RetrievalConfig) ([]types.ScoredFragment, error) {
	prefix := []byte("chunk:")
	if scope.Type == "memory" {
		prefix = []byte("episodic:")
	}

	var queryF32 []float32
	if hr.embedder != nil && query != "" {
		if qVec, err := hr.embedder.Embed(ctx, query); err == nil {
			queryF32 = qVec
		}
	}

	// Stage 0.6 — 计算 task_type（M05 §12.3 漂移降级判断用；不写回 RetrievalConfig，
	// 内部计算内部消费，避免改动调用方签名，见 ADR-0062 设计讨论）。
	taskType := prompt.ExtractTaskType(query)

	// 领域默认值必须在**本层**解析完毕后再交给 store/search：融合层按字面值
	// 使用权重（<=0 即关闭该路）。顺序不可交换——先补默认值再判漂移降级，
	// 否则降级置的 0 会被默认值覆盖，M05 §12.3 的降级开关直接失效。
	bw := resolveWeight(config.BM25Weight, defaultBM25Weight)
	vw := resolveWeight(config.VectorWeight, defaultVectorWeight)
	gw := resolveWeight(config.GraphWeight, defaultGraphWeight)

	// M05 §12.3 降级表：Embedding DriftDetector 检测到漂移 → 该 task_type 降级
	// 纯 BM25，其余 task_type 不受影响。Blue-Green 重嵌完成后 DriftOrchestrator
	// 清除降级标记，此处自动恢复正常权重（无需额外代码路径）。
	if hr.driftRegistry != nil && hr.driftRegistry.IsDowngraded(taskType) {
		vw = 0
	}

	topK := config.FinalTopK
	if topK <= 0 {
		topK = defaultMemoryTopK
	}

	if ext, ok := hr.store.(protocol.StoreExtVector); ok {
		mode := 0
		if scope.Type == "memory" && config.FinalTopK > 10 {
			mode = 1
		}
		if err := ext.VecSetMode(mode); err != nil {
			slog.WarnContext(ctx, "failed to set vector mode", "err", err)
		}
	}

	semanticType := hr.queryClassifier().ClassifyQuerySemantic(ctx, query, hr.embedder)
	src := &memoryDocumentSource{
		hr:           hr,
		scope:        scope,
		config:       config,
		prefix:       prefix,
		semanticType: semanticType,
	}

	merged, err := search.HybridSearch(ctx, src, query, queryF32, search.HybridSearchConfig{
		BM25Weight:   bw,
		VectorWeight: vw,
		GraphWeight:  gw,
		RRFk:         config.RRFK,
		TopK:         topK,
		RecallWidth:  topK * 3,
		EnableRerank: false,
	})

	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	// 记录最终合并结果的位图指标
	recordExplainBitMetrics(ctx, merged)

	hr.reinforceHits(merged)

	// Stage 5 — 漂移检测 anchor 采样（M05 §12.3，见 retriever_helpers.go sampleDriftAnchor）
	hr.sampleDriftAnchor(taskType, query, queryF32, merged)

	return merged, nil
}

// reinforceHits 记录本次检索的命中，供 ForgettingManager 做检索强化（GD-14-003）。
//
// 只对**最终进入结果**的片段计数，不含被 RRF 融合淘汰的中间候选——
// 后者不代表"有用"，计入会让强化信号失真，反而保护住噪声。
// Reinforce 只做内存累加，落盘由后台 ticker 批量执行，不拖慢读路径。
func (hr *HybridRetrieverImpl) reinforceHits(merged []types.ScoredFragment) {
	if hr.reinforcer == nil || len(merged) == 0 {
		return
	}
	sources := make([]string, 0, len(merged))
	for _, m := range merged {
		sources = append(sources, m.Source)
	}
	hr.reinforcer.Reinforce(sources)
}
