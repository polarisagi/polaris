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

// loopTestProvider 模拟 LLM 原生流式调用的测试桩。
type loopTestProvider struct {
	mu        sync.Mutex
	responses []*types.ProviderResponse
	idx       int
	history   [][]types.Message
}

func (p *loopTestProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.idx
}

func (p *loopTestProvider) Infer(_ context.Context, _ []types.Message, _ ...types.InferOption) (*types.ProviderResponse, error) {
	return nil, fmt.Errorf("Infer not used in streaming loop")
}

func (p *loopTestProvider) StreamInfer(_ context.Context, msgs []types.Message, _ ...types.InferOption) (<-chan types.StreamEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.history = append(p.history, msgs)
	if p.idx >= len(p.responses) {
		return nil, fmt.Errorf("loopTestProvider: no more responses configured (idx=%d)", p.idx)
	}
	resp := p.responses[p.idx]
	p.idx++

	ch := make(chan types.StreamEvent, len(resp.ToolCalls)+3)
	if resp.Content != "" {
		ch <- types.StreamEvent{Type: types.StreamTextDelta, Content: resp.Content}
	}
	for _, tc := range resp.ToolCalls {
		rawInput := tc.Input
		if len(rawInput) == 0 {
			rawInput = []byte("{}")
		}
		payload, _ := json.Marshal(map[string]any{
			"id":    tc.ID,
			"name":  tc.Name,
			"input": json.RawMessage(rawInput),
		})
		ch <- types.StreamEvent{Type: types.StreamToolCall, Content: string(payload)}
	}
	if resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0 {
		ch <- types.StreamEvent{Type: types.StreamTextDelta, Usage: resp.Usage}
	}
	close(ch)
	return ch, nil
}

func (p *loopTestProvider) Capabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{SupportsStreaming: true, SupportsTools: true}
}
func (p *loopTestProvider) Tokenizer() protocol.TokenizerAdapter { return nil }
func (p *loopTestProvider) ModelID() string                      { return "loop-test-provider" }

// TestAgenticLoop_AutoCompletion_FinishWithoutTools 验证 auto 模式下无工具调用即直接作为最终回复完成。
func TestAgenticLoop_AutoCompletion_FinishWithoutTools(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	agent := NewAgentWithDefaults("test-loop-auto")
	prov := &loopTestProvider{
		responses: []*types.ProviderResponse{
			{Content: "你好！我是 polaris，很高兴为您服务。"},
		},
	}
	agent.InjectProvider(prov)
	agent.InjectPolicyGate(&allowPolicyGate{})
	agent.InjectToolExecutor(&mockToolExecutor{})

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

	if st := agent.sm.Current(); st != types.AgentStateComplete {
		t.Fatalf("auto 模式下无工具调用应完成，当前状态: %v", st)
	}
	if prov.callCount() != 1 {
		t.Fatalf("期望 LLM 调用 1 次，实际: %d", prov.callCount())
	}

	agent.sCtx.Mu.Lock()
	defer agent.sCtx.Mu.Unlock()
	if len(agent.sCtx.LoopMessages) != 1 {
		t.Fatalf("期望 LoopMessages 记录 1 条回复，实际: %d", len(agent.sCtx.LoopMessages))
	}
	if agent.sCtx.LoopMessages[0].Content != "你好！我是 polaris，很高兴为您服务。" {
		t.Fatalf("回复内容不符合预期: %q", agent.sCtx.LoopMessages[0].Content)
	}
}

// TestAgenticLoop_MultiRound_ConcurrentTools 验证多轮并发工具调用与结果收集。
func TestAgenticLoop_MultiRound_ConcurrentTools(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	agent := NewAgentWithDefaults("test-loop-multi-round")
	prov := &loopTestProvider{
		responses: []*types.ProviderResponse{
			{
				Content: "正在读取文件...",
				ToolCalls: []types.InferToolCall{
					{ID: "c1", Name: "read_file", Input: []byte(`{"path":"a.txt"}`)},
					{ID: "c2", Name: "read_file", Input: []byte(`{"path":"b.txt"}`)},
				},
			},
			{
				Content: "已成功读取 a.txt 与 b.txt 并完成处理。",
			},
		},
	}
	agent.InjectProvider(prov)
	agent.InjectPolicyGate(&allowPolicyGate{})
	mockExec := &mockToolExecutor{}
	agent.InjectToolExecutor(mockExec)

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

	if st := agent.sm.Current(); st != types.AgentStateComplete {
		t.Fatalf("多轮工具执行后应完成，当前状态: %v", st)
	}
	if prov.callCount() != 2 {
		t.Fatalf("期望 LLM 交互 2 轮，实际: %d", prov.callCount())
	}
	if !mockExec.ExecuteWithTaintCalled {
		t.Fatal("工具执行器必须被调用")
	}

	agent.sCtx.Mu.Lock()
	defer agent.sCtx.Mu.Unlock()
	// 期望消息顺序：assistant(2 tool_use) -> tool_result(c1) -> tool_result(c2) -> assistant(final)
	if len(agent.sCtx.LoopMessages) != 4 {
		t.Fatalf("期望 4 条 LoopMessages，实际: %d", len(agent.sCtx.LoopMessages))
	}
}

