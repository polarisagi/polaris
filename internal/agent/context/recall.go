package agentctx

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// recallLimits 单次召回的渲染/过滤上限，来自 m4_kernel.recall.*（ADR-0105 决策四）。
type recallLimits struct {
	itemMaxChars  int
	minScore      float64
	minScoreRatio float64
}

// recallSpec 一次记忆召回的全部输入，只含调用前从 sCtx 捕获的值：recall 跑在独立
// goroutine 里，预算到期被放弃后可能仍在运行，绝不能再读写 sCtx（否则与主路径数据竞争）。
type recallSpec struct {
	episodic      bool
	episodicQuery string
	episodicK     int
	goal          string // 反思 / L2 语义 / RAG 的查询词；空则三者跳过
	withProfile   bool
	projectID     string
	knowledge     fsm.KnowledgeSearcher
	// knowledgeTopK RAG 检索条数；0 = 本回合不查 RAG（SurpriseIndex 低于 recall.rag_min_surprise）。
	knowledgeTopK int
	// maxTaint 召回项污点上限：高于它的命中一律丢弃（fail-closed）。取自本轮意图的来源污点，
	// 未标注（TaintNone）按 TaintHigh——与原 Assembler 旁路同口径（ADR-0105 决策十）。
	maxTaint types.TaintLevel
	limits   recallLimits
}

// degradeOnRecallTimeout 召回超时按"无召回"降级（召回是增益，不是阶段的前置条件）；
// 其他错误照旧上抛，由 fsm 回退到无记忆 prompt。
func degradeOnRecallTimeout(phase string, err error) error {
	if err == nil {
		return nil
	}
	if apperr.IsCode(err, apperr.CodeTimeout) {
		slog.Warn("agentctx: memory recall exceeded budget, continuing without recalled memory", "phase", phase, "err", err)
		return nil
	}
	return apperr.Wrap(apperr.CodeInternal, phase, err)
}

// recallWithin 在 ctx 截止前等待召回完成，到期返回 CodeTimeout 由调用方降级。
//
// 只给下游传带截止的 ctx 不够：search.Embedder 接口不收 ctx，SyncBatcherAdapter
// 内部固定 Background+30s，截止时间传不下去（2026-09-25 实测 Perceive 因此卡 30s）。
// 故在边界处放弃等待；后台 goroutine 受下游 30s 上限约束必然退出（A-13）。
func recallWithin(ctx context.Context, memory protocol.MemoryFacade, cognitive fsm.CognitiveSearcher, spec recallSpec) (*fsm.TurnRecall, error) {
	type result struct {
		items *fsm.TurnRecall
		err   error
	}
	done := make(chan result, 1)
	concurrent.SafeGo(ctx, "agentctx.recall", func(gctx context.Context) {
		items, err := recall(gctx, memory, cognitive, spec)
		done <- result{items, err}
	})
	select {
	case r := <-done:
		return r.items, r.err
	case <-ctx.Done():
		return nil, apperr.Wrap(apperr.CodeTimeout, "agentctx: memory recall exceeded budget", ctx.Err())
	}
}

// recall 逐来源检索（反思、情景、L2、RAG、画像），只返回渲染好的条目，各来源内按原生分降序。
// 来源间的取舍（RRF 融合）、预算截断与去重留给 fuseRecall/packRecall（它们依赖检索完成后的 L2 历史与总预算）。
func recall(ctx context.Context, memory protocol.MemoryFacade, cognitive fsm.CognitiveSearcher, spec recallSpec) (*fsm.TurnRecall, error) {
	out := &fsm.TurnRecall{}
	if spec.goal != "" {
		out.Items[fsm.RecallReflection] = collectReflections(ctx, memory, spec)
	}
	if spec.episodic {
		items, err := collectEpisodic(ctx, memory, spec)
		if err != nil {
			return nil, err
		}
		out.Items[fsm.RecallEpisodic] = items
	}
	if cognitive != nil && spec.goal != "" {
		out.Items[fsm.RecallSemantic] = collectSemantic(ctx, memory, cognitive, spec)
	}
	if spec.knowledge != nil && spec.goal != "" && spec.knowledgeTopK > 0 {
		out.Items[fsm.RecallRAG] = collectKnowledge(ctx, spec)
	}
	if spec.withProfile {
		out.Items[fsm.RecallProfile] = collectUserProfile(ctx, memory, spec.limits)
	}
	return out, nil
}

// withMeta 给召回条目补上污点与来源原生分。
func withMeta(it fsm.RecallItem, taint types.TaintLevel, score float64) fsm.RecallItem {
	it.Taint = taint
	it.Score = score
	return it
}

// sortByScoreDesc 来源内按原生分降序（稳定：同分保持检索返回序，保证同输入字节一致）。
func sortByScoreDesc(items []fsm.RecallItem) {
	sort.SliceStable(items, func(i, j int) bool { return items[i].Score > items[j].Score })
}

