package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// countingToolExecutor 用于统计工具调用次数及定制污点返回。
type countingToolExecutor struct {
	calls      atomic.Int32
	taintLevel types.TaintLevel
}

func (c *countingToolExecutor) Lookup(name string) (types.Tool, error) {
	return types.Tool{
		Name:       name,
		Source:     types.ToolBuiltin,
		Capability: types.CapReadOnly,
	}, nil
}

func (c *countingToolExecutor) ExecuteWithTaint(_ context.Context, _ string, _ []byte, _ types.TaintLevel) (*types.ToolResult, error) {
	c.calls.Add(1)
	return &types.ToolResult{
		Success:    true,
		Output:     []byte(`{"result":"ok"}`),
		TaintLevel: c.taintLevel,
	}, nil
}

// TestP6_PromptFn_NoSideEffect_LoopRoundIdempotent 验证 PromptFn 必须是无副作用的纯构造函数。
// 在同一轮内多次调用 PromptFn（如重试、溢出恢复、PRM 候选等场景），LoopRound 绝不得递增。
func TestP6_PromptFn_NoSideEffect_LoopRoundIdempotent(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	agent := NewAgentWithDefaults("test-p6-idempotent")
	agent.sCtx.LoopRound = 1

	pCtx := protocol.StateContext{}
	eff := agent.sm.LoopEffect(agent.sCtx)
	if eff == nil {
		t.Fatal("S_LOOP 状态必须能获取到 LLMFillEffect")
	}

	llmEff, ok := eff.(protocol.LLMFillEffect)
	if !ok || llmEff.PromptFn == nil {
		t.Fatal("获取的 Effect 必须为 LLMFillEffect 且 PromptFn 非空")
	}

	// 连续调用 3 次 PromptFn
	_ = llmEff.PromptFn(pCtx)
	_ = llmEff.PromptFn(pCtx)
	_ = llmEff.PromptFn(pCtx)

	agent.sCtx.Mu.Lock()
	gotRound := agent.sCtx.LoopRound
	agent.sCtx.Mu.Unlock()

	if gotRound != 1 {
		t.Fatalf("PromptFn 必须无副作用，期望 LoopRound=1，实际 got=%d", gotRound)
	}
}

// TestP2_NilDAGValidator_FailClosed 验证校验器为 nil 时必须 fail-closed，
// 绝不能跳过校验直接执行真实工具。
func TestP2_NilDAGValidator_FailClosed(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	agent := NewAgentWithDefaults("test-p2-fail-closed")
	agent.dagValidator = nil // 显式注入 nil 校验器

	exec := &countingToolExecutor{}
	agent.InjectToolExecutor(exec)

	agent.sCtx.LoopPendingToolCalls = []types.InferToolCall{
		{ID: "call-1", Name: "mock_tool", Input: []byte(`{}`)},
	}

	_ = agent.runExecuteLoop(context.Background())

	// 1. 工具调用次数必须为 0
	if calls := exec.calls.Load(); calls != 0 {
		t.Fatalf("fail-closed 违例：校验器为 nil 时工具不得被执行，实际调用次数=%d", calls)
	}

	// 2. 必须向模型注入 fail-closed 错误消息
	agent.sCtx.Mu.Lock()
	msgs := agent.sCtx.LoopMessages
	agent.sCtx.Mu.Unlock()

	foundErrMsg := false
	for _, m := range msgs {
		if strings.Contains(m.Content, "fail-closed") || strings.Contains(m.Content, "security validator is missing") {
			foundErrMsg = true
			break
		}
	}
	if !foundErrMsg {
		t.Fatal("未在 LoopMessages 中找到 fail-closed 错误响应消息")
	}
}

// TestP4_ToolResultTaint_PropagateToGlobalTaintLevel 验证工具结果返回的高污点
// 必须立即加锁并入 sCtx.GlobalTaintLevel（只升不降），并标记在 tool 消息上。
func TestP4_ToolResultTaint_PropagateToGlobalTaintLevel(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	agent := NewAgentWithDefaults("test-p4-taint-propagate")
	agent.InjectPolicyGate(&allowPolicyGate{})
	agent.sCtx.GlobalTaintLevel = types.TaintNone

	exec := &countingToolExecutor{taintLevel: types.TaintHigh}
	agent.InjectToolExecutor(exec)

	agent.sCtx.LoopPendingToolCalls = []types.InferToolCall{
		{ID: "call-fetch", Name: "web_fetch", Input: []byte(`{}`)},
	}

	_ = agent.runExecuteLoop(context.Background())

	agent.sCtx.Mu.Lock()
	gotGlobalTaint := agent.sCtx.GlobalTaintLevel
	msgs := agent.sCtx.LoopMessages
	agent.sCtx.Mu.Unlock()

	if gotGlobalTaint != types.TaintHigh {
		t.Fatalf("工具执行返回 TaintHigh 后，GlobalTaintLevel 未同步抬升，got=%v, want=%v", gotGlobalTaint, types.TaintHigh)
	}

	if len(msgs) == 0 {
		t.Fatal("LoopMessages 必须包含工具返回结果消息")
	}
	lastMsg := msgs[len(msgs)-1]
	if lastMsg.TaintLevel != types.TaintHigh {
		t.Fatalf("tool 消息的 TaintLevel 未标记为 TaintHigh，got=%v", lastMsg.TaintLevel)
	}
}
