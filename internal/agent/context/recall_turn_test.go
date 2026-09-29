package agentctx

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

// countingMem 计数 fake：验证 Plan 复用 Perceive 召回时确实不再调用 MemoryFacade 的检索方法。
// 计数用 atomic：召回跑在独立 goroutine 里。
type countingMem struct {
	*mockMemory
	episodicCalls   atomic.Int32
	reflectionCalls atomic.Int32
	profileCalls    atomic.Int32
	reflections     []types.ReflectionEntry
	profile         *types.UserProfile
	events          []types.ScoredEvent
}

func newCountingMem() *countingMem {
	return &countingMem{mockMemory: &mockMemory{
		episodic: &mockEpisodicMem{},
		working:  &mockWorkingMem{immutable: &mockImmutableCore{}},
	}}
}

func (m *countingMem) ListEpisodicEvents(context.Context, types.EpisodicQuery) ([]types.ScoredEvent, error) {
	m.episodicCalls.Add(1)
	return m.events, nil
}

func (m *countingMem) ListReflections(context.Context, types.ReflectionQuery) ([]types.ReflectionEntry, error) {
	m.reflectionCalls.Add(1)
	return m.reflections, nil
}

func (m *countingMem) GetUserProfile(context.Context, string) (*types.UserProfile, error) {
	m.profileCalls.Add(1)
	return m.profile, nil
}

func scoredEvent(typ, payload string, at time.Time) types.ScoredEvent {
	return types.ScoredEvent{Score: 1, Event: &types.Event{Type: types.EventType(typ), Payload: []byte(payload), CreatedAt: at}}
}

func newTurnCtx(intent, goal string) *fsm.StateContext {
	s := &fsm.StateContext{
		TaskID:      "t1",
		RawIntentTS: taint.NewTaintedString(intent, taint.TaintSource{OriginTaintLevel: types.TaintHigh}, "test"),
	}
	if goal != "" {
		s.TaskModel = &fsm.TaskModel{Goal: goal}
	}
	return s
}

// Perceive 已召回情景时，Plan 不再查情景；反思按 Plan 的 Goal 补查一次，且 Plan 看得到 Perceive 的召回。
func TestPlanReusesPerceiveRecall(t *testing.T) {
	mem := newCountingMem()
	mem.events = []types.ScoredEvent{scoredEvent("task_done", `{"summary":"上次迁移用了 goose 工具"}`, time.Date(2026, 9, 1, 13, 45, 10, 0, time.UTC))}
	mem.reflections = []types.ReflectionEntry{{Strategy: "先备份", Decision: "迁移前必须先备份数据库", CreatedAt: time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)}}
	sCtx := newTurnCtx("帮我迁移数据库", "")

	_, err := BuildPerceiveContext(context.Background(), mem, sCtx, nil)
	require.NoError(t, err)
	require.EqualValues(t, 1, mem.episodicCalls.Load(), "Perceive 应查一次情景")
	require.EqualValues(t, 0, mem.reflectionCalls.Load(), "Perceive 不以遗留 Goal 查反思")

	sCtx.TaskModel = &fsm.TaskModel{Goal: "迁移数据库到新版本"}
	msgs, err := BuildPlanContext(context.Background(), mem, sCtx, nil, nil)
	require.NoError(t, err)
	require.EqualValues(t, 1, mem.episodicCalls.Load(), "Plan 复用 Perceive 的情景召回，不得再调 ListEpisodicEvents")
	require.EqualValues(t, 1, mem.reflectionCalls.Load(), "Plan 用已解析 Goal 补查反思")

	all := joinContents(msgs)
	require.Contains(t, all, "上次迁移用了 goose 工具")
	require.Contains(t, all, "迁移前必须先备份数据库")

	// 同回合再次规划（重规划）：所有来源均已覆盖，零检索。
	_, err = BuildPlanContext(context.Background(), mem, sCtx, nil, nil)
	require.NoError(t, err)
	require.EqualValues(t, 1, mem.episodicCalls.Load())
	require.EqualValues(t, 1, mem.reflectionCalls.Load())
	require.EqualValues(t, 1, mem.profileCalls.Load(), "画像也只在 Perceive 取一次")
}

