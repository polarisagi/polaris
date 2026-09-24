package agentctx

import (
	"context"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

func newRespondTestMemory() *mockMemory {
	return &mockMemory{
		episodic: &mockEpisodicMem{},
		working:  &mockWorkingMem{immutable: &mockImmutableCore{}},
	}
}

func joinContents(msgs []types.Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		sb.WriteString(m.Role + ": " + m.Content + "\n")
	}
	return sb.String()
}

// TestBuildRespondContext_DirectReply 直答路径：人格 + 回复契约 + 对话历史 + 本轮意图，
// 且不做记忆检索（回复只基于本回合事实，见 BuildRespondContext 注释）。
func TestBuildRespondContext_DirectReply(t *testing.T) {
	mem := newRespondTestMemory()
	sCtx := &fsm.StateContext{
		RawIntentTS: taint.NewTaintedString("那你能做什么？", taint.TaintSource{OriginTaintLevel: types.TaintHigh}, "test"),
		ConversationHistory: []types.Message{
			{Role: "user", Content: "你是谁"},
			{Role: "assistant", Content: "我是 Polaris"},
		},
	}
	msgs, err := BuildRespondContext(context.Background(), mem, sCtx)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if msgs[0].Content != "[Immutable Core Rule: NO HARMFUL ACT]" {
		t.Fatalf("ImmutableCore（人格/用户偏好）必须前置: %q", msgs[0].Content)
	}
	all := joinContents(msgs)
	if !strings.Contains(all, "REPLY TO THE USER") {
		t.Error("缺少 kernel/respond.md 阶段契约")
	}
	if !strings.Contains(all, "<conversation_history>") || !strings.Contains(all, "我是 Polaris") {
		t.Error("缺少对话历史：多轮对话将失忆")
	}
	if !strings.Contains(all, "UNTRUSTED_DATA") {
		t.Error("对话历史必须按 TaintHigh 进围栏，不得提升为指令")
	}
	if strings.Contains(all, "<execution_result>") {
		t.Error("直答路径不应出现执行结果区")
	}
	if len(mem.episodic.queries) != 0 {
		t.Error("Respond 不应做情景记忆检索")
	}
	// 末尾是收尾提醒（利用位置优势重申"本阶段不调工具"），其前是本轮用户消息。
	if last := msgs[len(msgs)-1]; last.Role != "system" || !strings.Contains(last.Content, "Tools are not available") {
		t.Errorf("末条应为回复收尾提醒: %+v", last)
	}
	if prev := msgs[len(msgs)-2]; prev.Role != "user" || !strings.Contains(prev.Content, "那你能做什么？") {
		t.Errorf("本轮用户消息应紧邻收尾提醒之前: %+v", prev)
	}
}

// TestBuildRespondContext_AfterExecution 执行路径：回复必须能看到目标、执行结果与
// 反思结论，否则失败的回合会被写成成功。
func TestBuildRespondContext_AfterExecution(t *testing.T) {
	sCtx := &fsm.StateContext{
		RawIntentTS:   taint.NewTaintedString("读 README", taint.TaintSource{OriginTaintLevel: types.TaintHigh}, "test"),
		TaskModel:     &fsm.TaskModel{Goal: "读取 README.md 并总结"},
		ExecuteResult: []byte("error: file not found"),
		Reflection:    &fsm.ReflectionModel{GoalAchieved: false, Errors: []string{"README.md 不存在"}},
	}
	msgs, err := BuildRespondContext(context.Background(), newRespondTestMemory(), sCtx)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	all := joinContents(msgs)
	for _, want := range []string{"读取 README.md 并总结", "error: file not found", "GoalAchieved: false", "README.md 不存在"} {
		if !strings.Contains(all, want) {
			t.Errorf("回复上下文缺少 %q", want)
		}
	}
}

func TestBuildPerceiveContext_IncludesConversationHistory(t *testing.T) {
	sCtx := &fsm.StateContext{
		RawIntentTS:         taint.NewTaintedString("再做一次", taint.TaintSource{OriginTaintLevel: types.TaintHigh}, "test"),
		ConversationHistory: []types.Message{{Role: "user", Content: "把 a.txt 复制到 b.txt"}},
	}
	msgs, err := BuildPerceiveContext(context.Background(), newRespondTestMemory(), sCtx, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.Contains(joinContents(msgs), "把 a.txt 复制到 b.txt") {
		t.Error("Perceive 需要对话历史才能把「再做一次」消解为自包含 Goal")
	}
}
