package agentctx

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

// 情景召回按相关度检索（ADR-0105 决策十 WP11）：FTS 选 ID → 项目隔离 → 取正文 → 污点再判。

func epEvent(id string, at time.Time, taint types.TaintLevel, body string) types.ScoredEvent {
	return types.ScoredEvent{Score: 1, Event: &types.Event{ID: id, Type: "t", Payload: []byte(body), CreatedAt: at, TaintLevel: taint}}
}

func epMem(projects map[string]string, evs ...types.ScoredEvent) *countingMem {
	m := newCountingMem()
	m.events = evs
	m.eventProjects = projects
	return m
}

func episodicLines(text string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, "- ") {
			out = append(out, l)
		}
	}
	return out
}

func TestEpisodicRecall_RanksByFTSRelevance(t *testing.T) {
	day := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	mem := epMem(map[string]string{"e1": types.DefaultProjectID, "e2": types.DefaultProjectID, "e3": types.DefaultProjectID},
		epEvent("e1", day, types.TaintNone, "中等相关"),
		epEvent("e2", day, types.TaintNone, "最不相关"),
		epEvent("e3", day, types.TaintNone, "最相关"))
	// 存储返回序（e1,e2,e3）与相关度序无关：渲染必须按 FTS 的 BM25 秩。
	cog := &episodicCog{hits: []fsm.CogResult{{DocID: "e3", Score: 9}, {DocID: "e1", Score: 5}, {DocID: "e2", Score: 1}}}
	sCtx := newTurnCtx("部署服务", "")

	text, _, err := turnRecallText(context.Background(), mem, cog, sCtx, recallWant{episodicQuery: "部署服务", episodicK: 4}, "test")
	require.NoError(t, err)
	require.Equal(t, []string{"- [2026-09-01] t: 最相关", "- [2026-09-01] t: 中等相关", "- [2026-09-01] t: 最不相关"}, episodicLines(text))
	require.Equal(t, "部署服务", cog.lastQuery, "以本轮查询词走 FTS")
	require.Equal(t, 4*ftsOverfetch, cog.lastK, "放大取回量以便项目过滤后仍能凑满 K")
	require.Equal(t, []string{"e3", "e1", "e2"}, mem.lastFetchIDs(), "按 FTS 秩取正文，不再整句子串匹配")
}

func (m *countingMem) lastFetchIDs() []string {
	return m.episodic.queries[len(m.episodic.queries)-1].IDs
}

func TestEpisodicRecall_KCapsAndMinScore(t *testing.T) {
	day := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	proj := map[string]string{"a": types.DefaultProjectID, "b": types.DefaultProjectID, "c": types.DefaultProjectID, "d": types.DefaultProjectID}
	mem := epMem(proj,
		epEvent("a", day, types.TaintNone, "甲"), epEvent("b", day, types.TaintNone, "乙"),
		epEvent("c", day, types.TaintNone, "丙"), epEvent("d", day, types.TaintNone, "丁"))
	cog := &episodicCog{hits: []fsm.CogResult{{DocID: "a", Score: 9}, {DocID: "b", Score: 5}, {DocID: "c", Score: 1}, {DocID: "d", Score: 0.5}}}
	sCtx := newTurnCtx("x", "")

	text, _, err := turnRecallText(context.Background(), mem, cog, sCtx, recallWant{episodicQuery: "x", episodicK: 2}, "test")
	require.NoError(t, err)
	require.Len(t, episodicLines(text), 2, "条数受 K 约束")

	// 绝对下限：低于 recall.min_score 的命中在取正文之前就丢弃。
	withThresholds(t, func(th *config.Thresholds) { th.M4Kernel.RecallMinScore = 2 })
	mem2, sCtx2 := epMem(proj, mem.events...), newTurnCtx("x", "")
	text, _, err = turnRecallText(context.Background(), mem2, cog, sCtx2, recallWant{episodicQuery: "x", episodicK: 4}, "test")
	require.NoError(t, err)
	require.Len(t, episodicLines(text), 2)
	require.NotContains(t, text, "丙")
	require.Equal(t, []string{"a", "b"}, mem2.lastFetchIDs())
}