// Perceive 被跳过（无 TurnRecall）且 Goal 非空：Plan 补查一次情景。
func TestPlanFallsBackWhenPerceiveSkipped(t *testing.T) {
	mem := newCountingMem()
	mem.events = []types.ScoredEvent{scoredEvent("task_done", "补查到的情景", time.Now())}
	sCtx := newTurnCtx("好的", "部署服务")

	msgs, err := BuildPlanContext(context.Background(), mem, sCtx, nil, nil)
	require.NoError(t, err)
	require.EqualValues(t, 1, mem.episodicCalls.Load())
	require.Contains(t, joinContents(msgs), "补查到的情景")
	require.NotNil(t, sCtx.TurnRecall)
	require.True(t, sCtx.TurnRecall.EpisodicDone)
}

// 寒暄/短确认（Perceive 精简召回）：Perceive 不查情景，也不标记情景已覆盖。
func TestPerceiveLeanAckDoesNotMarkEpisodicDone(t *testing.T) {
	mem := newCountingMem()
	sCtx := newTurnCtx("好的", "")
	_, err := BuildPerceiveContext(context.Background(), mem, sCtx, nil)
	require.NoError(t, err)
	require.EqualValues(t, 0, mem.episodicCalls.Load())
	require.False(t, sCtx.TurnRecall != nil && sCtx.TurnRecall.EpisodicDone)
}

// Plan 的 Goal 为空时没有可补查的查询词：不发起任何检索。
func TestPlanEmptyGoalDoesNotQuery(t *testing.T) {
	mem := newCountingMem()
	sCtx := newTurnCtx("x", "")
	_, err := BuildPlanContext(context.Background(), mem, sCtx, nil, nil)
	require.NoError(t, err)
	require.EqualValues(t, 0, mem.episodicCalls.Load())
	require.EqualValues(t, 0, mem.reflectionCalls.Load())
}

// 情景不再输出原始 JSON payload 与秒级时间戳。
func TestEpisodicRenderedCompact(t *testing.T) {
	mem := newCountingMem()
	mem.events = []types.ScoredEvent{
		scoredEvent("execution_completed", `{"tool":"shell","args":{"cmd":"ls"},"summary":"列出了 3 个文件"}`, time.Date(2026, 9, 1, 13, 45, 10, 0, time.UTC)),
		scoredEvent("action_done", `{"tool":"shell","status":"done"}`, time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC)),
	}
	sCtx := newTurnCtx("列目录", "")
	msgs, err := BuildPerceiveContext(context.Background(), mem, sCtx, nil)
	require.NoError(t, err)
	all := joinContents(msgs)
	require.Contains(t, all, "- [2026-09-01] execution_completed: 列出了 3 个文件")
	require.Contains(t, all, `- [2026-09-02] action_done: {"tool":"shell","status":"done"}`)
	require.NotContains(t, all, "13:45")
	require.NotContains(t, all, `"args"`)
}

// 同输入两次渲染字节一致（含 map 遍历的画像）。
func TestRecallDeterministic(t *testing.T) {
	build := func() string {
		mem := newCountingMem()
		mem.profile = &types.UserProfile{
			StableFacts:        map[string]any{"role": "架构师", "lang": "Go", "editor": "vim", "os": "macOS", "shell": "zsh"},
			BehavioralPatterns: map[string]any{"style": "简洁", "tone": "直接", "review": "严格"},
		}
		mem.events = []types.ScoredEvent{scoredEvent("t", "事件甲", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))}
		sCtx := newTurnCtx("查询", "")
		msgs, err := BuildPerceiveContext(context.Background(), mem, sCtx, nil)
		require.NoError(t, err)
		return joinContents(msgs)
	}
	first := build()
	for i := 0; i < 20; i++ {
		require.Equal(t, first, build())
	}
	require.Contains(t, first, "- editor: vim\n- lang: Go\n- os: macOS\n- role: 架构师\n- shell: zsh\n- review: 严格\n- style: 简洁\n- tone: 直接")
}

