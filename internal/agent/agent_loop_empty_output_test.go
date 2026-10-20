package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// emptyScriptStep 脚本化的一次 LLM 响应：可只有思考链（模拟 DeepSeek 思考后空 stop）。
type emptyScriptStep struct {
	reasoning string
	content   string
	toolCalls []types.InferToolCall
}

// emptyScriptProvider 记录每次请求的消息与选项，按脚本回放。
type emptyScriptProvider struct {
	mu    sync.Mutex
	steps []emptyScriptStep
	msgs  [][]types.Message
	opts  []types.InferOptions
}

func (p *emptyScriptProvider) Infer(context.Context, []types.Message, ...types.InferOption) (*types.ProviderResponse, error) {
	return nil, fmt.Errorf("not used")
}

func (p *emptyScriptProvider) StreamInfer(_ context.Context, msgs []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var o types.InferOptions
	for _, fn := range opts {
		fn(&o)
	}
	p.msgs = append(p.msgs, msgs)
	p.opts = append(p.opts, o)
	if len(p.msgs) > len(p.steps) {
		return nil, fmt.Errorf("script exhausted at call %d", len(p.msgs))
	}
	st := p.steps[len(p.msgs)-1]
	ch := make(chan types.StreamEvent, 8)
	if st.reasoning != "" {
		ch <- types.StreamEvent{Type: types.StreamThinking, Content: st.reasoning}
	}
	if st.content != "" {
		ch <- types.StreamEvent{Type: types.StreamTextDelta, Content: st.content}
	}
	for _, tc := range st.toolCalls {
		payload, _ := json.Marshal(map[string]any{"id": tc.ID, "name": tc.Name, "input": json.RawMessage(tc.Input)})
		ch <- types.StreamEvent{Type: types.StreamToolCall, Content: string(payload)}
	}
	ch <- types.StreamEvent{Type: types.StreamTextDelta, Usage: types.Usage{InputTokens: 10, OutputTokens: 5}, FinishReason: "stop"}
	close(ch)
	return ch, nil
}

func (p *emptyScriptProvider) Capabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{SupportsStreaming: true, SupportsTools: true}
}
func (p *emptyScriptProvider) Tokenizer() protocol.TokenizerAdapter { return nil }
func (p *emptyScriptProvider) ModelID() string                      { return "empty-script" }

func runEmptyScript(t *testing.T, steps ...emptyScriptStep) (*Agent, *emptyScriptProvider) {
	t.Helper()
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	agent := NewAgentWithDefaults("test-loop-empty")
	prov := &emptyScriptProvider{steps: steps}
	agent.InjectProvider(prov)
	agent.InjectPolicyGate(&allowPolicyGate{})
	agent.InjectToolExecutor(&mockToolExecutor{})
	done := make(chan struct{})
	go func() { _ = agent.Run(context.Background()); close(done) }()
	if err := agent.SendIntent(types.TriggerIntentReceived); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("agent run timeout")
	}
	return agent, prov
}

// B3.1：空输出重试不得重发与上次相同的请求——追加 user 提示，并回传 reasoning_content。
func TestLoopEmpty_RetryCarriesNudgeAndReasoning(t *testing.T) {
	agent, prov := runEmptyScript(t,
		emptyScriptStep{reasoning: "Let me start with web_search calls in parallel."},
		emptyScriptStep{content: "这是最终回答"},
	)
	if st := agent.sm.Current(); st != types.AgentStateComplete {
		t.Fatalf("重试拿到回答后应完成，当前 %v", st)
	}
	if len(prov.msgs) != 2 {
		t.Fatalf("应共 2 次 LLM 调用，实际 %d", len(prov.msgs))
	}
	first, second := prov.msgs[0], prov.msgs[1]
	if len(second) <= len(first) {
		t.Fatalf("重试请求必须比首次请求多出提示消息: %d vs %d", len(second), len(first))
	}
	// 末尾易变块（usage）也是 user 消息，故按内容查找提示而非取末位。
	var nudge, placeholder *types.Message
	for i := range second {
		if second[i].Role == "user" && strings.Contains(second[i].Content, "上一轮只完成了思考") {
			nudge = &second[i]
		}
		if second[i].Role == "assistant" && second[i].ReasoningContent != "" {
			placeholder = &second[i]
		}
	}
	if nudge == nil || !strings.Contains(nudge.Content, "请直接发出工具调用，或给出最终回答") {
		t.Fatalf("重试请求缺少 user 提示")
	}
	if placeholder == nil || placeholder.ReasoningContent != "Let me start with web_search calls in parallel." || placeholder.Content == "" {
		t.Fatalf("重试请求应回传上一轮 reasoning_content，且占位助手消息正文非空: %+v", placeholder)
	}
	// 瞬态消息不得进入持久的 LoopMessages，也不得被当作「助手正文」。
	agent.sCtx.Mu.Lock()
	defer agent.sCtx.Mu.Unlock()
	for _, m := range agent.sCtx.LoopMessages {
		if strings.Contains(m.Content, "上一轮只完成了思考") || strings.Contains(m.Content, "没有输出正文") {
			t.Fatalf("提示消息泄漏进 LoopMessages: %+v", m)
		}
	}
	if agent.sCtx.LoopEmptyRetries != 0 {
		t.Fatalf("拿到非空输出后恢复档位应清零，实际 %d", agent.sCtx.LoopEmptyRetries)
	}
}

