package agent

import (
	"context"
	"fmt"
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

// TestP3_ObservationMasking_PreservesRecentRounds 验证批量观察遮蔽永不遮蔽最近 maskAfter 轮的结果，
// 且只有 currentRound - msgRound >= maskAfter 的旧结果才成为候选。
func TestP3_ObservationMasking_PreservesRecentRounds(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	agent := NewAgentWithDefaults("test-p3-masking")
	agent.sCtx.LoopObservationMaskAfterRounds = 6
	agent.sCtx.LoopObservationMaskBatch = 4

	// 前 4 轮输出已在之前批次被遮蔽
	for r := 1; r <= 4; r++ {
		agent.appendLoopMessages(types.Message{
			Role:    "user",
			Content: fmt.Sprintf("[output masked: full output: read_tool_ref(task_id=\"test\", id=\"ref-%d\")]", r),
			Round:   r,
			Parts: []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": fmt.Sprintf("call-%d", r),
					"content":     fmt.Sprintf("[output masked: full output: read_tool_ref(task_id=\"test\", id=\"ref-%d\")]", r),
				},
			},
		})
	}

	// 轮次 5, 6, 7 以及刚产生的第 8 轮未遮蔽输出
	for r := 5; r <= 8; r++ {
		agent.appendLoopMessages(types.Message{
			Role:    "user",
			Content: fmt.Sprintf("raw output of round %d", r),
			Round:   r,
			Parts: []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": fmt.Sprintf("call-%d", r),
					"content":     fmt.Sprintf("raw output of round %d", r),
				},
			},
		})
	}

	// 第 8 轮结束调用遮蔽逻辑
	agent.sCtx.LoopRound = 8
	agent.applyLoopObservationMasking(context.Background())

	agent.sCtx.Mu.Lock()
	msgs := make([]types.Message, len(agent.sCtx.LoopMessages))
	copy(msgs, agent.sCtx.LoopMessages)
	agent.sCtx.Mu.Unlock()

	// 最近 6 轮（轮次 5~8，尤其是刚产生的第 8 轮）绝对不可被遮蔽！
	for _, m := range msgs {
		if m.Round >= 5 {
			if strings.Contains(m.Content, "[output masked:") {
				t.Fatalf("违背 P3：最近 6 轮的消息（round=%d）被错误遮蔽！内容: %s", m.Round, m.Content)
			}
		}
	}
}

// TestP5_LoopTurnBudgets_USD_Tokens_WallSeconds 验证三种预算的生效逻辑：
// 触发后首轮注入告警，次轮未收尾硬熔断并 abortTurn。
func TestP5_LoopTurnBudgets_USD_Tokens_WallSeconds(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")

	t.Run("USD_Budget", func(t *testing.T) {
		agent := NewAgentWithDefaults("test-p5-usd")
		agent.InjectPolicyGate(&allowPolicyGate{})
		agent.sCtx.LoopTurnBudgetUSD = 0.50
		agent.sCtx.LoopLastUsageCost = 0.60
		agent.sCtx.LoopRound = 1

		// 首触：注入易变告警
		err := agent.runExecuteLoop(context.Background())
		if err != nil {
			t.Fatalf("首触预算上限不应直接失败，应给一轮缓冲: %v", err)
		}
		if !agent.sCtx.LoopWarningInjected {
			t.Fatal("首触 USD 预算上限必须将 LoopWarningInjected 置为 true")
		}

		// 次轮仍超限：硬熔断
		err = agent.runExecuteLoop(context.Background())
		if err == nil {
			t.Fatal("次轮未收尾必须返回硬熔断错误")
		}
		if !strings.Contains(err.Error(), "turn budget USD exceeded") {
			t.Fatalf("错误信息必须包含 turn budget USD exceeded: %v", err)
		}
	})

	t.Run("Tokens_Budget", func(t *testing.T) {
		agent := NewAgentWithDefaults("test-p5-tokens")
		agent.InjectPolicyGate(&allowPolicyGate{})
		agent.sCtx.LoopTurnBudgetTokens = 1000
		agent.sCtx.LoopLastUsageTokens = 1200
		agent.sCtx.LoopRound = 1

		err := agent.runExecuteLoop(context.Background())
		if err != nil {
			t.Fatalf("首触预算上限不应直接失败: %v", err)
		}
		if !agent.sCtx.LoopWarningInjected {
			t.Fatal("首触 Tokens 预算上限必须置位 LoopWarningInjected")
		}

		err = agent.runExecuteLoop(context.Background())
		if err == nil {
			t.Fatal("次轮仍超限必须硬熔断")
		}
		if !strings.Contains(err.Error(), "turn budget tokens exceeded") {
			t.Fatalf("错误信息必须包含 turn budget tokens exceeded: %v", err)
		}
	})

	t.Run("WallSeconds_Budget", func(t *testing.T) {
		agent := NewAgentWithDefaults("test-p5-wall")
		agent.InjectPolicyGate(&allowPolicyGate{})
		agent.sCtx.LoopTurnWallSeconds = 10
		agent.sCtx.LoopLastWallSeconds = 15
		agent.sCtx.LoopRound = 1

		err := agent.runExecuteLoop(context.Background())
		if err != nil {
			t.Fatalf("首触墙钟上限不应直接失败: %v", err)
		}
		if !agent.sCtx.LoopWarningInjected {
			t.Fatal("首触墙钟上限必须置位 LoopWarningInjected")
		}

		err = agent.runExecuteLoop(context.Background())
		if err == nil {
			t.Fatal("次轮仍超限必须硬熔断")
		}
		if !strings.Contains(err.Error(), "turn wall time exceeded") {
			t.Fatalf("错误信息必须包含 turn wall time exceeded: %v", err)
		}
	})
}

