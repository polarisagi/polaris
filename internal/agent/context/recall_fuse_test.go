package agentctx

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

// withThresholds 临时替换全局阈值表，结束后还原（召回读 config.CurrentThresholds()）。
func withThresholds(t *testing.T, mutate func(*config.Thresholds)) {
	t.Helper()
	prev := config.Get()
	cfg := &config.Config{Thresholds: config.DefaultThresholds()}
	mutate(&cfg.Thresholds)
	config.Update(cfg)
	t.Cleanup(func() { config.Update(prev) })
}

// withSurprise 把进程级 SurpriseIndex 固定为 v，结束后还原。
func withSurprise(t *testing.T, v float64) {
	t.Helper()
	prev := metrics.GlobalSurpriseIndex().Current()
	metrics.GlobalSurpriseIndex().SetLastValue(v)
	t.Cleanup(func() { metrics.GlobalSurpriseIndex().SetLastValue(prev) })
}

func ranked(kind fsm.RecallKind, r *fsm.TurnRecall, texts ...string) {
	for i, s := range texts {
		r.Items[kind] = append(r.Items[kind], withMeta(newRecallItem("", s, 400), types.TaintNone, float64(len(texts)-i)))
	}
}

func keys(f []fusedItem) []string {
	out := make([]string, len(f))
	for i, it := range f {
		out[i] = it.item.Key
	}
	return out
}

func equalWeights() recallWeights {
	return recallWeights{reflection: 1, episodic: 1, semantic: 1, rag: 1}
}

// RRF 只用秩：同秩时权重高的来源在前；权重只在同秩/邻秩间起作用，不让低秩条目越过高秩多名。
func TestFuseRecall_RRFWeightsAndRanks(t *testing.T) {
	r := &fsm.TurnRecall{}
	ranked(fsm.RecallReflection, r, "反思一", "反思二")
	ranked(fsm.RecallEpisodic, r, "情景一", "情景二", "情景三")
	ranked(fsm.RecallRAG, r, "知识一")

	got := keys(fuseRecall(r, "", testWeights()))
	// 1.2/61 > 1.1/61 > 1.1/62 > 1.2/62(反思二=0.019355) 需逐项验算：
	// 反思一 .019672；情景一 .018033；反思二 .019355；情景二 .017742；情景三 .017460；知识一 .016393
	require.Equal(t, []string{"反思一", "反思二", "情景一", "情景二", "情景三", "知识一"}, got)

	// 把 RAG 权重抬到 3：知识一(3/61=.04918) 压过所有，说明权重确实作用在 RRF 分上，且来源不再是硬优先级。
	w := testWeights()
	w.rag = 3
	require.Equal(t, "知识一", keys(fuseRecall(r, "", w))[0])
}

// 同正文被多个来源独立命中：分数累加（比只在单来源排第 1 的条目更靠前），且只保留一条。
func TestFuseRecall_CrossSourceAgreementAccumulates(t *testing.T) {
	r := &fsm.TurnRecall{}
	ranked(fsm.RecallReflection, r, "只在反思里第一", "两路都命中的条目")
	ranked(fsm.RecallRAG, r, "只在知识里第一", "两路都命中的条目")

	f := fuseRecall(r, "", equalWeights())
	require.Equal(t, "两路都命中的条目", f[0].item.Key, "2/62 > 1/61")
	require.Len(t, f, 3)
	require.InDelta(t, 2.0/62.0, f[0].score, 1e-12)
	cnt := 0
	for _, it := range f {
		if it.item.Key == "两路都命中的条目" {
			cnt++
		}
	}
	require.Equal(t, 1, cnt)
}

func TestFuseRecall_ZeroWeightDisablesSource(t *testing.T) {
	r := &fsm.TurnRecall{}
	ranked(fsm.RecallReflection, r, "反思")
	ranked(fsm.RecallRAG, r, "知识")
	w := equalWeights()
	w.rag = 0
	require.Equal(t, []string{"反思"}, keys(fuseRecall(r, "", w)))
}

