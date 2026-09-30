package agentctx

import (
	"context"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// perceiveEpisodicK Perceive 的情景召回条数。回合内 Plan 复用 Perceive 的情景结果、不再自查
// （原 Plan K=5、Perceive K=3），因此取二者折中的 4：Plan 只损失 1 条低相关尾部，
// Perceive 多 1 条；条数上限之外还有 recall.max_tokens 兜底，多取的代价有界。
const perceiveEpisodicK = 4

// planEpisodicK Perceive 被跳过（寒暄快路/短确认精简）或召回被放弃时 Plan 补查的条数，
// 沿用 Plan 原值。
const planEpisodicK = 5

// ragTopKBase SurpriseIndex 处于常态区间时的 RAG 检索条数；高于 SurpriseHintThreshold 时加倍。
// 自原 Assembler 旁路迁入（其 depth=1/2 对应 topK 5/10，这里取 3/6：召回段受 recall.max_tokens 约束，
// 多取的候选大多装不下）。
const ragTopKBase = 3

// recallWant 一个阶段"想要"的召回。
type recallWant struct {
	episodicQuery string // 空 = 不要情景记忆
	episodicK     int
	goal          string // 空 = 不要 反思/L2/RAG
	withProfile   bool
}

// recallMaxTaint 召回项污点上限：本轮意图的来源污点；未标注（TaintNone）按 TaintHigh。
// 原 Assembler 旁路的 MaxTaint 语义原样迁入——此前现召回对情景硬编码 TaintHigh、对 L2/RAG 不过滤。
func recallMaxTaint(sCtx *fsm.StateContext) types.TaintLevel {
	sCtx.Mu.RLock()
	lvl := sCtx.RawIntentTS.Source.OriginTaintLevel
	sCtx.Mu.RUnlock()
	if lvl == types.TaintNone {
		return types.TaintHigh
	}
	return lvl
}

// ragTopK 按 SurpriseIndex 决定 RAG 检索深度（自原 Assembler 旁路迁入）：
// 低于 recall.rag_min_surprise 不查（环境平稳，无需外部知识补充）；高于 surprise_hint_threshold 加倍深度。
func ragTopK(surprise float64, th config.M4KernelThresholds) int {
	if surprise < th.RecallRAGMinSurprise {
		return 0
	}
	if surprise > th.SurpriseHintThreshold {
		return ragTopKBase * 2
	}
	return ragTopKBase
}

// turnRecallText 回合内召回的统一入口（ADR-0105 决策四/十）：
// 已被本回合较早阶段覆盖的来源直接复用，只对缺口发起检索，合并后 RRF 融合 → （可选）本地重排相关度门
// → 按总预算装入。返回召回段正文与其中已装入条目的最高污点（调用方据此给整段定级）。
//
// 并发：sCtx 只在本函数（主路径）里持锁读写；recall goroutine 只拿调用前捕获的值。
// 检索因预算到期被放弃时不置"已覆盖"标记，缓存原样保留，后续阶段会补查。
func turnRecallText(
	ctx context.Context, memory protocol.MemoryFacade, cognitive fsm.CognitiveSearcher,
	sCtx *fsm.StateContext, want recallWant, phase string) (string, types.TaintLevel, error) {

	projectID := scopeProjectID(sCtx)
	maxTaint := recallMaxTaint(sCtx)
	sCtx.Mu.RLock()
	cache := sCtx.TurnRecall.Clone()
	history := historyBlob(sCtx.ConversationHistory)
	knowledge := sCtx.KnowledgeSearcher
	reranker := sCtx.RecallReranker
	hadCache := sCtx.TurnRecall != nil
	sCtx.Mu.RUnlock()

	th := config.CurrentThresholds().M4Kernel
	spec := recallSpec{
		episodic:      want.episodicQuery != "" && !cache.EpisodicDone,
		episodicQuery: want.episodicQuery,
		episodicK:     want.episodicK,
		withProfile:   want.withProfile && !cache.ProfileDone,
		projectID:     projectID,
		knowledge:     knowledge,
		knowledgeTopK: ragTopK(metrics.GlobalSurpriseIndex().Current(), th),
		maxTaint:      maxTaint,
		limits: recallLimits{
			itemMaxChars:  th.RecallItemMaxChars,
			minScore:      th.RecallMinScore,
			minScoreRatio: th.RecallMinScoreRatio,
		},
	}
	if want.goal != "" && !cache.GoalDone {
		spec.goal = want.goal
	}
	if !spec.episodic && spec.goal == "" && !spec.withProfile {
		if hadCache {
			return cache.Text, cache.Taint, nil // 全部来源已覆盖：零检索、零 embedding
		}
		return "", types.TaintNone, nil
	}

	fetched, err := recallWithin(ctx, memory, cognitive, spec)
	if err != nil {
		// 超时降级为"用已有的"：未置覆盖标记，后续阶段会补查。
		return cache.Text, cache.Taint, degradeOnRecallTimeout(phase, err)
	}
	for k := range fetched.Items {
		cache.Items[k] = append(cache.Items[k], fetched.Items[k]...)
	}
	cache.EpisodicDone = cache.EpisodicDone || spec.episodic
	cache.GoalDone = cache.GoalDone || spec.goal != ""
	cache.ProfileDone = cache.ProfileDone || spec.withProfile

	gateQuery := want.goal
	if gateQuery == "" {
		gateQuery = want.episodicQuery
	}
	fused := fuseRecall(cache, history, recallWeightsOf(th))
	fused = applyRerankGate(ctx, rerankGate{
		reranker: reranker, topN: th.RecallRerankTopN, minProb: th.RecallRerankMinProb, query: gateQuery,
	}, fused)
	cache.Text, cache.Taint = packRecall(fused, cache.Items[fsm.RecallProfile], history, th.RecallMaxTokens)

	sCtx.Mu.Lock()
	sCtx.TurnRecall = cache
	sCtx.Mu.Unlock()
	return cache.Text, cache.Taint, nil
}
