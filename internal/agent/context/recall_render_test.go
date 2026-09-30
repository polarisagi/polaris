package agentctx

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestEpisodicSummary(t *testing.T) {
	cases := []struct{ name, payload, want string }{
		{"取 summary 字段", `{"tool":"x","summary":"做了 A"}`, "做了 A"},
		{"summary 优先于 content", `{"content":"c","summary":"s"}`, "s"},
		{"无描述字段压成单行 JSON", "{\n  \"tool\": \"shell\",\n  \"status\": \"done\"\n}", `{"tool":"shell","status":"done"}`},
		{"JSON 数组压成单行", "[1,\n 2]", "[1,2]"},
		{"纯文本原样", "  plain text  ", "plain text"},
		{"空 payload", "  ", ""},
		{"损坏 JSON 退化为原文", `{"a":`, `{"a":`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, episodicSummary([]byte(c.payload)))
		})
	}
}

func TestNewRecallItem_SingleLineAndBounded(t *testing.T) {
	it := newRecallItem("[2026-09-01] t: ", "第一行\n\n第二行\t第三行"+strings.Repeat("字", 1000), 50)
	require.NotContains(t, it.Text, "\n")
	require.LessOrEqual(t, utf8.RuneCountInString(strings.TrimPrefix(it.Text, "- ")), 50)
	require.True(t, strings.HasSuffix(it.Text, "…"))
	require.True(t, utf8.ValidString(it.Text), "不得切断多字节字符")
	require.True(t, strings.HasPrefix(it.Text, "- [2026-09-01] t: 第一行 第二行 第三行"))
	require.True(t, strings.HasPrefix(it.Text[2:], "[2026-09-01] t: "+it.Key[:3]))

	short := newRecallItem("", "短", 400)
	require.Equal(t, "- 短", short.Text)
	require.Equal(t, "短", short.Key)

	// 前缀比上限还长：正文至少保留 1 个字符，不 panic。
	require.NotPanics(t, func() { newRecallItem(strings.Repeat("p", 100), "body", 10) })
}

func TestNewRecallItem_InvalidUTF8(t *testing.T) {
	it := newRecallItem("", "ok\xff\xfebad", 100)
	require.True(t, utf8.ValidString(it.Text))
}

func TestEstimateTokens(t *testing.T) {
	require.Equal(t, 0, estimateTokens(""))
	require.Equal(t, 4, estimateTokens("你好世界"), "CJK 按 1 token/字")
	require.Equal(t, 4, estimateTokens("hello world"), "11 字节 /3 向上取整")
	require.Greater(t, estimateTokens("你好世界"), len("你好世界")/4, "不得像 len/4 那样低估中文")
}

func items(kind fsm.RecallKind, r *fsm.TurnRecall, texts ...string) {
	for _, s := range texts {
		r.Items[kind] = append(r.Items[kind], newRecallItem("", s, 400))
	}
}

func TestRenderRecall_PriorityAndBudget(t *testing.T) {
	r := &fsm.TurnRecall{}
	items(fsm.RecallProfile, r, "role: "+strings.Repeat("画像", 40))
	items(fsm.RecallRAG, r, "rag "+strings.Repeat("知", 60))
	items(fsm.RecallSemantic, r, "l2 "+strings.Repeat("语", 60))
	items(fsm.RecallEpisodic, r, "ep "+strings.Repeat("事", 60))
	items(fsm.RecallReflection, r, "refl "+strings.Repeat("思", 60))

	// 充足预算：五段齐全，且按 反思>情景>L2>RAG>画像 输出。
	full := renderRecall(r, "", 100000)
	order := []string{"Cross-Session Reflections", "Relevant Historical Episodic Memories", "Semantic Memory (L2)", "Knowledge Base (RAG)", "## User Profile"}
	last := -1
	for _, h := range order {
		i := strings.Index(full, h)
		require.Greater(t, i, last, "段顺序应为 %s 之后", h)
		last = i
	}

	// 紧预算：优先级高的先装，低的被挤掉；总量不超预算。
	const budget = 200
	tight := renderRecall(r, "", budget)
	require.LessOrEqual(t, estimateTokens(tight), budget)
	require.Contains(t, tight, "refl ")
	require.NotContains(t, tight, "role: ", "画像优先级最低，应最先被挤掉")

	// 预算为 0 等价于无召回。
	require.Empty(t, renderRecall(r, "", 1))
}

func TestRenderRecall_NoHeaderWhenNothingFits(t *testing.T) {
	r := &fsm.TurnRecall{}
	items(fsm.RecallEpisodic, r, strings.Repeat("长", 300))
	require.Empty(t, renderRecall(r, "", 50), "一条都装不下时不应只输出孤立的段标题")
}

func TestRenderRecall_DedupWithinRecall(t *testing.T) {
	r := &fsm.TurnRecall{}
	items(fsm.RecallReflection, r, "重复的正文内容需要去重")
	items(fsm.RecallEpisodic, r, "重复的正文内容需要去重", "另一条独特内容")
	out := renderRecall(r, "", 10000)
	require.Equal(t, 1, strings.Count(out, "重复的正文内容需要去重"), "跨段同正文只留优先级最高的一条")
	require.Contains(t, out, "另一条独特内容")
}

func TestRenderRecall_HistoryContainment(t *testing.T) {
	r := &fsm.TurnRecall{}
	items(fsm.RecallEpisodic, r, "已经出现在历史里的 一段话", "ok")
	hist := historyBlob([]types.Message{
		{Role: "user", Content: "前文 已经出现在历史里的  \n 一段话 后文"},
		{Role: "system", Content: "system 消息不参与"},
	})
	out := renderRecall(r, hist, 10000)
	require.NotContains(t, out, "已经出现在历史里的")
	require.Contains(t, out, "- ok", "过短条目不按包含判重，避免误杀")
}

func TestRenderRecall_TruncatedItemStillDedups(t *testing.T) {
	long := strings.Repeat("这是一段很长的历史原话", 20)
	r := &fsm.TurnRecall{}
	r.Items[fsm.RecallEpisodic] = []fsm.RecallItem{newRecallItem("", long, 60)} // 截断，Key 以 … 结尾
	hist := historyBlob([]types.Message{{Role: "user", Content: long}})
	require.Empty(t, renderRecall(r, hist, 10000))
}

func TestRenderRecall_Deterministic(t *testing.T) {
	r := &fsm.TurnRecall{}
	items(fsm.RecallEpisodic, r, "甲事件内容", "乙事件内容")
	items(fsm.RecallReflection, r, "丙反思内容")
	require.Equal(t, renderRecall(r, "", 1000), renderRecall(r, "", 1000))
	require.Empty(t, renderRecall(nil, "", 1000))
}