// 召回被预算放弃时不置覆盖标记，Plan 会补查。
func TestRecallTimeoutDoesNotMarkDone(t *testing.T) {
	kb := &blockingKnowledge{release: make(chan struct{})}
	defer close(kb.release)
	mem := newCountingMem()
	sCtx := newTurnCtx("你好", "打招呼")
	sCtx.KnowledgeSearcher = kb
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := BuildPlanContext(ctx, mem, sCtx, nil, nil)
	require.NoError(t, err, "超时应降级而非报错")
	require.False(t, sCtx.TurnRecall != nil && sCtx.TurnRecall.GoalDone)
}

func TestScoreFilter(t *testing.T) {
	l := recallLimits{minScoreRatio: 0.2}
	require.Equal(t, []bool{true, true, false}, scoreFilter([]float64{10, 2, 1.9}, l))
	// 全同分（RAG 现状恒 1.0）：相对阈值不误杀。
	require.Equal(t, []bool{true, true, true}, scoreFilter([]float64{1, 1, 1}, l))
	// 绝对下限。
	require.Equal(t, []bool{true, false}, scoreFilter([]float64{0.9, 0.3}, recallLimits{minScore: 0.5}))
	// 全关闭。
	require.Equal(t, []bool{true, true}, scoreFilter([]float64{5, 0.01}, recallLimits{}))
	// 全零分不除零、不丢弃。
	require.Equal(t, []bool{true}, scoreFilter([]float64{0}, l))
}

// L2/RAG 低分命中在采集阶段被丢弃。
type scoredCog struct{ hits []fsm.CogResult }

func (c scoredCog) FTSSearch(string, int) ([]fsm.CogResult, error) { return c.hits, nil }
func (scoredCog) VecKNN([]float32, int) ([]fsm.CogResult, error)   { return nil, nil }

func TestSemanticMinScoreRatioFiltersTail(t *testing.T) {
	mem := newCountingMem()
	cog := scoredCog{hits: []fsm.CogResult{
		{DocID: "a", Snippet: "高相关命中甲", Score: 12},
		{DocID: "b", Snippet: "低相关尾部乙", Score: 1},
	}}
	sCtx := newTurnCtx("x", "部署")
	msgs, err := BuildPlanContext(context.Background(), mem, sCtx, nil, cog)
	require.NoError(t, err)
	all := joinContents(msgs)
	require.Contains(t, all, "- 高相关命中甲")
	require.NotContains(t, all, "低相关尾部乙")
	require.NotContains(t, all, "score=", "BM25 原始分不再写进提示词")
}

// 召回条目已被 L2 历史包含则丢弃。
func TestRecallDedupAgainstHistory(t *testing.T) {
	mem := newCountingMem()
	mem.events = []types.ScoredEvent{
		scoredEvent("chat", "用户刚才说过要把配置迁移到 goose 工具", time.Now()),
		scoredEvent("chat", "一条与历史无关的独特记忆", time.Now()),
	}
	sCtx := newTurnCtx("继续", "")
	sCtx.ConversationHistory = []types.Message{
		{Role: "user", Content: "我想   把配置迁移到\ngoose 工具，然后跑测试"},
		{Role: "user", Content: "用户刚才说过要把配置迁移到 goose 工具"},
	}
	text, err := turnRecallText(context.Background(), mem, nil, sCtx, recallWant{episodicQuery: "继续", episodicK: 4}, "test")
	require.NoError(t, err)
	require.NotContains(t, text, "goose", "已在 L2 历史里的召回应丢弃（规范化空白后包含即算）")
	require.Contains(t, text, "一条与历史无关的独特记忆")
}

// Reflect 不再全文注入 8KB 的 ExecuteResult：与 Plan/Respond 同用观察上限投影（带取回提示）。
func TestBuildReflectContext_CapsExecuteResult(t *testing.T) {
	mem := newCountingMem()
	obs := "[output truncated: 7000 bytes total; full output: read_tool_ref(task_id=\"t\", id=\"x\")]\nHEAD…TAIL"
	sCtx := newTurnCtx("跑一下", "跑测试")
	sCtx.ExecuteResult = []byte(strings.Repeat("X", 7000))
	sCtx.Observations = []string{obs}

	msgs, err := BuildReflectContext(context.Background(), mem, sCtx)
	require.NoError(t, err)
	all := joinContents(msgs)
	require.Contains(t, all, "read_tool_ref(task_id=")
	require.NotContains(t, all, strings.Repeat("X", 500), "原始 7KB 结果不得再全文注入 Reflect")
}
