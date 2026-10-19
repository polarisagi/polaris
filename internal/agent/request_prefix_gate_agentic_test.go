package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/memory/store"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
	"github.com/stretchr/testify/require"
)

// TestRequestPrefixGate_Agentic5Rounds 验证 R3-01：
// 模拟 5 轮 agentic 循环，每轮计量行与时间均不同。
// 断言第 k 轮请求去掉最后一条末尾易变消息后，是第 k+1 轮请求的严格字节前缀。
// 并验证缓存断点落在 L4 末尾之前的稳定位置。
func TestRequestPrefixGate_Agentic5Rounds(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	setGateThresholds(t, func(th *config.Thresholds) {
		th.M4Kernel.LoopMode = "agentic"
		th.M4Kernel.LoopRunawayGuardRounds = 20
		th.M4Kernel.PromptPhaseContractsInCore = true
	})

	prov := &loopTestProvider{
		responses: []*types.ProviderResponse{
			// Round 1 -> call tool 1
			{
				ToolCalls: []types.InferToolCall{
					{ID: "call_1", Name: "mock_tool", Input: []byte(`{"doc":"1"}`)},
				},
				Usage: types.Usage{InputTokens: 100, OutputTokens: 50},
			},
			// Round 2 -> call tool 2
			{
				ToolCalls: []types.InferToolCall{
					{ID: "call_2", Name: "mock_tool", Input: []byte(`{"doc":"2"}`)},
				},
				Usage: types.Usage{InputTokens: 250, OutputTokens: 60},
			},
			// Round 3 -> call tool 3
			{
				ToolCalls: []types.InferToolCall{
					{ID: "call_3", Name: "mock_tool", Input: []byte(`{"doc":"3"}`)},
				},
				Usage: types.Usage{InputTokens: 400, OutputTokens: 70},
			},
			// Round 4 -> call tool 4
			{
				ToolCalls: []types.InferToolCall{
					{ID: "call_4", Name: "mock_tool", Input: []byte(`{"doc":"4"}`)},
				},
				Usage: types.Usage{InputTokens: 550, OutputTokens: 80},
			},
			// Round 5 -> final completion
			{
				Content: "已顺序处理完全部 4 份文档",
				Usage:   types.Usage{InputTokens: 700, OutputTokens: 90},
			},
		},
	}

	agent := NewAgentWithDefaults("prefix-session-5rounds")
	agent.InjectProvider(prov)
	agent.InjectPolicyGate(&allowPolicyGate{})
	agent.InjectToolExecutor(&mockToolExecutor{})

	// 装配真实 ImmutableCore（含可变日期时间等 volatile 内容）与 Memory
	core := store.NewImmutableCore()
	core.SoulMDContent = "You are the gate-test persona."
	core.ModelGuidance = "Prefer tool calls over guessing."
	core.PlatformHint = "Platform: test harness."
	core.BuiltinTools = "mock_tool"
	core.VolatileBlock = "日期 2026-10-19 18:00:00"

	mem := &gateMemory{
		mockMemoryForIntegration: &mockMemoryForIntegration{},
		core:                     core,
		blocks: []types.CoreMemoryBlock{
			{BlockKey: "project_goal", Content: "Test prefix caching over 5 rounds", TaintLevel: types.TaintNone},
		},
	}
	agent.InjectMemory(mem)

	userQ := "请按顺序处理 4 份文档"
	agent.SetTaskIntent(taint.NewTaintedString(userQ, taint.TaintSource{OriginTaintLevel: types.TaintLow}, "test"))

	done := make(chan struct{})
	go func() {
		_ = agent.Run(context.Background())
		close(done)
	}()

	require.NoError(t, agent.SendIntent(types.TriggerIntentReceived))

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("5 轮 agentic 循环执行超时")
	}

	prov.mu.Lock()
	history := prov.history
	prov.mu.Unlock()

	require.Equal(t, 5, len(history), "期望恰好收到 5 轮 LLM 请求")

	serialize := func(msgs []types.Message) string {
		var sb strings.Builder
		for _, m := range msgs {
			fmt.Fprintf(&sb, "<role:%s taint:%v bp:%v>\n%s\n</role>\n", m.Role, m.TaintLevel, m.CacheBreakpoint, m.Content)
		}
		return sb.String()
	}

	// 断言：每一轮的最后一条消息必须是末尾易变 user 消息（包含 [usage] 计量行）
	// 去掉最后一条消息后，第 k 轮必须是第 k+1 轮的严格字节前缀（T1 前缀稳定）
	for k := 0; k < 4; k++ {
		curReq := history[k]
		nextReq := history[k+1]

		require.True(t, len(curReq) > 1, "第 %d 轮请求消息数应 > 1", k+1)
		lastMsg := curReq[len(curReq)-1]
		require.Equal(t, "user", lastMsg.Role, "第 %d 轮末尾易变块应为 user 角色", k+1)
		require.False(t, lastMsg.CacheBreakpoint, "末尾易变消息不得携带 CacheBreakpoint")
		if k > 0 {
			require.Contains(t, lastMsg.Content, "[usage]", "第 %d 轮末尾易变块应包含 T7 计量行", k+1)
		}

		stableCur := serialize(curReq[:len(curReq)-1])
		fullNext := serialize(nextReq)

		require.True(t, strings.HasPrefix(fullNext, stableCur),
			"R3-01 失败：第 %d 轮（去掉末尾易变消息后）不是第 %d 轮请求的严格字节前缀！\n--- Round %d (without trailing volatile) ---\n%s\n--- Round %d ---\n%s",
			k+1, k+2, k+1, stableCur, k+2, fullNext)
	}

	// 验证缓存断点 CacheBreakpoint 落在 L4 末尾之前的稳定位置
	req5 := history[4]
	require.True(t, len(req5) > 1)
	lastReqMsg := req5[len(req5)-1]
	require.False(t, lastReqMsg.CacheBreakpoint, "末尾易变块不得携带 CacheBreakpoint")

	hasStableBreakpoint := false
	for i := 0; i < len(req5)-1; i++ {
		if req5[i].CacheBreakpoint {
			hasStableBreakpoint = true
			break
		}
	}
	require.True(t, hasStableBreakpoint, "L4 末尾之前必须有稳定的 CacheBreakpoint")
}
