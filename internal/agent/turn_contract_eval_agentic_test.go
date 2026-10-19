package agent

// 回合契约评测的 agentic 版（R5-06，HE-4）：默认模式已是 agentic，
// 原 staged 版 TestTurnContractEval_* 保留不动。每个场景同时断言 LLM 调用次数的上界，
// 作为 token 回归的门：agentic 不应因为实现变动悄悄多调模型。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

type agenticOutcome struct {
	reply   string
	errors  []string
	final   types.AgentState
	calls   int // LLM 调用次数
	reqs    [][]types.Message
	toolsOK bool
}

// runAgenticEval 以脚本化 Provider 跑一个 agentic 回合，收集用户可见输出与 LLM 调用次数。
func runAgenticEval(t *testing.T, responses []*types.ProviderResponse, gate protocol.PolicyGate, exec *mockToolExecutor, input string) agenticOutcome {
	t.Helper()
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	a := NewAgentWithDefaults("eval-agentic-" + t.Name())
	prov := &e2eContractProvider{responses: responses}
	a.InjectProvider(prov)
	a.InjectPolicyGate(gate)
	a.InjectToolExecutor(exec)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sub := a.SubscribeStream(ctx)
	done := make(chan struct{})
	go func() { _ = a.Run(ctx); close(done) }()

	a.SetTaskIntent(taint.NewTaintedString(input, taint.TaintSource{OriginTaintLevel: types.TaintHigh}, "eval"))
	if err := a.SendIntent(types.TriggerIntentReceived); err != nil {
		t.Fatalf("SendIntent: %v", err)
	}

	var out agenticOutcome
	for {
		select {
		case ev := <-sub:
			switch ev.Type {
			case types.AgentStreamEventToken:
				out.reply += ev.Content
			case types.AgentStreamEventError:
				out.errors = append(out.errors, ev.Content)
			case types.AgentStreamEventStatus:
				if ev.Content == "task_done" {
					<-done
					out.final = a.sm.Current()
					prov.mu.Lock()
					out.reqs = append(out.reqs, prov.requests...)
					out.calls = len(prov.requests)
					prov.mu.Unlock()
					out.toolsOK = exec.ExecuteWithTaintCalled
					return out
				}
			}
		case <-ctx.Done():
			t.Fatalf("回合未在期限内结束（reply=%q errors=%v）", out.reply, out.errors)
		}
	}
}

// toolResultOf 取出请求序列中某 tool_use_id 的 tool_result 文本。
func toolResultOf(reqs [][]types.Message, id string) string {
	for _, req := range reqs {
		for _, m := range req {
			for _, p := range m.Parts {
				if pm, ok := p.(map[string]any); ok && pm["type"] == "tool_result" && pm["tool_use_id"] == id {
					s, _ := pm["content"].(string)
					return s
				}
			}
		}
	}
	return ""
}

func assertAgenticNoArtifacts(t *testing.T, reply string) {
	t.Helper()
	for _, leak := range []string{`"tool_use"`, `"tool_use_id"`, `"nodes"`, "UNSCRIPTED_", "e2eContractProvider"} {
		if strings.Contains(reply, leak) {
			t.Errorf("用户回复泄露内部产物 %q：%q", leak, reply)
		}
	}
}

// 场景 A：纯对话直答——整回合 1 次 LLM，无工具。
func TestAgenticTurnContractEval_DirectReply(t *testing.T) {
	out := runAgenticEval(t, []*types.ProviderResponse{
		{Content: "你好！我是 Polaris。有什么可以帮你？"},
	}, &allowPolicyGate{}, &mockToolExecutor{}, "你好。你是谁？")

	if out.final != types.AgentStateComplete || out.reply != "你好！我是 Polaris。有什么可以帮你？" {
		t.Fatalf("final=%v reply=%q errors=%v", out.final, out.reply, out.errors)
	}
	if out.calls != 1 {
		t.Errorf("直答 LLM 调用次数 = %d，上界 1", out.calls)
	}
	if out.toolsOK {
		t.Error("直答不应执行任何工具")
	}
	assertAgenticNoArtifacts(t, out.reply)
}

