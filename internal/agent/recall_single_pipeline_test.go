package agent

// ADR-0105 决策十：召回单一管线。此前 executeEffect 在 PromptFn 之后又经 Assembler 召回一次，
// 并把 "Relevant Context:" 插在 L1 与 L2 之间，使 Plan/Reflect/Respond 的 L2 历史前缀全部失配；
// 前缀门控只测了 builder，没覆盖真实请求边界。本测试以**实际发往 Provider 的消息**为断言对象。

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

// msgRecorder 在脚本化 Provider 之上记录每个阶段收到的完整消息序列（含角色与次序）。
type msgRecorder struct {
	*scriptedTurnProvider
	rmu  sync.Mutex
	reqs map[string][][]types.Message
}

func newMsgRecorder(script map[string][]scriptedReply) *msgRecorder {
	return &msgRecorder{scriptedTurnProvider: newScriptedTurnProvider(script), reqs: map[string][][]types.Message{}}
}

func phaseOf(msgs []types.Message) string {
	var all strings.Builder
	for _, m := range msgs {
		all.WriteString(m.Content + "\n")
	}
	for _, pm := range phaseMarkers {
		if strings.Contains(all.String(), pm.marker) {
			return pm.phase
		}
	}
	return "unknown"
}

func (r *msgRecorder) capture(msgs []types.Message) {
	r.rmu.Lock()
	defer r.rmu.Unlock()
	ph := phaseOf(msgs)
	r.reqs[ph] = append(r.reqs[ph], append([]types.Message(nil), msgs...))
}

func (r *msgRecorder) Infer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
	r.capture(msgs)
	// custom-nolint:bare-infer 测试替身：只转发给脚本化 provider 并记录真实请求，不是生产调用点
	return r.scriptedTurnProvider.Infer(ctx, msgs, opts...)
}

func (r *msgRecorder) StreamInfer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	r.capture(msgs)
	// custom-nolint:bare-infer 测试替身：只转发给脚本化 provider 并记录真实请求，不是生产调用点
	return r.scriptedTurnProvider.StreamInfer(ctx, msgs, opts...)
}

// recallMemory 在集成用 mock 之上提供有内容可召回的情景与反思。
type recallMemory struct{ *mockMemoryForIntegration }

func (recallMemory) ListEpisodicEvents(context.Context, types.EpisodicQuery) ([]types.ScoredEvent, error) {
	return []types.ScoredEvent{{Score: 1, Event: &types.Event{
		Type:      "task_done",
		Payload:   []byte(`{"summary":"RECALL_EPISODIC_MARKER 上次迁移用了 goose"}`),
		CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	}}}, nil
}

func (recallMemory) ListReflections(context.Context, types.ReflectionQuery) ([]types.ReflectionEntry, error) {
	return []types.ReflectionEntry{{Strategy: "先备份", Decision: "RECALL_REFLECTION_MARKER 迁移前必须备份", CreatedAt: time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)}}, nil
}

func countIn(msgs []types.Message, sub string) int {
	n := 0
	for _, m := range msgs {
		n += strings.Count(m.Content, sub)
	}
	return n
}

func indexOfContent(msgs []types.Message, sub string) int {
	for i, m := range msgs {
		if strings.Contains(m.Content, sub) {
			return i
		}
	}
	return -1
}

func runMemoryTurn(t *testing.T, p protocol.Provider, history []types.Message, input string) {
	t.Helper()
	a := NewAgentWithDefaults("recall-" + t.Name())
	pinSystem2Routing(t)
	a.surpriseCalc = fixedSurprise(0.7)
	a.InjectProvider(p)
	a.InjectPolicyGate(&allowPolicyGate{})
	a.InjectToolExecutor(&mockToolExecutor{})
	a.InjectMemory(recallMemory{&mockMemoryForIntegration{
		episodic: &mockEpisodicMemForIntegration{},
		working:  &mockWorkingMemForIntegration{immutable: &mockImmutableCoreForIntegration{}},
	}})
	a.SetConversationHistory(history)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sub := a.SubscribeStream(ctx)
	done := make(chan struct{})
	go func() { _ = a.Run(ctx); close(done) }()
	a.SetTaskIntent(taint.NewTaintedString(input, taint.TaintSource{OriginTaintLevel: types.TaintHigh}, "eval"))
	if err := a.SendIntent(types.TriggerIntentReceived); err != nil {
		t.Fatalf("SendIntent: %v", err)
	}
	for {
		select {
		case ev := <-sub:
			if ev.Type == types.AgentStreamEventStatus && ev.Content == "task_done" {
				<-done
				return
			}
		case <-ctx.Done():
			t.Fatal("回合未在期限内结束")
		}
	}
}

