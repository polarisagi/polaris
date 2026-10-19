package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

// TestAgenticFinish_ToolSchemaPresentWhenExplicit 验证只有在 explicit 模式下才在循环中下发 finish 工具 schema (R3-07)。
func TestAgenticFinish_ToolSchemaPresentWhenExplicit(t *testing.T) {
	tools := []types.ToolSchema{{Name: "read_file"}, {Name: "spawn_planner"}}
	agent := NewAgentWithDefaults("test-schema-finish")
	agent.catalog = &stubCatalog{schemas: tools}
	ctx := context.Background()

	// 1. auto 模式（默认）：不包含 finish，且已移除 spawn_planner，包含 code_act
	optsAuto := agent.toolInferOptions(ctx, types.AgentStateLoop, false)
	appliedAuto := applyOpts(optsAuto)
	for _, tc := range appliedAuto.Tools {
		if tc.Name == "finish" {
			t.Fatalf("auto 模式下不应下发 finish 工具: %+v", appliedAuto.Tools)
		}
	}

	// 2. explicit 模式：下发 finish 工具 schema
	agent.SetLoopCompletion("explicit")
	optsExplicit := agent.toolInferOptions(ctx, types.AgentStateLoop, false)
	appliedExplicit := applyOpts(optsExplicit)
	var finishSchema *types.ToolSchema
	for _, tc := range appliedExplicit.Tools {
		if tc.Name == "finish" {
			schemaCopy := tc
			finishSchema = &schemaCopy
			break
		}
	}
	if finishSchema == nil {
		t.Fatalf("explicit 模式下未找到 finish 工具 schema: %+v", appliedExplicit.Tools)
	}
	if finishSchema.Parameters == nil {
		t.Fatal("finish 工具 schema 缺少 parameters")
	}
	params, ok := finishSchema.Parameters.(map[string]any)
	if !ok {
		t.Fatalf("finish 工具 parameters 格式不符: %+v", finishSchema.Parameters)
	}
	props, ok := params["properties"].(map[string]any)
	if !ok || props["summary"] == nil {
		t.Fatalf("finish 工具 parameters 缺少 summary 字段: %+v", params)
	}
}

// TestAgenticFinish_ExplicitMode_CallsFinish_Succeeds 验证显式完成模式下模型调用 finish 工具能够顺利完成任务 (R3-07)。
func TestAgenticFinish_ExplicitMode_CallsFinish_Succeeds(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	agent := NewAgentWithDefaults("test-finish-call-success")
	agent.SetLoopCompletion("explicit")
	agent.InjectPolicyGate(&allowPolicyGate{})
	agent.InjectToolExecutor(&mockToolExecutor{})

	prov := &loopTestProvider{
		responses: []*types.ProviderResponse{
			{
				ToolCalls: []types.InferToolCall{
					{
						ID:    "call_finish_1",
						Name:  "finish",
						Input: []byte(`{"summary":"All subtasks completed successfully."}`),
					},
				},
			},
		},
	}
	agent.InjectProvider(prov)

	runAgenticTurnWait(t, agent)

	if st := agent.sm.Current(); st != types.AgentStateComplete {
		t.Fatalf("agent should complete, current state: %v", st)
	}
	if agent.sCtx.LoopSummary != "All subtasks completed successfully." {
		t.Fatalf("expected LoopSummary to be parsed, got %q", agent.sCtx.LoopSummary)
	}
}

// TestAgenticFinish_ExplicitMode_TwoConsecutiveTextRounds_Fallback 验证防浪费兜底：
// explicit 模式下，模型连续 2 轮只给文本、不调用 finish，以最后一段文本作为 summary 收尾，总轮数不超过 3 (R3-07)。
func TestAgenticFinish_ExplicitMode_TwoConsecutiveTextRounds_Fallback(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	agent := NewAgentWithDefaults("test-finish-text-fallback")
	agent.SetLoopCompletion("explicit")
	agent.InjectPolicyGate(&allowPolicyGate{})
	agent.InjectToolExecutor(&mockToolExecutor{})

	prov := &loopTestProvider{
		responses: []*types.ProviderResponse{
			// 第 1 轮：模型仅输出纯文本，未调用 finish
			{Content: "Working on the solution step 1..."},
			// 第 2 轮：模型收到系统提示要求 finish 后，仍仅输出纯文本
			{Content: "Final answer is 42."},
		},
	}
	agent.InjectProvider(prov)

	runAgenticTurnWait(t, agent)

	if st := agent.sm.Current(); st != types.AgentStateComplete {
		t.Fatalf("agent should complete after 2 consecutive text rounds, current state: %v", st)
	}
	// 断言总轮数不超过 3（此处恰好为 2 轮）
	if agent.sCtx.LoopRound > 3 {
		t.Fatalf("expected total rounds <= 3, got %d", agent.sCtx.LoopRound)
	}
	// 断言把最后一段文本当作 summary 收尾
	if agent.sCtx.LoopSummary != "Final answer is 42." {
		t.Fatalf("expected LoopSummary to be last text, got %q", agent.sCtx.LoopSummary)
	}

	// 验证第 1 轮与第 2 轮之间注入了系统提示
	foundSystemHint := false
	for _, m := range agent.sCtx.LoopMessages {
		if strings.Contains(m.Content, "系统提示：当前任务模式要求显式完成，请调用 finish 工具提供总结并结束任务。") {
			foundSystemHint = true
			break
		}
	}
	if !foundSystemHint {
		t.Fatalf("expected system hint in LoopMessages, got %+v", agent.sCtx.LoopMessages)
	}
}