func collectEpisodic(ctx context.Context, memory protocol.MemoryFacade, spec recallSpec) ([]fsm.RecallItem, error) {
	events, err := memory.ListEpisodicEvents(ctx, types.EpisodicQuery{
		Semantic:      spec.episodicQuery,
		ProjectID:     spec.projectID, // 情景记忆按项目隔离（ADR-0097 决策三修订）
		K:             spec.episodicK,
		MaxTaintLevel: spec.maxTaint,
	})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "failed to query episodic memory", err)
	}
	var items []fsm.RecallItem
	for _, e := range events {
		pbEv := e.EventPtr()
		if pbEv == nil {
			continue
		}
		// 底层已按 MaxTaintLevel 过滤；此处再判一次，不依赖各存储实现都遵守该参数（fail-closed）。
		if pbEv.TaintLevel > spec.maxTaint {
			continue
		}
		// 不再输出 payload 原始 JSON：payload 形态不一（工具输出/结构化 JSON/纯文本，最长 8KB），
		// 取描述性字段或压成单行后按单条上限截断；时间戳只要日期，秒级时间在提示词里无信息量。
		items = append(items, withMeta(newRecallItem(datePrefix(pbEv.CreatedAt)+string(pbEv.Type)+": ",
			episodicSummary(pbEv.Payload), spec.limits.itemMaxChars), pbEv.TaintLevel, e.Score))
	}
	sortByScoreDesc(items)
	return items, nil
}

func collectReflections(ctx context.Context, memory protocol.MemoryFacade, spec recallSpec) []fsm.RecallItem {
	reflections, err := memory.ListReflections(ctx, types.ReflectionQuery{Topic: spec.goal, K: 3})
	if err != nil {
		return nil
	}
	items := make([]fsm.RecallItem, 0, len(reflections))
	for i, r := range reflections {
		// 反思无相关度分：保持存储层返回序，用递减序号分占位，使来源内排序语义与其它来源一致。
		items = append(items, withMeta(newRecallItem(datePrefix(r.CreatedAt), r.Strategy+": "+r.Decision, spec.limits.itemMaxChars),
			types.TaintNone, float64(len(reflections)-i)))
	}
	return items
}

func collectSemantic(ctx context.Context, memory protocol.MemoryFacade, cognitive fsm.CognitiveSearcher, spec recallSpec) []fsm.RecallItem {
	hits, err := projectScopedFTS(ctx, memory, cognitive, spec.goal, 5, spec.projectID)
	if err != nil || len(hits) == 0 {
		return nil
	}
	scores := make([]float64, len(hits))
	for i, h := range hits {
		scores[i] = float64(h.Score)
	}
	var items []fsm.RecallItem
	for i, keep := range scoreFilter(scores, spec.limits) {
		// 分数只用于过滤/排序，不再写进提示词（BM25 原始分对模型无意义，还白占 token）。
		if !keep || hits[i].Taint > spec.maxTaint {
			continue
		}
		items = append(items, withMeta(newRecallItem("", hits[i].Snippet, spec.limits.itemMaxChars), hits[i].Taint, scores[i]))
	}
	sortByScoreDesc(items)
	return items
}

func collectKnowledge(ctx context.Context, spec recallSpec) []fsm.RecallItem {
	hits, err := spec.knowledge.SearchRAG(ctx, spec.goal, spec.knowledgeTopK)
	if err != nil || len(hits) == 0 {
		return nil
	}
	scores := make([]float64, len(hits))
	for i, h := range hits {
		scores[i] = float64(h.Score)
	}
	var items []fsm.RecallItem
	for i, keep := range scoreFilter(scores, spec.limits) {
		if !keep || hits[i].Taint > spec.maxTaint {
			continue
		}
		// 来源 URI 可能很长，单独限长后再作前缀，避免它吃光单条上限而正文只剩一截。
		prefix := ""
		if src := truncateRunes(collapseSpace(hits[i].Source), sourceMaxChars); src != "" {
			prefix = src + ": "
		}
		items = append(items, withMeta(newRecallItem(prefix, hits[i].Content, spec.limits.itemMaxChars), hits[i].Taint, scores[i]))
	}
	sortByScoreDesc(items)
	return items
}

// collectUserProfile 消费 default 用户画像（P0-2）。StableFacts/BehavioralPatterns 是 map：
// 遍历顺序随机会使召回段逐回合字节不同（破坏 L4 内确定性），且此前只输出 value、丢了 key。
func collectUserProfile(ctx context.Context, memory protocol.MemoryFacade, limits recallLimits) []fsm.RecallItem {
	p, err := memory.GetUserProfile(ctx, "default")
	if err != nil || p == nil {
		return nil
	}
	var items []fsm.RecallItem
	for _, m := range []map[string]any{p.StableFacts, p.BehavioralPatterns} {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			items = append(items, newRecallItem("", k+": "+fmt.Sprint(m[k]), limits.itemMaxChars))
		}
	}
	return items
}

// datePrefix 日期前缀（UTC，确定性）；零值时间不输出（0001-01-01 只是噪声）。
func datePrefix(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return "[" + t.UTC().Format("2006-01-02") + "] "
}
