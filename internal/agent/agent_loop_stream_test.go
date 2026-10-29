package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

// streamTurnResult 一个回合内订阅到的全部流事件及其聚合。
type streamTurnResult struct {
	tokens      []string
	toolCalls   int
	toolResults []types.AgentStreamEvent
	history     []types.Message
}

func (r streamTurnResult) joined() string { return strings.Join(r.tokens, "") }

// runStreamTurn 以生产构造跑一个 agentic 回合并收集事件；background=true 模拟 AcquireHeadless 获取。
func runStreamTurn(t *testing.T, id string, background bool, completion string, responses []*types.ProviderResponse) streamTurnResult {
	t.Helper()
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	agent := NewAgentWithDefaults(id)
	agent.InjectProvider(&loopTestProvider{responses: responses})
	agent.InjectPolicyGate(&allowPolicyGate{})
	agent.InjectToolExecutor(newAwaitTestToolExecutor())
	agent.sCtx.SetBackgroundTurn(background)
	agent.SetTaskIntent(taint.NewTaintedString("请读两次文件再总结", taint.TaintSource{
		Module: "test", OriginTaintLevel: types.TaintHigh,
	}, "test"))
	if completion != "" {
		agent.SetLoopCompletion(completion)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := agent.SubscribeStream(ctx)

	done := make(chan struct{})
	go func() {
		_ = agent.Run(context.Background())
		close(done)
	}()
	if err := agent.SendIntent(types.TriggerIntentReceived); err != nil {
		t.Fatalf("SendIntent failed: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("agent run timeout")
	}

	var res streamTurnResult
	for {
		select {
		case ev := <-stream:
			switch ev.Type {
			case types.AgentStreamEventToken:
				res.tokens = append(res.tokens, ev.Content)
			case types.AgentStreamEventToolCall:
				res.toolCalls++
			case types.AgentStreamEventToolResult:
				res.toolResults = append(res.toolResults, ev)
			}
			continue
		default:
		}
		break
	}
	agent.sCtx.Mu.RLock()
	res.history = append(res.history, agent.sCtx.ConversationHistory...)
	agent.sCtx.Mu.RUnlock()
	return res
}

func threeRoundScript() []*types.ProviderResponse {
	readCall := func(id string) []types.InferToolCall {
		return []types.InferToolCall{{ID: id, Name: "read_file", Input: []byte(`{"path":"a.txt"}`)}}
	}
	return []*types.ProviderResponse{
		{Content: "先读文件。", ToolCalls: readCall("c1")},
		{Content: "再读一次。", ToolCalls: readCall("c2")},
		{Content: "结论：完成。"},
		{Content: "结论：完成。"}, // explicit 模式需连续两轮纯文本才触发 implicit_finish
	}
}

// TestR5_02_InteractiveTurnStreamsTextAndToolProgress 交互回合：每轮文本增量流式推送，
// 回复只出现一次，工具调用/结果事件逐个到达，历史中的助手内容等于用户实际看到的文本。
func TestR5_02_InteractiveTurnStreamsTextAndToolProgress(t *testing.T) {
	res := runStreamTurn(t, "r5-02-interactive", false, "", threeRoundScript())

	const want = "先读文件。再读一次。结论：完成。"
	if got := res.joined(); got != want {
		t.Errorf("Token 增量按序拼接 = %q，期望 %q（逐轮流式且不重复发布）", got, want)
	}
	if len(res.tokens) < 3 {
		t.Errorf("Token 事件数 = %d，期望每轮至少 1 条（共 3 轮）", len(res.tokens))
	}
	if res.toolCalls != 2 {
		t.Errorf("tool_call 事件数 = %d，期望 2", res.toolCalls)
	}
	if len(res.toolResults) != 2 {
		t.Fatalf("tool_result 事件数 = %d，期望 2", len(res.toolResults))
	}
	for _, ev := range res.toolResults {
		if !strings.Contains(ev.Content, "file content") {
			t.Errorf("tool_result 内容缺失: %q", ev.Content)
		}
	}
	if n := len(res.history); n != 2 || res.history[1].Content != want {
		t.Errorf("ConversationHistory 助手内容应等于流式文本 %q，实际 %+v", want, res.history)
	}
}

// TestR5_02_ToolResultTruncatedTo500 工具结果事件内容截断到 500 字符。
func TestR5_02_ToolResultTruncatedTo500(t *testing.T) {
	long := strings.Repeat("长", 800)
	got := truncateStreamToolResult(long)
	if r := []rune(got); len(r) > 500+3 {
		t.Errorf("截断后 %d 字符，期望 <= 503（含省略号）", len(r))
	}
	if truncateStreamToolResult("short") != "short" {
		t.Error("短内容不应被改动")
	}
}

// TestR5_02_BackgroundAndExplicitTurnsDoNotStream 委派 / cron / workflow 回合保持现状：
// 不逐轮推送文本与工具进度，终态只一次性发布最终回复（AcquireHeadless 靠它拼 Output）。
func TestR5_02_BackgroundAndExplicitTurnsDoNotStream(t *testing.T) {
	cases := []struct {
		name       string
		background bool
		completion string
	}{
		{"background", true, ""},
		{"explicit", false, "explicit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runStreamTurn(t, "r5-02-"+tc.name, tc.background, tc.completion, threeRoundScript())
			if res.toolCalls != 0 || len(res.toolResults) != 0 {
				t.Errorf("非交互回合不应推送工具进度: calls=%d results=%d", res.toolCalls, len(res.toolResults))
			}
			if len(res.tokens) > 1 {
				t.Errorf("非交互回合应只在终态发布一次最终回复，实际 Token 事件: %q", res.tokens)
			}
		})
	}
}