func TestEpisodicRecall_ProjectIsolationAndNonEpisodicHits(t *testing.T) {
	day := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	mem := epMem(map[string]string{"mine": "prj_me", "theirs": "prj_other"},
		epEvent("mine", day, types.TaintNone, "本项目情景"),
		epEvent("theirs", day, types.TaintNone, "他项目机密情景"))
	cog := &episodicCog{hits: []fsm.CogResult{
		{DocID: "theirs", Score: 9},
		{DocID: "sement_Tool_go", Score: 8}, // 语义实体：归 L2 来源，情景来源不得重复放行
		{DocID: "her_task1", Score: 7},      // 反思洞察：归反思来源
		{DocID: "mine", Score: 6},
	}}
	sCtx := newTurnCtx("x", "")
	sCtx.ProjectID = "prj_me"

	text, _, err := turnRecallText(context.Background(), mem, cog, sCtx, recallWant{episodicQuery: "x", episodicK: 4}, "test")
	require.NoError(t, err)
	require.Contains(t, text, "本项目情景")
	require.NotContains(t, text, "他项目机密情景")
	require.Equal(t, []string{"mine"}, mem.lastFetchIDs(), "他项目与非情景命中在取正文前就被剔除")
	require.Equal(t, "prj_me", mem.episodic.queries[0].ProjectID, "取正文时再带项目过滤")
}

func TestEpisodicRecall_TaintFailClosed(t *testing.T) {
	day := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	mem := epMem(map[string]string{"lo": types.DefaultProjectID, "hi": types.DefaultProjectID},
		epEvent("lo", day, types.TaintLow, "低污点情景"),
		epEvent("hi", day, types.TaintHigh, "高污点情景"))
	cog := &episodicCog{hits: []fsm.CogResult{{DocID: "hi", Score: 9}, {DocID: "lo", Score: 5}}}
	sCtx := newTurnCtx("x", "")
	sCtx.RawIntentTS = taint.NewTaintedString("x", taint.TaintSource{OriginTaintLevel: types.TaintLow}, "test")

	// countingMem 不按污点过滤（模拟不遵守 MaxTaintLevel 的存储实现）：召回侧必须自己再判一次。
	text, tl, err := turnRecallText(context.Background(), mem, cog, sCtx, recallWant{episodicQuery: "x", episodicK: 4}, "test")
	require.NoError(t, err)
	require.Contains(t, text, "低污点情景")
	require.NotContains(t, text, "高污点情景")
	require.Equal(t, types.TaintLow, tl)
}

func TestEpisodicRecall_Degrades(t *testing.T) {
	day := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	newMem := func() *countingMem {
		return epMem(map[string]string{"e1": types.DefaultProjectID}, epEvent("e1", day, types.TaintNone, "情景"))
	}
	want := recallWant{episodicQuery: "x", episodicK: 4}

	t.Run("FTS 失败：无情景、不报错、仍标记已覆盖", func(t *testing.T) {
		mem, sCtx := newMem(), newTurnCtx("x", "")
		text, _, err := turnRecallText(context.Background(), mem, &episodicCog{err: errors.New("surreal down")}, sCtx, want, "test")
		require.NoError(t, err)
		require.Empty(t, text)
		require.EqualValues(t, 0, mem.episodicCalls.Load())
		require.True(t, sCtx.TurnRecall.EpisodicDone)
	})
	t.Run("Tier0 无 cognitive：情景来源为空，不碰存储", func(t *testing.T) {
		mem, sCtx := newMem(), newTurnCtx("x", "")
		text, _, err := turnRecallText(context.Background(), mem, nil, sCtx, want, "test")
		require.NoError(t, err)
		require.Empty(t, text)
		require.EqualValues(t, 0, mem.episodicCalls.Load())
	})
	t.Run("cognitive 不支持情景检索：同样降级为空", func(t *testing.T) {
		mem, sCtx := newMem(), newTurnCtx("x", "")
		text, _, err := turnRecallText(context.Background(), mem, scoredCog{}, sCtx, want, "test")
		require.NoError(t, err)
		require.Empty(t, text)
		require.EqualValues(t, 0, mem.episodicCalls.Load())
	})
	t.Run("无命中：不取正文", func(t *testing.T) {
		mem, sCtx := newMem(), newTurnCtx("x", "")
		_, _, err := turnRecallText(context.Background(), mem, &episodicCog{}, sCtx, want, "test")
		require.NoError(t, err)
		require.EqualValues(t, 0, mem.episodicCalls.Load())
	})
}

// 画像只在 L0 稳定核：召回不再读取画像，也不再渲染画像段（ADR-0105 决策十 WP11）。
func TestProfileNotRecalled(t *testing.T) {
	mem := newCountingMem()
	mem.profile = &types.UserProfile{StableFacts: map[string]any{"role": "架构师"}}
	sCtx := newTurnCtx("查询", "")
	msgs, err := BuildPerceiveContext(context.Background(), mem, sCtx, mem.cog())
	require.NoError(t, err)
	require.EqualValues(t, 0, mem.profileCalls.Load())
	require.NotContains(t, joinContents(msgs), "架构师")
	require.NotContains(t, joinContents(msgs), "User Profile")
}