// 场景 A-1：寒暄——agentic 没有 Perceive/Plan/Reflect 额外阶段，寒暄只允许 1 次 LLM
// （staged 版靠跳过 Perceive 达到同一上界）。
func TestAgenticTurnContractEval_PhaticSingleCall(t *testing.T) {
	out := runAgenticEval(t, []*types.ProviderResponse{
		{Content: "你好！有什么可以帮你？"},
	}, &allowPolicyGate{}, &mockToolExecutor{}, "你好呀")

	if out.final != types.AgentStateComplete || out.reply != "你好！有什么可以帮你？" {
		t.Fatalf("final=%v reply=%q errors=%v", out.final, out.reply, out.errors)
	}
	if out.calls != 1 {
		t.Errorf("寒暄 LLM 调用次数 = %d，上界 1", out.calls)
	}
	assertAgenticNoArtifacts(t, out.reply)
}

// 场景 C：工具路径——回复基于真实工具结果，结果以 tool_result 回到模型；整回合 2 次 LLM。
func TestAgenticTurnContractEval_ToolPathGroundedReply(t *testing.T) {
	exec := &mockToolExecutor{}
	out := runAgenticEval(t, []*types.ProviderResponse{
		{ToolCalls: []types.InferToolCall{{ID: "call_1", Name: "read_file", Input: json.RawMessage(`{"path":"README.md"}`)}}},
		{Content: "README 读取成功。"},
	}, &allowPolicyGate{}, exec, "读一下 README")

	if out.final != types.AgentStateComplete || out.reply != "README 读取成功。" {
		t.Fatalf("final=%v reply=%q errors=%v", out.final, out.reply, out.errors)
	}
	if !out.toolsOK {
		t.Fatal("工具应被真实执行")
	}
	if got := toolResultOf(out.reqs, "call_1"); !strings.Contains(got, `{"ok":true}`) {
		t.Errorf("第 2 轮请求必须带着工具真实输出，实际 tool_result=%q", got)
	}
	if out.calls != 2 {
		t.Errorf("单工具任务 LLM 调用次数 = %d，上界 2", out.calls)
	}
	assertAgenticNoArtifacts(t, out.reply)
}

// 场景 D：拒绝后的反馈回路——策略拒绝以 tool_result 回到模型，模型据此如实说明；
// 不空转重试（上界 2 次 LLM），工具不得被执行。
func TestAgenticTurnContractEval_RejectionFeedbackLoop(t *testing.T) {
	exec := &mockToolExecutor{}
	out := runAgenticEval(t, []*types.ProviderResponse{
		{ToolCalls: []types.InferToolCall{{ID: "call_1", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)}}},
		{Content: "出于安全策略，这次无法执行 shell 命令。"},
	}, &denyAllGate{}, exec, "帮我执行 ls")

	if out.final != types.AgentStateComplete || out.reply != "出于安全策略，这次无法执行 shell 命令。" {
		t.Fatalf("应以说明性回复收尾：final=%v reply=%q errors=%v", out.final, out.reply, out.errors)
	}
	if out.toolsOK {
		t.Error("被策略拒绝的工具不得执行")
	}
	if got := toolResultOf(out.reqs, "call_1"); !strings.Contains(got, "Forbidden") {
		t.Errorf("拒绝原因必须以 tool_result 回到模型，实际 %q", got)
	}
	if out.calls != 2 {
		t.Errorf("拒绝场景 LLM 调用次数 = %d，上界 2", out.calls)
	}
	assertAgenticNoArtifacts(t, out.reply)
}

