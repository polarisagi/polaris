package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

// TestR2_01_AgenticLoopContract 验证 R2-01 规约：
// (a) 单回合 3 轮工具调用，每一轮请求里都含有本回合用户原问题，且只出现一次；
// (b) 同一会话连续两个回合，第 2 回合的请求含第 2 个问题，不含第 1 回合的工具往返消息；
// (c) 第 2 回合开始时 LoopRound==1，预算累计为 0；
// (d) 同一回合内，第 1 轮请求的前缀是第 2 轮请求前缀的字节前缀（T1/T2）。
func TestR2_01_AgenticLoopContract(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")

	userQ1 := "请帮我分析项目结构"
	userQ2 := "请帮我编写测试代码"

	prov := &loopTestProvider{
		responses: []*types.ProviderResponse{
			// 回合 1 - 第 1 轮：调用 tool_1
			{
				ToolCalls: []types.InferToolCall{
					{ID: "call_1", Name: "mock_tool", Input: []byte(`{"arg":"step1"}`)},
				},
				Usage: types.Usage{InputTokens: 100, OutputTokens: 50},
			},
			// 回合 1 - 第 2 轮：调用 tool_2
			{
				ToolCalls: []types.InferToolCall{
					{ID: "call_2", Name: "mock_tool", Input: []byte(`{"arg":"step2"}`)},
				},
				Usage: types.Usage{InputTokens: 150, OutputTokens: 60},
			},
			// 回合 1 - 第 3 轮：收尾文本
			{
				Content: "回合1完成：项目分析已就绪",
				Usage:   types.Usage{InputTokens: 200, OutputTokens: 70},
			},
			// 回合 2 - 第 1 轮：直接收尾
			{
				Content: "回合2完成：测试代码已编写",
				Usage:   types.Usage{InputTokens: 120, OutputTokens: 40},
			},
		},
	}

	agent := NewAgentWithDefaults("test-r2-01-session")
	agent.InjectProvider(prov)
	agent.InjectPolicyGate(&allowPolicyGate{})
	agent.InjectToolExecutor(&mockToolExecutor{})

	// 运行回合 1
	agent.SetTaskIntent(taint.NewTaintedString(userQ1, taint.TaintSource{OriginTaintLevel: types.TaintLow}, "test"))

	done1 := make(chan struct{})
	go func() {
		_ = agent.Run(context.Background())
		close(done1)
	}()

	if err := agent.SendIntent(types.TriggerIntentReceived); err != nil {
		t.Fatalf("回合 1 SendIntent 失败: %v", err)
	}

	select {
	case <-done1:
	case <-time.After(5 * time.Second):
		t.Fatal("回合 1 超时")
	}

	if st := agent.sm.Current(); st != types.AgentStateComplete {
		t.Fatalf("回合 1 期望状态 S_COMPLETE，实际: %v", st)
	}

	prov.mu.Lock()
	h1 := make([][]types.Message, len(prov.history))
	for i, h := range prov.history {
		h1[i] = make([]types.Message, len(h))
		copy(h1[i], h)
	}
	prov.mu.Unlock()

	if len(h1) < 3 {
		t.Fatalf("期望回合 1 记录 3 次 LLM 请求，实际收到: %d 次", len(h1))
	}

	// (a) 单回合 3 轮工具调用，每一轮请求里都含有本回合用户原问题，且只出现一次
	for roundIdx := 0; roundIdx < 3; roundIdx++ {
		msgs := h1[roundIdx]
		count := 0
		for _, m := range msgs {
			if strings.Contains(m.Content, userQ1) {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("断言 (a) 失败：回合 1 第 %d 轮请求中用户原问题出现次数不为 1 (实际 count=%d)", roundIdx+1, count)
		}
	}

	// (d) 同一回合内，第 1 轮请求的前缀是第 2 轮请求前缀的字节前缀（T1/T2）
	serializeMsgs := func(msgs []types.Message) string {
		var sb strings.Builder
		for _, m := range msgs {
			sb.WriteString(fmt.Sprintf("[%s]:%s\n", m.Role, m.Content))
		}
		return sb.String()
	}
	r1Str := serializeMsgs(h1[0])
	r2Str := serializeMsgs(h1[1])
	if !strings.HasPrefix(r2Str, r1Str) {
		t.Fatalf("断言 (d) 失败：第 1 轮请求不是第 2 轮请求的前缀\n--- R1 ---\n%s\n--- R2 ---\n%s", r1Str, r2Str)
	}

	// 运行回合 2（同一 Agent 实例复用）
	agent.SetTaskIntent(taint.NewTaintedString(userQ2, taint.TaintSource{OriginTaintLevel: types.TaintLow}, "test"))

	// 重置 FSM 回到 Idle 态以模拟下一回合到达
	agent.sm.ForceState(types.AgentStateIdle)

	done2 := make(chan struct{})
	go func() {
		_ = agent.Run(context.Background())
		close(done2)
	}()

	if err := agent.SendIntent(types.TriggerIntentReceived); err != nil {
		t.Fatalf("回合 2 SendIntent 失败: %v", err)
	}

	select {
	case <-done2:
	case <-time.After(5 * time.Second):
		t.Fatal("回合 2 超时")
	}

	prov.mu.Lock()
	totalReqs := len(prov.history)
	var h2 []types.Message
	if totalReqs > 3 {
		h2 = prov.history[3]
	}
	prov.mu.Unlock()

	if h2 == nil {
		t.Fatalf("回合 2 未收到 LLM 请求，总请求数: %d", totalReqs)
	}

	// (b) 同一会话连续两个回合，第 2 回合的请求含第 2 个问题，不含第 1 回合的工具往返消息
	hasQ2 := false
	hasStep1ToolResult := false
	for _, m := range h2 {
		if strings.Contains(m.Content, userQ2) {
			hasQ2 = true
		}
		if strings.Contains(m.Content, "call_1") || strings.Contains(m.Content, "step1") {
			hasStep1ToolResult = true
		}
	}
	if !hasQ2 {
		t.Fatalf("断言 (b) 失败：第 2 回合请求中未包含第 2 个问题 %q", userQ2)
	}
	if hasStep1ToolResult {
		t.Fatalf("断言 (b) 失败：第 2 回合请求泄漏了第 1 回合的中间工具往返内容")
	}

	// (c) 第 2 回合开始时 LoopRound==1，预算累计为 0
	agent.sCtx.Mu.RLock()
	curRound := agent.sCtx.LoopRound
	cost := agent.sCtx.LoopLastUsageCost
	agent.sCtx.Mu.RUnlock()

	// 回合 2 执行了 1 轮，但在开始前必须清零，回合结束后轮次应为 1
	if curRound != 1 {
		t.Fatalf("断言 (c) 失败：第 2 回合执行完后 LoopRound 期望为 1，实际: %d", curRound)
	}
	// 在回合 2 开始前预算清零，故回合 2 结束时 cost 应仅为回合 2 单轮的 cost (40 tokens ~ 0.0000)
	if cost > 0.001 {
		t.Fatalf("断言 (c) 失败：第 2 回合预算累计未清零，实际 cost: %f", cost)
	}
}