// B3.2a：已有工具结果、重试仍空 → 第 2 次额外调用为 tool_choice=none 的无工具收尾，给出回答而非报错。
func TestLoopEmpty_WrapUpWithoutToolsWhenToolResultsExist(t *testing.T) {
	agent, prov := runEmptyScript(t,
		emptyScriptStep{toolCalls: []types.InferToolCall{{ID: "c1", Name: "read_file", Input: []byte(`{"path":"a.txt"}`)}}},
		emptyScriptStep{reasoning: "第一次空"},
		emptyScriptStep{reasoning: "第二次空"},
		emptyScriptStep{content: "基于已读取的结果：完成"},
	)
	if st := agent.sm.Current(); st != types.AgentStateComplete {
		t.Fatalf("收尾调用成功后应完成，当前 %v", st)
	}
	if len(prov.msgs) != 4 {
		t.Fatalf("应为 1 次工具调用 + 空输出 + 2 次额外调用 = 4，实际 %d", len(prov.msgs))
	}
	if prov.opts[2].ToolChoice == "none" {
		t.Fatal("第 1 次额外调用是提示重试，仍应允许工具调用")
	}
	if prov.opts[3].ToolChoice != "none" {
		t.Fatalf("第 2 次额外调用应 tool_choice=none，实际 %q", prov.opts[3].ToolChoice)
	}
	var wrap bool
	for _, m := range prov.msgs[3] {
		wrap = wrap || (m.Role == "user" && strings.Contains(m.Content, "基于已获得的工具结果直接给出最终回答"))
	}
	if !wrap {
		t.Fatal("收尾调用应带「基于已有结果直接回答」提示")
	}
}

// B3.2b：尚无工具结果、重试仍空 → 第 2 次额外调用关闭思考模式。
func TestLoopEmpty_DisableThinkingWhenNoToolResults(t *testing.T) {
	agent, prov := runEmptyScript(t,
		emptyScriptStep{reasoning: "空1"},
		emptyScriptStep{reasoning: "空2"},
		emptyScriptStep{content: "关闭思考后的回答"},
	)
	if st := agent.sm.Current(); st != types.AgentStateComplete {
		t.Fatalf("当前 %v", st)
	}
	if len(prov.opts) != 3 {
		t.Fatalf("调用次数 %d", len(prov.opts))
	}
	if prov.opts[1].ThinkingMode == types.ThinkingDisabled && prov.opts[0].ThinkingMode != types.ThinkingDisabled {
		t.Fatal("第 1 次额外调用不应关闭思考")
	}
	if prov.opts[2].ThinkingMode != types.ThinkingDisabled {
		t.Fatalf("第 2 次额外调用应关闭思考模式，实际 %v", prov.opts[2].ThinkingMode)
	}
	if prov.opts[2].ToolChoice == "none" {
		t.Fatal("尚无工具结果时不应禁用工具")
	}
}

// B3.3：额外调用上限 2 次；仍为空且无正文 → 友好报错，且总调用恰为 3 次。
func TestLoopEmpty_GivesUpAfterTwoExtraCalls(t *testing.T) {
	agent, prov := runEmptyScript(t,
		emptyScriptStep{reasoning: "空1"},
		emptyScriptStep{reasoning: "空2"},
		emptyScriptStep{reasoning: "空3"},
		emptyScriptStep{content: "不应被调用"},
	)
	if st := agent.sm.Current(); st != types.AgentStateFailed {
		t.Fatalf("三次均空应失败，当前 %v", st)
	}
	if len(prov.msgs) != 3 {
		t.Fatalf("总调用应为 1 + 2 次额外 = 3，实际 %d", len(prov.msgs))
	}
}