// 被历史包含的条目先剔除再排秩：它不占来源内的名次。
func TestFuseRecall_HistoryDropBeforeRank(t *testing.T) {
	r := &fsm.TurnRecall{}
	ranked(fsm.RecallEpisodic, r, "这条已经在对话历史里出现过了", "历史里没有的独特条目")
	f := fuseRecall(r, historyBlob([]types.Message{{Role: "user", Content: "这条已经在对话历史里出现过了"}}), equalWeights())
	require.Len(t, f, 1)
	require.InDelta(t, 1.0/61.0, f[0].score, 1e-12, "剔除后幸存者升为第 1 名")
}

func TestFuseRecall_DeterministicAndNilSafe(t *testing.T) {
	build := func() []string {
		r := &fsm.TurnRecall{}
		ranked(fsm.RecallEpisodic, r, "甲", "乙", "丙")
		ranked(fsm.RecallSemantic, r, "丁", "戊")
		ranked(fsm.RecallRAG, r, "己", "甲")
		return keys(fuseRecall(r, "", testWeights()))
	}
	first := build()
	for i := 0; i < 50; i++ {
		require.Equal(t, first, build())
	}
	require.Empty(t, fuseRecall(nil, "", testWeights()))
}

// 预算按融合秩装入（而非按来源硬优先级）：RAG 权重够高时，紧预算下留 RAG、挤掉反思。
func TestPackRecall_BudgetFollowsFusedRank(t *testing.T) {
	r := &fsm.TurnRecall{}
	ranked(fsm.RecallReflection, r, "refl "+strings.Repeat("思", 60))
	ranked(fsm.RecallRAG, r, "rag "+strings.Repeat("知", 60))
	w := equalWeights()
	w.rag = 5

	text, _ := packRecall(fuseRecall(r, "", w), nil, "", 100)
	require.Contains(t, text, "rag ")
	require.NotContains(t, text, "refl ")
	require.LessOrEqual(t, estimateTokens(text), 100)
}

func TestPackRecall_TaintIsMaxOfIncluded(t *testing.T) {
	r := &fsm.TurnRecall{}
	r.Items[fsm.RecallEpisodic] = []fsm.RecallItem{
		withMeta(newRecallItem("", "低污点的一条", 400), types.TaintLow, 1),
		withMeta(newRecallItem("", "高污点的一条", 400), types.TaintHigh, 1),
	}
	_, tl := packRecall(fuseRecall(r, "", testWeights()), nil, "", 10000)
	require.Equal(t, types.TaintHigh, tl)

	// 高污点那条没装下时，整段不得被它抬级。
	_, tl = packRecall(fuseRecall(r, "", testWeights())[:1], nil, "", 10000)
	require.Equal(t, types.TaintLow, tl)

	_, tl = packRecall(nil, nil, "", 10000)
	require.Equal(t, types.TaintNone, tl)
}

// 画像不参与融合：只在融合条目之后、用剩余预算装入；与融合条目/历史重复的画像项丢弃。
func TestPackRecall_ProfileFillsLeftoverOnly(t *testing.T) {
	r := &fsm.TurnRecall{}
	ranked(fsm.RecallEpisodic, r, "情景事件内容")
	r.Items[fsm.RecallProfile] = []fsm.RecallItem{
		newRecallItem("", "role: 架构师", 400),
		newRecallItem("", "情景事件内容", 400), // 与融合条目同正文
		newRecallItem("", "lang: Go", 400),
	}
	text, _ := packRecall(fuseRecall(r, "", testWeights()), r.Items[fsm.RecallProfile], "", 10000)
	require.Less(t, strings.Index(text, "Relevant Historical"), strings.Index(text, "## User Profile"))
	require.Equal(t, 1, strings.Count(text, "情景事件内容"))
	require.Contains(t, text, "- role: 架构师")
	require.Contains(t, text, "- lang: Go")

	// 预算刚够融合条目时画像整体不出现（也不留孤立标题）。
	tight, _ := packRecall(fuseRecall(r, "", testWeights()), r.Items[fsm.RecallProfile], "", estimateTokens("Relevant Historical Episodic Memories:\n- 情景事件内容\n")+2)
	require.NotContains(t, tight, "User Profile")
}

