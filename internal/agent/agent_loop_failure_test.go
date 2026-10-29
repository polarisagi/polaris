package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

type errorLoopProvider struct {
	err error
}

func (p *errorLoopProvider) Infer(_ context.Context, _ []types.Message, _ ...types.InferOption) (*types.ProviderResponse, error) {
	return nil, p.err
}

func (p *errorLoopProvider) StreamInfer(_ context.Context, _ []types.Message, _ ...types.InferOption) (<-chan types.StreamEvent, error) {
	return nil, p.err
}

func (p *errorLoopProvider) Capabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{SupportsStreaming: true, SupportsTools: true}
}
func (p *errorLoopProvider) Tokenizer() protocol.TokenizerAdapter { return nil }
func (p *errorLoopProvider) ModelID() string                      { return "error-loop-provider" }

// TestAgenticLoop_ProviderFailure_TerminalStateAndErrorEvent 验证 LLM 推理失败时通过合法 FSM 转移进入 AgentStateFailed，
// 且正常收尾并发出错误事件，不抛出 "unknown next state: S_FAILED"（R2-13）。
func TestAgenticLoop_ProviderFailure_TerminalStateAndErrorEvent(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	agent := NewAgentWithDefaults("test-loop-failure")
	prov := &errorLoopProvider{
		err: fmt.Errorf("upstream provider network timeout"),
	}
	agent.InjectProvider(prov)
	agent.InjectPolicyGate(&allowPolicyGate{})
	agent.InjectToolExecutor(&mockToolExecutor{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := agent.SubscribeStream(ctx)

	done := make(chan struct{})
	var runErr error
	go func() {
		runErr = agent.Run(context.Background())
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

	// 1. 验证 Run() 正常收尾返回 nil，而非因 "unknown next state: S_FAILED" 异常中止
	if runErr != nil {
		t.Fatalf("Run() 应当由 FSM 正常收尾返回 nil，实际返回错误: %v", runErr)
	}

	// 2. 验证回合以正确的失败状态（AgentStateFailed）终止
	if st := agent.sm.Current(); st != types.AgentStateFailed {
		t.Fatalf("LLM 推理失败后期望进入 AgentStateFailed，实际: %v", st)
	}

	// 3. 验证流中收到错误事件且不含 "unknown next state"
	var gotErrorEvent bool
	var errorContents []string
drain:
	for {
		select {
		case ev := <-stream:
			if ev.Type == types.AgentStreamEventError {
				gotErrorEvent = true
				errorContents = append(errorContents, ev.Content)
			}
		default:
			break drain
		}
	}
	if !gotErrorEvent {
		t.Fatal("期望收到 AgentStreamEventError 事件，实际未收到")
	}
	for _, content := range errorContents {
		if strings.Contains(content, "unknown next state") {
			t.Fatalf("错误事件中不应包含未识别状态错误，实际内容: %q", content)
		}
	}
}
