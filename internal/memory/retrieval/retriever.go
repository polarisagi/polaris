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
// ============================================================================

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

	taskType := prompt.ExtractTaskType(query)
	vw := config.VectorWeight
	if hr.driftRegistry != nil && hr.driftRegistry.IsDowngraded(taskType) {
		vw = 0 // M05 §12.3 降级
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
		BM25Weight:   config.BM25Weight,
		VectorWeight: vw,
		GraphWeight:  config.GraphWeight,
		RRFk:         config.RRFK,
		TopK:         config.FinalTopK,
		EnableRerank: false,
	})

	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	// 记录最终合并结果的位图指标
	recordExplainBitMetrics(ctx, merged)

	// Stage 5 — 漂移检测 anchor 采样（M05 §12.3，见 retriever_helpers.go sampleDriftAnchor）
	hr.sampleDriftAnchor(taskType, query, queryF32, merged)

	return merged, nil
}