// ---- 相关度门 ----

type fakeReranker struct {
	mu    sync.Mutex
	calls int
	query string
	docs  []string
	probs []float64
	err   error
	block chan struct{}
}

func (f *fakeReranker) RelevanceProbs(ctx context.Context, query string, docs []string) ([]float64, error) {
	f.mu.Lock()
	f.calls++
	f.query, f.docs = query, append([]string(nil), docs...)
	f.mu.Unlock()
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
		}
	}
	return f.probs, f.err
}

func gateFixture() []fusedItem {
	r := &fsm.TurnRecall{}
	ranked(fsm.RecallEpisodic, r, "甲条目", "乙条目", "丙条目", "丁条目")
	return fuseRecall(r, "", equalWeights())
}

func TestRerankGate_DropsBelowMinProbAndUnverifiedTail(t *testing.T) {
	f := gateFixture()
	rr := &fakeReranker{probs: []float64{0.9, 0.49, 0.5}} // topN=3：丁不进重排
	out := applyRerankGate(context.Background(), rerankGate{reranker: rr, topN: 3, minProb: 0.5, query: "查询"}, f)

	require.Equal(t, []string{"甲条目", "丙条目"}, keys(out), "0.49 被丢；恰等于阈值(0.5)保留；topN 之外的未验证尾部不入选")
	require.Equal(t, "查询", rr.query)
	require.Equal(t, []string{"甲条目", "乙条目", "丙条目"}, rr.docs, "送入重排的是融合序前 N 条的正文（不含日期/类型前缀）")
}

func TestRerankGate_FailureModesSkipGate(t *testing.T) {
	f := gateFixture()
	all := keys(f)
	g := func(rr *fakeReranker) rerankGate {
		return rerankGate{reranker: rr, topN: 4, minProb: 0.5, query: "q"}
	}
	t.Run("重排报错", func(t *testing.T) {
		out := applyRerankGate(context.Background(), g(&fakeReranker{err: errors.New("boom")}), f)
		require.Equal(t, all, keys(out))
	})
	t.Run("返回条数不符", func(t *testing.T) {
		out := applyRerankGate(context.Background(), g(&fakeReranker{probs: []float64{0.9}}), f)
		require.Equal(t, all, keys(out))
	})
	t.Run("概率非法", func(t *testing.T) {
		for _, bad := range []float64{math.NaN(), -0.1, 1.5, math.Inf(1)} {
			out := applyRerankGate(context.Background(), g(&fakeReranker{probs: []float64{0.9, bad, 0.9, 0.9}}), f)
			require.Equal(t, all, keys(out), "非法概率 %v 说明重排器不可信，整个门跳过", bad)
		}
	})
	t.Run("超时", func(t *testing.T) {
		rr := &fakeReranker{block: make(chan struct{}), probs: []float64{0, 0, 0, 0}}
		defer close(rr.block)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		start := time.Now()
		out := applyRerankGate(ctx, g(rr), f)
		require.Less(t, time.Since(start), time.Second, "必须遵守召回时间预算")
		require.Equal(t, all, keys(out), "超时跳过此门，条目原样保留")
	})
}

func TestRerankGate_InactiveWhenUnavailable(t *testing.T) {
	f := gateFixture()
	all := keys(f)
	rr := &fakeReranker{probs: []float64{0, 0, 0, 0}}
	require.Equal(t, all, keys(applyRerankGate(context.Background(), rerankGate{topN: 4, minProb: 0.5, query: "q"}, f)), "无重排器")
	require.Equal(t, all, keys(applyRerankGate(context.Background(), rerankGate{reranker: rr, topN: 0, minProb: 0.5, query: "q"}, f)), "topN=0 关闭")
	require.Equal(t, all, keys(applyRerankGate(context.Background(), rerankGate{reranker: rr, topN: 4, minProb: 0.5}, f)), "无查询词")
	require.Zero(t, rr.calls)
	require.Empty(t, applyRerankGate(context.Background(), rerankGate{reranker: rr, topN: 4, query: "q"}, nil))
}