type imageReturningToolExecutor struct {
	images []types.ImagePart
}

func (e *imageReturningToolExecutor) Lookup(name string) (types.Tool, error) {
	return types.Tool{
		Name:       name,
		Source:     types.ToolBuiltin,
		Capability: types.CapReadOnly,
	}, nil
}

func (e *imageReturningToolExecutor) ExecuteWithTaint(_ context.Context, _ string, _ []byte, _ types.TaintLevel) (*types.ToolResult, error) {
	return &types.ToolResult{
		Success:    true,
		Output:     []byte(`{"result":"ok"}`),
		ImageParts: e.images,
	}, nil
}

// TestV10_ExecuteImageParts_LimitAndCleanup 验证单回合 ExecuteImageParts 上限为 10，
// 超限自动截断并在 handleTerminalState 时清理为 nil（V10 / 资源防泄漏）。
func TestV10_ExecuteImageParts_LimitAndCleanup(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	agent := NewAgentWithDefaults("test-v10-images")
	agent.InjectPolicyGate(&allowPolicyGate{})

	// 1. 测试单回合累积图片上限截断为 maxExecuteImageParts (10)
	exec := &imageReturningToolExecutor{
		images: []types.ImagePart{
			{URL: "img1"}, {URL: "img2"}, {URL: "img3"}, {URL: "img4"}, {URL: "img5"}, {URL: "img6"},
		},
	}
	agent.InjectToolExecutor(exec)

	// 第一批工具执行：注入 6 张图
	agent.sCtx.LoopPendingToolCalls = []types.InferToolCall{
		{ID: "call-1", Name: "image_tool", Input: []byte(`{}`)},
	}
	if err := agent.runExecuteLoop(context.Background()); err != nil {
		t.Fatalf("first runExecuteLoop failed: %v", err)
	}

	agent.sCtx.Mu.Lock()
	count1 := len(agent.sCtx.ExecuteImageParts)
	agent.sCtx.Mu.Unlock()
	if count1 != 6 {
		t.Fatalf("expected 6 images after first call, got %d", count1)
	}

	// 第二批工具执行：再注入 6 张图，总数应被截断为 maxExecuteImageParts (10)
	agent.sCtx.LoopPendingToolCalls = []types.InferToolCall{
		{ID: "call-2", Name: "image_tool", Input: []byte(`{}`)},
	}
	if err := agent.runExecuteLoop(context.Background()); err != nil {
		t.Fatalf("second runExecuteLoop failed: %v", err)
	}

	agent.sCtx.Mu.Lock()
	count2 := len(agent.sCtx.ExecuteImageParts)
	agent.sCtx.Mu.Unlock()
	if count2 != maxExecuteImageParts {
		t.Fatalf("expected %d images capped after second call, got %d", maxExecuteImageParts, count2)
	}

	// 2. 测试终态清理：进入终态后 ExecuteImageParts 必须置 nil
	agent.handleTerminalState(context.Background(), types.AgentStateComplete)

	agent.sCtx.Mu.Lock()
	afterTerminal := agent.sCtx.ExecuteImageParts
	agent.sCtx.Mu.Unlock()
	if afterTerminal != nil {
		t.Fatalf("expected ExecuteImageParts to be nil after handleTerminalState, got len=%d", len(afterTerminal))
	}
}