// TestAgenticFinish_HeadlessDelegation_ProductionAssembly 验证生产装配下通过 AcquireHeadless 执行委派并显式完成 (R3-07)。
func TestAgenticFinish_HeadlessDelegation_ProductionAssembly(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")

	prov := &loopTestProvider{
		responses: []*types.ProviderResponse{
			{
				ToolCalls: []types.InferToolCall{
					{
						ID:    "call_finish_delegation",
						Name:  "finish",
						Input: []byte(`{"summary":"subagent task done"}`),
					},
				},
			},
		},
	}

	pool := NewPool(func(sid string) *Agent {
		a := NewAgentWithDefaults(sid)
		a.InjectProvider(prov)
		a.InjectPolicyGate(&allowPolicyGate{})
		a.InjectToolExecutor(&mockToolExecutor{})
		return a
	}, 2)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := pool.AcquireHeadless(ctx, types.Intent{Query: "subagent goal"}, types.WithLoopCompletion("explicit"))
	if err != nil {
		t.Fatalf("AcquireHeadless failed: %v", err)
	}
	if res.Output != "subagent task done" {
		t.Fatalf("expected output 'subagent task done', got %q", res.Output)
	}
}

// TestAgenticLoopCompletion_WebhookAuto_FinishesInSingleRound 验证 R4-04：
// webhook/auto 完成模式下，模型直接给出文本即结束，总共 1 次 LLM 调用。
func TestAgenticLoopCompletion_WebhookAuto_FinishesInSingleRound(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")

	prov := &loopTestProvider{
		responses: []*types.ProviderResponse{
			{Content: "你好，我是客服助手！"},
		},
	}

	pool := NewPool(func(sid string) *Agent {
		a := NewAgentWithDefaults(sid)
		a.InjectProvider(prov)
		a.InjectPolicyGate(&allowPolicyGate{})
		a.InjectToolExecutor(&mockToolExecutor{})
		return a
	}, 2)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 模拟 webhook 入站（auto 模式，不传 explicit）
	res, err := pool.AcquireHeadless(ctx, types.Intent{Query: "用户问候"}, types.WithLoopCompletion("auto"))
	if err != nil {
		t.Fatalf("AcquireHeadless failed: %v", err)
	}
	if res.Output != "你好，我是客服助手！" {
		t.Fatalf("expected output '你好，我是客服助手！', got %q", res.Output)
	}
	if prov.callCount() != 1 {
		t.Fatalf("auto 模式下直接给文本应在 1 轮内完成，实际调用次数: %d", prov.callCount())
	}
}

// TestAgenticLoopCompletion_CronThenInteractive_NoStickyExplicit 验证 R4-04：
// 同一会话先跑一次 cron 回合（explicit）、再跑一次交互回合，交互回合自动重置为 auto，不发生黏附。
func TestAgenticLoopCompletion_CronThenInteractive_NoStickyExplicit(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")

	prov := &loopTestProvider{
		responses: []*types.ProviderResponse{
			// 回合 1 (cron): 第 1 轮只给文本，不收尾；第 2 轮调用 finish 收尾
			{Content: "正在执行日常定时统计..."},
			{
				ToolCalls: []types.InferToolCall{
					{
						ID:    "call_cron_finish",
						Name:  "finish",
						Input: []byte(`{"summary":"cron job completed"}`),
					},
				},
			},
			// 回合 2 (交互): 第 1 轮只给文本，应当直接以 auto 模式收尾（不需要 finish）
			{Content: "这是交互式问答回复"},
		},
	}

	agent := NewAgentWithDefaults("test-sess-cron-then-interactive")
	agent.InjectProvider(prov)
	agent.InjectPolicyGate(&allowPolicyGate{})
	agent.InjectToolExecutor(&mockToolExecutor{})

	// 1. 回合 1：以 cron explicit 模式运行
	agent.SetLoopCompletion("explicit")
	agent.SetTaskIntent(taint.NewTaintedString("每日巡检", taint.TaintSource{OriginTaintLevel: types.TaintLow}, "test"))
	runAgenticTurnWait(t, agent)

	if agent.sCtx.LoopSummary != "cron job completed" {
		t.Fatalf("expected cron summary 'cron job completed', got %q", agent.sCtx.LoopSummary)
	}

	// 2. 回合 2：在同一 agent 实例上运行交互回合（不设 explicit，应自动重置为 auto）
	agent.sm.ForceState(types.AgentStateIdle)
	agent.SetTaskIntent(taint.NewTaintedString("用户交互咨询", taint.TaintSource{OriginTaintLevel: types.TaintLow}, "test"))
	runAgenticTurnWait(t, agent)

	// 断言第 2 回合完成，LoopCompletion 为 auto
	agent.sCtx.Mu.Lock()
	completionAfter := agent.sCtx.LoopCompletion
	var finalReply string
	for i := len(agent.sCtx.ConversationHistory) - 1; i >= 0; i-- {
		if agent.sCtx.ConversationHistory[i].Role == "assistant" {
			finalReply = agent.sCtx.ConversationHistory[i].Content
			break
		}
	}
	agent.sCtx.Mu.Unlock()

	if completionAfter != "auto" {
		t.Fatalf("交互回合应自动重置为 auto，实际为: %s", completionAfter)
	}
	if finalReply != "这是交互式问答回复" {
		t.Fatalf("预期回复 '这是交互式问答回复'，实际为: %s", finalReply)
	}
	if prov.callCount() != 3 {
		t.Fatalf("预期总共 3 次 LLM 调用（cron 2 次 + 交互 1 次），实际调用: %d", prov.callCount())
	}
}
