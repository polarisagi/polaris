package agentctx

import (
	"context"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
)

// perceiveEpisodicK Perceive 的情景召回条数。回合内 Plan 复用 Perceive 的情景结果、不再自查
// （原 Plan K=5、Perceive K=3），因此取二者折中的 4：Plan 只损失 1 条低相关尾部，
// Perceive 多 1 条；条数上限之外还有 recall.max_tokens 兜底，多取的代价有界。
const perceiveEpisodicK = 4

// planEpisodicK Perceive 被跳过（寒暄快路/短确认精简）或召回被放弃时 Plan 补查的条数，
// 沿用 Plan 原值。
const planEpisodicK = 5

// recallWant 一个阶段"想要"的召回。
type recallWant struct {
	episodicQuery string // 空 = 不要情景记忆
	episodicK     int
	goal          string // 空 = 不要 反思/L2/RAG
	withProfile   bool
}

// turnRecallText 回合内召回的统一入口（ADR-0105 决策四）：
// 已被本回合较早阶段覆盖的来源直接复用，只对缺口发起检索，合并后按总预算渲染。
//
// 并发：sCtx 只在本函数（主路径）里持锁读写；recall goroutine 只拿调用前捕获的值。
// 检索因预算到期被放弃时不置"已覆盖"标记，缓存原样保留，后续阶段会补查。
func turnRecallText(
	ctx context.Context, memory protocol.MemoryFacade, cognitive fsm.CognitiveSearcher,
	sCtx *fsm.StateContext, want recallWant, phase string) (string, error) {

	projectID := scopeProjectID(sCtx)
	sCtx.Mu.RLock()
	cache := sCtx.TurnRecall.Clone()
	history := historyBlob(sCtx.ConversationHistory)
	knowledge := sCtx.KnowledgeSearcher
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
			return cache.Text, nil // 全部来源已覆盖：零检索、零 embedding
		}
		return "", nil
	}

	fetched, err := recallWithin(ctx, memory, cognitive, spec)
	if err != nil {
		// 超时降级为"用已有的"：未置覆盖标记，后续阶段会补查。
		return cache.Text, degradeOnRecallTimeout(phase, err)
	}
	for k := range fetched.Items {
		cache.Items[k] = append(cache.Items[k], fetched.Items[k]...)
	}
	cache.EpisodicDone = cache.EpisodicDone || spec.episodic
	cache.GoalDone = cache.GoalDone || spec.goal != ""
	cache.ProfileDone = cache.ProfileDone || spec.withProfile
	cache.Text = renderRecall(cache, history, th.RecallMaxTokens)

	sCtx.Mu.Lock()
	sCtx.TurnRecall = cache
	sCtx.Mu.Unlock()
	return cache.Text, nil
}
