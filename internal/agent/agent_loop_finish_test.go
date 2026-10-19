package agent

import (
	"context"
	"strings"
	"testing"
	"time"

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
