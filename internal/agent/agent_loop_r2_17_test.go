package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/agent/fsm"
)

// TestR2_17_RunawayGuard_VolatileUserPrompt 验证失控保护告警使用 user 角色且位于易变区，不污染历史。
func TestR2_17_RunawayGuard_VolatileUserPrompt(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	agent := NewAgentWithDefaults("test-r2-17-runaway")
	agent.InjectPolicyGate(&allowPolicyGate{})
	agent.sCtx.LoopRunawayLimit = 2
	agent.sCtx.LoopRound = 2

	err := agent.runExecuteLoop(context.Background())
	if err != nil {
		t.Fatalf("首触失控保护上限不应直接报错，应缓冲一轮: %v", err)
	}

	agent.sCtx.Mu.Lock()
	warned := agent.sCtx.LoopWarningInjected
	warnPrompt := agent.sCtx.LoopWarningPrompt
	loopMsgs := agent.sCtx.LoopMessages
	agent.sCtx.Mu.Unlock()

	if !warned {
		t.Fatal("首触上限必须置位 LoopWarningInjected")
	}
	if !strings.Contains(warnPrompt, "失控保护告警") {
		t.Fatalf("LoopWarningPrompt 必须包含失控保护告警，实际: %q", warnPrompt)
	}

	// 历史消息中严禁出现任何 Role: "system" 消息（防破坏 Anthropic 与前缀缓存，R2-17）
	for _, m := range loopMsgs {
		if m.Role == "system" {
			t.Fatalf("LoopMessages 中严禁追加 system 角色消息: %+v", m)
		}
	}

	// 验证 BuildAgenticPrompt 在易变区输出 user 角色的收尾告警
	msgs, err := fsm.BuildAgenticPrompt(context.Background(), nil, agent.sCtx, loopMsgs, "[usage] tokens=100")
	if err != nil {
		t.Fatalf("BuildAgenticPrompt failed: %v", err)
	}

	var foundUserWarning bool
	for _, m := range msgs {
		if m.Role == "user" && strings.Contains(m.Content, "失控保护告警") {
			foundUserWarning = true
			break
		}
	}
	if !foundUserWarning {
		t.Fatal("BuildAgenticPrompt 易变区中必须包含 user 角色的失控保护告警消息")
	}
}

// TestR2_17_BudgetGuard_VolatileUserPrompt 验证预算收尾提示使用 user 角色易变区（R2-17）。
func TestR2_17_BudgetGuard_VolatileUserPrompt(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	agent := NewAgentWithDefaults("test-r2-17-budget")
	agent.InjectPolicyGate(&allowPolicyGate{})
	agent.sCtx.LoopTurnBudgetUSD = 0.50
	agent.sCtx.LoopLastUsageCost = 0.60
	agent.sCtx.LoopRound = 1

	err := agent.runExecuteLoop(context.Background())
	if err != nil {
		t.Fatalf("首触预算上限不应报错: %v", err)
	}

	agent.sCtx.Mu.Lock()
	warnPrompt := agent.sCtx.LoopWarningPrompt
	loopMsgs := agent.sCtx.LoopMessages
	agent.sCtx.Mu.Unlock()

	if !strings.Contains(warnPrompt, "turn budget USD exceeded") {
		t.Fatalf("LoopWarningPrompt 必须记录预算超限告警: %q", warnPrompt)
	}

	for _, m := range loopMsgs {
		if m.Role == "system" {
			t.Fatalf("LoopMessages 中严禁追加 system 角色消息: %+v", m)
		}
	}
}