// ---- 端到端（经 turnRecallText）----

func TestTurnRecall_RerankGateEndToEnd(t *testing.T) {
	mem := newCountingMem()
	mem.events = []types.ScoredEvent{
		scoredEvent("t", "相关的情景事件", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)),
		scoredEvent("t", "不相关的情景事件", time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)),
	}
	sCtx := newTurnCtx("查询", "")
	sCtx.RecallReranker = &fakeReranker{probs: []float64{0.8, 0.1}}

	text, _, err := turnRecallText(context.Background(), mem, nil, sCtx, recallWant{episodicQuery: "查询", episodicK: 4}, "test")
	require.NoError(t, err)
	require.Contains(t, text, "相关的情景事件")
	require.NotContains(t, text, "不相关的情景事件")
}

func TestTurnRecall_NoRerankerMeansNoGate(t *testing.T) {
	mem := newCountingMem()
	mem.events = []types.ScoredEvent{scoredEvent("t", "情景甲", time.Now()), scoredEvent("t", "情景乙", time.Now())}
	sCtx := newTurnCtx("查询", "")
	text, _, err := turnRecallText(context.Background(), mem, nil, sCtx, recallWant{episodicQuery: "查询", episodicK: 4}, "test")
	require.NoError(t, err)
	require.Contains(t, text, "情景甲")
	require.Contains(t, text, "情景乙")
}

// ---- Surprise 门（自原 Assembler 迁入）----

type recordingKB struct {
	mu    sync.Mutex
	calls []int
	hits  []fsm.KnowledgeResult
}

func (k *recordingKB) SearchRAG(_ context.Context, _ string, topK int) ([]fsm.KnowledgeResult, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.calls = append(k.calls, topK)
	return k.hits, nil
}

func TestTurnRecall_SurpriseGatesRAGDepth(t *testing.T) {
	cases := []struct {
		name     string
		surprise float64
		wantTopK []int
	}{
		{"平稳：不查知识", 0.1, nil},
		{"常态：深度 1", 0.5, []int{ragTopKBase}},
		{"恰等于阈值 0.6：仍是深度 1（仅大于才加倍）", 0.6, []int{ragTopKBase}},
		{"高惊奇：深度加倍", 0.9, []int{ragTopKBase * 2}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withSurprise(t, c.surprise)
			kb := &recordingKB{hits: []fsm.KnowledgeResult{{Content: "知识片段", Source: "doc", Score: 1}}}
			sCtx := newTurnCtx("x", "部署")
			sCtx.KnowledgeSearcher = kb
			text, _, err := turnRecallText(context.Background(), newCountingMem(), nil, sCtx, recallWant{goal: "部署"}, "test")
			require.NoError(t, err)
			require.Equal(t, c.wantTopK, kb.calls)
			if c.wantTopK == nil {
				require.NotContains(t, text, "知识片段")
				require.True(t, sCtx.TurnRecall.GoalDone, "已决定不查也算覆盖，回合内不重复判定")
			} else {
				require.Contains(t, text, "知识片段")
			}
		})
	}
}

// ---- MaxTaint 过滤（自原 Assembler 迁入）----

type taintedCog struct{ hits []fsm.CogResult }

func (c taintedCog) FTSSearch(context.Context, string, int) ([]fsm.CogResult, error) {
	return c.hits, nil
}