// 场景 B：模型输出异常不外泄——模型调用了不存在的工具并给出不合 schema 的参数，
// 错误只以 tool_result 回给模型，用户回复里不得出现内部结构；整回合 2 次 LLM。
func TestAgenticTurnContractEval_MisbehavingOutputNeverLeaks(t *testing.T) {
	out := runAgenticEval(t, []*types.ProviderResponse{
		{Content: "我来处理。", ToolCalls: []types.InferToolCall{{ID: "call_bad", Name: "no_such_tool", Input: json.RawMessage(`{"unexpected_field":null}`)}}},
		{Content: "抱歉，这个操作无法完成。"},
	}, &denyAllGate{}, &mockToolExecutor{}, "帮我做点事")

	if out.final != types.AgentStateComplete {
		t.Fatalf("应以回复收尾：final=%v errors=%v", out.final, out.errors)
	}
	if !strings.HasSuffix(out.reply, "抱歉，这个操作无法完成。") {
		t.Errorf("回复应以模型的说明性文本收尾，实际 %q", out.reply)
	}
	if len(out.errors) != 0 {
		t.Errorf("异常输出不应变成面向用户的错误事件：%v", out.errors)
	}
	if out.calls > 2 {
		t.Errorf("LLM 调用次数 = %d，上界 2", out.calls)
	}
	assertAgenticNoArtifacts(t, out.reply)
}

// 场景 E：空输出重试——模型首轮既无正文也无工具调用（DeepSeek 思考模式偶发只出思考链），
// 不能把空回复交给用户；重试一次后正常完成，LLM 调用次数上界 2。
func TestAgenticTurnContractEval_EmptyOutputRetried(t *testing.T) {
	out := runAgenticEval(t, []*types.ProviderResponse{
		{Content: ""},
		{Content: "你好！我是 Polaris。"},
	}, &allowPolicyGate{}, &mockToolExecutor{}, "你是谁？")

	if out.final != types.AgentStateComplete || out.reply != "你好！我是 Polaris。" {
		t.Fatalf("空输出应重试后完成：final=%v reply=%q errors=%v", out.final, out.reply, out.errors)
	}
	if out.calls != 2 {
		t.Errorf("空输出应恰好重试一次，LLM 调用次数 = %d，期望 2", out.calls)
	}
}

// 场景 E-1：重试后仍为空且本回合从未产出正文——判失败并给出错误事件，
// 而不是以空回复"成功"收尾；LLM 调用次数上界 2（不无限重试）。
func TestAgenticTurnContractEval_EmptyOutputExhaustedFails(t *testing.T) {
	out := runAgenticEval(t, []*types.ProviderResponse{
		{Content: ""},
		{Content: ""},
	}, &allowPolicyGate{}, &mockToolExecutor{}, "你是谁？")

	if out.final != types.AgentStateFailed || out.reply != "" {
		t.Fatalf("两次空输出应判失败：final=%v reply=%q", out.final, out.reply)
	}
	if len(out.errors) == 0 {
		t.Error("失败必须有面向客户端的错误事件，否则用户只见空白")
	}
	if out.calls != 2 {
		t.Errorf("LLM 调用次数 = %d，上界 2", out.calls)
	}
}

// 场景 E-2：工具轮之后的空输出——此前轮次已有正文，沿用既有收尾，不把整回合判失败。
func TestAgenticTurnContractEval_EmptyAfterTextKeepsEarlierReply(t *testing.T) {
	out := runAgenticEval(t, []*types.ProviderResponse{
		{Content: "先读文件。", ToolCalls: []types.InferToolCall{{ID: "call_1", Name: "read_file", Input: json.RawMessage(`{"path":"a"}`)}}},
		{Content: ""},
		{Content: ""},
	}, &allowPolicyGate{}, &mockToolExecutor{}, "读文件")

	if out.final != types.AgentStateComplete || out.reply != "先读文件。" {
		t.Fatalf("已有正文时空轮不应判失败：final=%v reply=%q errors=%v", out.final, out.reply, out.errors)
	}
	if out.calls > 3 {
		t.Errorf("LLM 调用次数 = %d，上界 3", out.calls)
	}
}