// TestAgenticLoop_ExplicitCompletion_FinishTool 验证 explicit 模式下必须调用 finish 工具才结束。
func TestAgenticLoop_ExplicitCompletion_FinishTool(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	agent := NewAgentWithDefaults("test-loop-explicit")
	agent.sCtx.LoopCompletion = "explicit"

	prov := &loopTestProvider{
		responses: []*types.ProviderResponse{
			{Content: "我先思考一下，还没调用 finish"},
			{
				Content: "任务已完毕",
				ToolCalls: []types.InferToolCall{
					{ID: "c_finish", Name: "finish", Input: []byte(`{"summary":"all done"}`)},
				},
			},
		},
	}
	agent.InjectProvider(prov)
	agent.InjectPolicyGate(&allowPolicyGate{})
	agent.InjectToolExecutor(&mockToolExecutor{})

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

	if st := agent.sm.Current(); st != types.AgentStateComplete {
		t.Fatalf("调用 finish 工具后应完成，当前状态: %v", st)
	}
	if prov.callCount() != 2 {
		t.Fatalf("explicit 模式下首轮无 finish 工具不应退出，期望调用 2 次，实际: %d", prov.callCount())
	}
}

// TestAgenticLoop_RunawayGuard_WarningAndHardAbort 验证失控保护：首触上限告警再给一轮，次轮仍不收尾硬熔断。
func TestAgenticLoop_RunawayGuard_WarningAndHardAbort(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	agent := NewAgentWithDefaults("test-loop-runaway")
	agent.sCtx.LoopRunawayLimit = 2

	prov := &loopTestProvider{
		responses: []*types.ProviderResponse{
			{
				Content: "第1轮调用工具",
				ToolCalls: []types.InferToolCall{
					{ID: "c1", Name: "read_file", Input: []byte(`{"path":"a"}`)},
				},
			},
			{
				Content: "第2轮达到上限但继续调用工具",
				ToolCalls: []types.InferToolCall{
					{ID: "c2", Name: "read_file", Input: []byte(`{"path":"b"}`)},
				},
			},
			{
				Content: "第3轮仍然不收尾调用工具",
				ToolCalls: []types.InferToolCall{
					{ID: "c3", Name: "read_file", Input: []byte(`{"path":"c"}`)},
				},
			},
		},
	}
	agent.InjectProvider(prov)
	agent.InjectPolicyGate(&allowPolicyGate{})
	agent.InjectToolExecutor(&mockToolExecutor{})

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

	if st := agent.sm.Current(); st != types.AgentStateFailed {
		t.Fatalf("失控硬熔断后应进入 S_FAILED，当前状态: %v", st)
	}

	agent.sCtx.Mu.Lock()
	warned := agent.sCtx.LoopWarningInjected
	warnPrompt := agent.sCtx.LoopWarningPrompt
	loopMsgs := agent.sCtx.LoopMessages
	agent.sCtx.Mu.Unlock()
	if !warned {
		t.Fatal("必须触发并记录 LoopWarningInjected")
	}
	if !strings.Contains(warnPrompt, "失控保护告警") {
		t.Fatalf("必须记录失控保护告警易变提示，实际: %q", warnPrompt)
	}
	prov.mu.Lock()
	var foundVolatileUserWarning bool
	for _, hist := range prov.history {
		for _, m := range hist {
			if m.Role == "user" && strings.Contains(m.Content, "失控保护告警") {
				foundVolatileUserWarning = true
				break
			}
		}
		if foundVolatileUserWarning {
			break
		}
	}
	prov.mu.Unlock()

	if !foundVolatileUserWarning {
		t.Fatal("必须向模型上下文注入 user 角色的易变收尾告警消息")
	}
	for _, m := range loopMsgs {
		if m.Role == "system" {
			t.Fatalf("LoopMessages 中严禁追加 system 角色消息: %+v", m)
		}
	}
}

// TestAgenticLoop_ReplayMode_CrashRecovery 验证 ADR-0104 / HE-6 崩溃恢复与回放测试。
func TestAgenticLoop_ReplayMode_CrashRecovery(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	agent := NewAgentWithDefaults("test-loop-replay")
	provider := &failIfCalledProvider{}
	agent.InjectProvider(provider)
	agent.InjectPolicyGate(&allowPolicyGate{})
	agent.InjectToolExecutor(&mockToolExecutor{})

	agent.InjectReplayData([]protocol.ReplayLLMCall{
		{
			Response: map[string]any{
				"content": "崩溃前已录制的回放响应",
			},
		},
	})
	protocol.SetReplayMode(true)
	defer protocol.SetReplayMode(false)

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

	if st := agent.sm.Current(); st != types.AgentStateComplete {
		t.Fatalf("重放模式下应推进到 Complete，当前状态: %v", st)
	}
	if got := provider.callCount(); got != 0 {
		t.Fatalf("重放期间真实 Provider 不应被调用，实际调用次数: %d", got)
	}
}