func TestTurnRecall_MaxTaintFiltersEverySource(t *testing.T) {
	mkEvent := func(text string, lvl types.TaintLevel) types.ScoredEvent {
		return types.ScoredEvent{Score: 1, Event: &types.Event{Type: "t", Payload: []byte(text), CreatedAt: time.Now(), TaintLevel: lvl}}
	}
	setup := func(origin types.TaintLevel) (*countingMem, *fsm.StateContext, *recordingKB, taintedCog) {
		mem := newCountingMem()
		mem.events = []types.ScoredEvent{mkEvent("低污点情景", types.TaintLow), mkEvent("高污点情景", types.TaintHigh)}
		sCtx := newTurnCtx("x", "部署")
		sCtx.RawIntentTS = taint.NewTaintedString("x", taint.TaintSource{OriginTaintLevel: origin}, "test")
		kb := &recordingKB{hits: []fsm.KnowledgeResult{
			{Content: "低污点知识", Score: 2, Taint: types.TaintLow},
			{Content: "高污点知识", Score: 1, Taint: types.TaintHigh},
		}}
		sCtx.KnowledgeSearcher = kb
		cog := taintedCog{hits: []fsm.CogResult{
			{DocID: "a", Snippet: "低污点实体", Score: 2, Taint: types.TaintLow},
			{DocID: "b", Snippet: "高污点实体", Score: 1, Taint: types.TaintHigh},
		}}
		return mem, sCtx, kb, cog
	}
	want := recallWant{episodicQuery: "x", episodicK: 5, goal: "部署"}

	t.Run("意图来源污点 Low：Medium 以上一律不得进入召回", func(t *testing.T) {
		mem, sCtx, _, cog := setup(types.TaintLow)
		text, tl, err := turnRecallText(context.Background(), mem, cog, sCtx, want, "test")
		require.NoError(t, err)
		for _, s := range []string{"低污点情景", "低污点知识", "低污点实体"} {
			require.Contains(t, text, s)
		}
		for _, s := range []string{"高污点情景", "高污点知识", "高污点实体"} {
			require.NotContains(t, text, s, "fail-closed：超过上限的命中必须被丢弃")
		}
		require.Equal(t, types.TaintLow, tl)
	})
	t.Run("意图来源污点 High（生产常态）：全部放行，整段定级 High", func(t *testing.T) {
		mem, sCtx, _, cog := setup(types.TaintHigh)
		text, tl, err := turnRecallText(context.Background(), mem, cog, sCtx, want, "test")
		require.NoError(t, err)
		require.Contains(t, text, "高污点情景")
		require.Contains(t, text, "高污点知识")
		require.Contains(t, text, "高污点实体")
		require.Equal(t, types.TaintHigh, tl, "召回段污点取已装入条目的最大值，不得被压成 Medium")
	})
	t.Run("意图未标注（TaintNone）按 High 处理", func(t *testing.T) {
		mem, sCtx, _, cog := setup(types.TaintNone)
		text, _, err := turnRecallText(context.Background(), mem, cog, sCtx, want, "test")
		require.NoError(t, err)
		require.Contains(t, text, "高污点知识")
	})
}

// ---- RAG 真实分 ----

func TestCollectKnowledge_OrdersByRealScoreAndFiltersWithRatio(t *testing.T) {
	kb := &recordingKB{hits: []fsm.KnowledgeResult{
		{Content: "中", Score: 0.5},
		{Content: "高", Score: 0.9},
		{Content: "低", Score: 0.05},
	}}
	spec := recallSpec{knowledge: kb, goal: "g", knowledgeTopK: 3, maxTaint: types.TaintHigh,
		limits: recallLimits{itemMaxChars: 400, minScoreRatio: 0.2}}
	got := collectKnowledge(context.Background(), spec)
	require.Len(t, got, 2, "0.05 < 0.2×0.9 被相对阈值丢弃")
	require.Equal(t, "高", got[0].Key, "来源内按真实分降序（此前恒 1.0 时只能保持检索序）")
	require.Equal(t, "中", got[1].Key)
	require.Equal(t, []int{3}, kb.calls)
}