func TestSingleRecallPipeline_NoSecondInjectionAtRequestBoundary(t *testing.T) {
	p := newMsgRecorder(map[string][]scriptedReply{
		"perceive": {{content: `{"Goal":"迁移数据库到新版本","NeedsTools":true}`}},
		"plan": {{toolCalls: []types.InferToolCall{
			{ID: "call_1", Name: "read_file", Input: json.RawMessage(`{"path":"README.md"}`)},
		}}},
		"reflect": {{content: `{"GoalAchieved":true,"Errors":[],"Learnings":[]}`}},
		"respond": {{content: "迁移方案已确认。"}},
	})
	history := []types.Message{
		{Role: "user", Content: "HISTORY_USER_MARKER 我想把数据库迁移到新版本"},
		{Role: "assistant", Content: "HISTORY_ASSISTANT_MARKER 好的，需要先评估风险"},
	}
	runMemoryTurn(t, p, history, "帮我迁移数据库")

	p.rmu.Lock()
	defer p.rmu.Unlock()
	var prefixes [][]types.Message
	for _, phase := range []string{"perceive", "plan", "reflect", "respond"} {
		reqs := p.reqs[phase]
		if len(reqs) == 0 {
			t.Fatalf("阶段 %s 没有请求被记录", phase)
		}
		for _, msgs := range reqs {
			if n := countIn(msgs, "Relevant Context:"); n != 0 {
				t.Errorf("%s 请求中不得再出现旁路注入的 \"Relevant Context:\"（%d 处）", phase, n)
			}
			// 历史逐条真实消息：甲（user）后紧跟乙（assistant），二者之间、之前都不得被插入任何内容。
			iu := indexOfContent(msgs, "HISTORY_USER_MARKER")
			ia := indexOfContent(msgs, "HISTORY_ASSISTANT_MARKER")
			if iu < 0 || ia != iu+1 {
				t.Fatalf("%s：L2 历史不连续（user@%d assistant@%d）", phase, iu, ia)
			}
			// L0/L1 是开头连续的 system；紧接着就是 L2，两者之间不得有非 system 的插入物。
			for i := 0; i < iu; i++ {
				if msgs[i].Role != "system" {
					t.Errorf("%s：L1 与 L2 之间出现非 system 消息 #%d role=%s", phase, i, msgs[i].Role)
				}
			}
			prefixes = append(prefixes, msgs[:ia+1])
			// 召回只在 L4 首位出现（历史之后），且每个标记至多一份。
			for _, marker := range []string{"RECALL_EPISODIC_MARKER", "RECALL_REFLECTION_MARKER"} {
				if n := countIn(msgs, marker); n > 1 {
					t.Errorf("%s：召回 %s 重复出现 %d 次", phase, marker, n)
				}
				if i := indexOfContent(msgs, marker); i >= 0 && i <= ia {
					t.Errorf("%s：召回 %s 位于 L2 之前（#%d），破坏共享前缀", phase, marker, i)
				}
			}
		}
	}
	// 四个阶段 L0..L2 字节一致。
	for i := 1; i < len(prefixes); i++ {
		if len(prefixes[i]) != len(prefixes[0]) {
			t.Fatalf("前缀长度不一致：%d vs %d", len(prefixes[i]), len(prefixes[0]))
		}
		for j := range prefixes[0] {
			if prefixes[i][j].Role != prefixes[0][j].Role || prefixes[i][j].Content != prefixes[0][j].Content {
				t.Errorf("L0..L2 第 %d 条在阶段间不一致：\n%q\n%q", j, prefixes[0][j].Content, prefixes[i][j].Content)
			}
		}
	}
	// 召回确实经回合内管线进入了 Plan（反思由 Plan 以 Goal 补查，情景由 Perceive 查过后复用）。
	plan := p.reqs["plan"][0]
	if countIn(plan, "RECALL_REFLECTION_MARKER") != 1 || countIn(plan, "RECALL_EPISODIC_MARKER") != 1 {
		t.Errorf("Plan 应各含一份召回的反思与情景：reflection=%d episodic=%d",
			countIn(plan, "RECALL_REFLECTION_MARKER"), countIn(plan, "RECALL_EPISODIC_MARKER"))
	}
}
