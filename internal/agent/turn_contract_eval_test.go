package agent

// 回合契约评测（ADR-0098，R3-HE4）：用脚本化 Provider 驱动完整 FSM 回合，把
// "用户最终看到什么"作为可回归的评测断言。阶段契约模板（kernel/*.md）与 Respond
// 路由的任何改动都必须保持这些场景全绿——它们是 2026-09-24 回复夹带 DAG JSON
// 缺陷及其后续工具路径缺陷的最小复现集。

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

// 阶段识别依据 kernel/*.md 标题：评测锁的正是"阶段契约来自模板"这件事。
var phaseMarkers = []struct{ marker, phase string }{
	{"# TASK PERCEPTION", "perceive"},
	{"# DAG EXECUTION PLANNING", "plan"},
	{"# EXECUTION REFLECTION", "reflect"},
	{"# REPLY TO THE USER", "respond"},
}

type scriptedReply struct {
	content   string
	toolCalls []types.InferToolCall
}

// scriptedTurnProvider 按阶段返回预设输出，并记录每个阶段收到的 prompt。
type scriptedTurnProvider struct {
	mu      sync.Mutex
	script  map[string][]scriptedReply // phase → 按调用次序出队
	prompts map[string][]string
	pools   map[string][]string // phase → 每次调用请求的 ModelPool（ADR-0101 决策六评测）
}

func newScriptedTurnProvider(script map[string][]scriptedReply) *scriptedTurnProvider {
	return &scriptedTurnProvider{script: script, prompts: map[string][]string{}, pools: map[string][]string{}}
}

func (p *scriptedTurnProvider) next(msgs []types.Message, opts ...types.InferOption) scriptedReply {
	var all strings.Builder
	for _, m := range msgs {
		all.WriteString(m.Content + "\n")
	}
	phase := "unknown"
	for _, pm := range phaseMarkers {
		if strings.Contains(all.String(), pm.marker) {
			phase = pm.phase
			break
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prompts[phase] = append(p.prompts[phase], all.String())
	var o types.InferOptions
	for _, opt := range opts {
		opt(&o)
	}
	p.pools[phase] = append(p.pools[phase], o.ModelPool)
	q := p.script[phase]
	if len(q) == 0 {
		return scriptedReply{content: "UNSCRIPTED_" + phase}
	}
	r := q[0]
	if len(q) > 1 {
		p.script[phase] = q[1:]
	}
	return r
}

func (p *scriptedTurnProvider) Infer(_ context.Context, msgs []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
	r := p.next(msgs, opts...)
	return &types.ProviderResponse{Content: r.content, ToolCalls: r.toolCalls}, nil
}

// StreamInfer 把正文切成多帧并附带思考链，贴近真实流式形态。
func (p *scriptedTurnProvider) StreamInfer(_ context.Context, msgs []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	r := p.next(msgs, opts...)
	ch := make(chan types.StreamEvent, len(r.content)+len(r.toolCalls)+2)
	ch <- types.StreamEvent{Type: types.StreamThinking, Content: "思考中"}
	for _, chunk := range strings.SplitAfter(r.content, "。") {
		if chunk != "" {
			ch <- types.StreamEvent{Type: types.StreamTextDelta, Content: chunk}
		}
	}
	for _, tc := range r.toolCalls {
		// 与真实适配器同形（stream.go emitCollectedToolCalls 以 RawMessage 承载参数）。
		payload, _ := json.Marshal(map[string]any{"id": tc.ID, "name": tc.Name, "input": json.RawMessage(tc.Input)})
		ch <- types.StreamEvent{Type: types.StreamToolCall, Content: string(payload)}
	}
	close(ch)
	return ch, nil
}

func (p *scriptedTurnProvider) Capabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{}
}
func (p *scriptedTurnProvider) Tokenizer() protocol.TokenizerAdapter { return nil }
func (p *scriptedTurnProvider) ModelID() string                      { return "scripted" }

func (p *scriptedTurnProvider) promptsOf(phase string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.prompts[phase]...)
}

// pinSystem2Routing 把进程级 GlobalSurpriseIndex 固定在 System-2 区间，测试结束恢复原值。
// fsm S_PERCEIVE→S_PLAN 读该全局值决定是否走 System-1 旁路（复用已有 DAGModel、跳过 LLM
// 规划）；同包其它测试（SetTaskIntent 等）会把它改低，依赖完整规划链路的用例在
// -shuffle 下随执行顺序时过时挂。
func pinSystem2Routing(t *testing.T) {
	t.Helper()
	prev := metrics.GlobalSurpriseIndex().Current()
	metrics.GlobalSurpriseIndex().SetLastValue(0.7)
	t.Cleanup(func() { metrics.GlobalSurpriseIndex().SetLastValue(prev) })
}

type fixedSurprise float64

func (f fixedSurprise) SubmitToolSeq(string, []string) {}
func (f fixedSurprise) CurrentSurprise() float64       { return float64(f) }

type turnOutcome struct {
	reply    string
	phases   []string
	errors   []string
	final    types.AgentState
	degraded bool
}

func runScriptedTurn(t *testing.T, p *scriptedTurnProvider, gate protocol.PolicyGate, exec *mockToolExecutor, input string) turnOutcome {
	t.Helper()
	a := NewAgentWithDefaults("eval-" + t.Name())
	// 固定 System-2 路由：评测验证的是完整规划链路。
	pinSystem2Routing(t)
	a.surpriseCalc = fixedSurprise(0.7)
	a.InjectProvider(p)
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

	var out turnOutcome
	for {
		select {
		case ev := <-sub:
			switch ev.Type {
			case types.AgentStreamEventToken:
				out.reply += ev.Content
			case types.AgentStreamEventPhase:
				out.phases = append(out.phases, ev.Content)
			case types.AgentStreamEventError:
				out.errors = append(out.errors, ev.Content)
			case types.AgentStreamEventStatus:
				if ev.Content == "task_done" {
					<-done
					out.final = a.sm.Current()
					out.degraded = a.sCtx.TurnDegraded
					return out
				}
			}
		case <-ctx.Done():
			t.Fatalf("回合未在期限内结束（phases=%v reply=%q）", out.phases, out.reply)
		}
	}
}

// assertNoInternalArtifacts 用户可见输出不得出现任何内部阶段产物。
func assertNoInternalArtifacts(t *testing.T, reply string) {
	t.Helper()
	for _, leak := range []string{"```", `"nodes"`, `"Goal"`, `"NeedsTools"`, `"GoalAchieved"`, "UNSCRIPTED_"} {
		if strings.Contains(reply, leak) {
			t.Errorf("用户回复泄露内部产物 %q：%q", leak, reply)
		}
	}
}

// 场景 A：纯对话直答——Perceive 判定无需工具，只经 S_RESPOND 回复。
func TestTurnContractEval_DirectReply(t *testing.T) {
	p := newScriptedTurnProvider(map[string][]scriptedReply{
		"perceive": {{content: `{"Goal":"用户打招呼并询问身份","NeedsTools":false}`}},
		"respond":  {{content: "你好！我是 Polaris。有什么可以帮你？"}},
	})
	out := runScriptedTurn(t, p, &allowPolicyGate{}, &mockToolExecutor{}, "你好。你是谁？")

	if out.final != types.AgentStateComplete || out.reply != "你好！我是 Polaris。有什么可以帮你？" {
		t.Fatalf("final=%v reply=%q errors=%v", out.final, out.reply, out.errors)
	}
	if strings.Join(out.phases, ",") != "perceive,respond" {
		t.Errorf("直答应只经 perceive,respond，实际 %v", out.phases)
	}
	if len(p.promptsOf("plan")) != 0 {
		t.Error("直答不应调用规划阶段")
	}
	assertNoInternalArtifacts(t, out.reply)
}

// 场景 A-1：寒暄零 LLM 感知（ADR-0101 决策一）——只调一次 Respond，Perceive/Plan 零调用。
func TestTurnContractEval_PhaticSkipsPerceive(t *testing.T) {
	p := newScriptedTurnProvider(map[string][]scriptedReply{
		"respond": {{content: "你好！有什么可以帮你？"}},
	})
	out := runScriptedTurn(t, p, &allowPolicyGate{}, &mockToolExecutor{}, "你好呀")

	if out.final != types.AgentStateComplete || out.reply != "你好！有什么可以帮你？" {
		t.Fatalf("final=%v reply=%q errors=%v", out.final, out.reply, out.errors)
	}
	if n := len(p.promptsOf("perceive")); n != 0 {
		t.Errorf("寒暄不应调用 Perceive LLM，实际 %d 次", n)
	}
	if n := len(p.promptsOf("plan")); n != 0 {
		t.Errorf("寒暄不应调用规划阶段，实际 %d 次", n)
	}
	if n := len(p.promptsOf("respond")); n != 1 {
		t.Errorf("寒暄应恰好一次 Respond，实际 %d 次", n)
	}
	assertNoInternalArtifacts(t, out.reply)
}

// 场景 A-2：直答合并（ADR-0101 决策四 4b′）——Perceive 同次产出回复，整回合只调 1 次 LLM。
func TestTurnContractEval_DirectReplyMergedIntoPerceive(t *testing.T) {
	p := newScriptedTurnProvider(map[string][]scriptedReply{
		"perceive": {{content: `{"Goal":"询问身份","NeedsTools":false,"Reply":"我是 Polaris，你的 AI 助手。"}`}},
	})
	out := runScriptedTurn(t, p, &allowPolicyGate{}, &mockToolExecutor{}, "你是谁？")

	if out.final != types.AgentStateComplete || out.reply != "我是 Polaris，你的 AI 助手。" {
		t.Fatalf("final=%v reply=%q errors=%v", out.final, out.reply, out.errors)
	}
	if n := len(p.promptsOf("respond")); n != 0 {
		t.Errorf("Perceive 已产出回复时不应再调 Respond LLM，实际 %d 次", n)
	}
	if strings.Join(out.phases, ",") != "perceive,respond" {
		t.Errorf("phases = %v, want perceive,respond", out.phases)
	}
	assertNoInternalArtifacts(t, out.reply)
}

// 场景 B：原始缺陷形态——规划阶段模型输出"散文 + 围栏 JSON + 散文"。
// 修复前这段内容被逐 token 推给用户，且回合以 S_FAILED 结束。
func TestTurnContractEval_MisbehavingPlanNeverLeaks(t *testing.T) {
	p := newScriptedTurnProvider(map[string][]scriptedReply{
		"perceive": {{content: `{"Goal":"自我介绍","NeedsTools":true}`}},
		"plan":     {{content: "我是 Polaris，我可以调用```json\n{\"nodes\":[],\"edges\":[]}\n```\n你好！我是 Polaris。"}},
		"respond":  {{content: "你好！我是 Polaris。"}},
	})
	out := runScriptedTurn(t, p, &allowPolicyGate{}, &mockToolExecutor{}, "你好。你是谁？")

	if out.final != types.AgentStateComplete || out.reply != "你好！我是 Polaris。" {
		t.Fatalf("final=%v reply=%q errors=%v", out.final, out.reply, out.errors)
	}
	assertNoInternalArtifacts(t, out.reply)
}

// 场景 C：工具路径——回复基于真实执行结果，且执行结果进入回复阶段 prompt。
func TestTurnContractEval_ToolPathGroundedReply(t *testing.T) {
	exec := &mockToolExecutor{}
	p := newScriptedTurnProvider(map[string][]scriptedReply{
		"perceive": {{content: `{"Goal":"读取 README","NeedsTools":true}`}},
		"plan": {{toolCalls: []types.InferToolCall{
			{ID: "call_1", Name: "read_file", Input: json.RawMessage(`{"path":"README.md"}`)},
		}}},
		"reflect": {{content: `{"GoalAchieved":true,"Errors":[],"Learnings":[]}`}},
		"respond": {{content: "README 读取成功。"}},
	})
	out := runScriptedTurn(t, p, &allowPolicyGate{}, exec, "读一下 README")

	if out.final != types.AgentStateComplete || out.reply != "README 读取成功。" {
		t.Fatalf("final=%v reply=%q errors=%v", out.final, out.reply, out.errors)
	}
	if !exec.ExecuteWithTaintCalled {
		t.Fatal("工具应被真实执行")
	}
	if want := "perceive,plan,execute,reflect,respond"; strings.Join(out.phases, ",") != want {
		t.Errorf("phases = %v, want %s", out.phases, want)
	}
	if rp := p.promptsOf("respond"); len(rp) != 1 || !strings.Contains(rp[0], "<observations>") {
		t.Error("回复阶段必须看到执行结果，否则回复无从基于事实")
	}
	assertNoInternalArtifacts(t, out.reply)
}

// 场景 C-1：简单工具任务首轮全部成功 → 跳过 Reflect LLM（ADR-0101 决策四），
// 回复仍基于执行结果。
func TestTurnContractEval_SimpleToolTaskSkipsReflect(t *testing.T) {
	exec := &mockToolExecutor{}
	p := newScriptedTurnProvider(map[string][]scriptedReply{
		"perceive": {{content: `{"Goal":"读取 README","NeedsTools":true,"Complexity":0.2}`}},
		"plan": {{toolCalls: []types.InferToolCall{
			{ID: "call_1", Name: "read_file", Input: json.RawMessage(`{"path":"README.md"}`)},
		}}},
		"respond": {{content: "README 读取成功。"}},
	})
	out := runScriptedTurn(t, p, &allowPolicyGate{}, exec, "读一下 README")

	if out.final != types.AgentStateComplete || out.reply != "README 读取成功。" {
		t.Fatalf("final=%v reply=%q errors=%v", out.final, out.reply, out.errors)
	}
	if n := len(p.promptsOf("reflect")); n != 0 {
		t.Errorf("简单任务成功应跳过 Reflect LLM，实际调用 %d 次", n)
	}
	if rp := p.promptsOf("respond"); len(rp) != 1 || !strings.Contains(rp[0], "<observations>") {
		t.Error("跳过反思后回复阶段仍必须看到执行结果")
	}
	assertNoInternalArtifacts(t, out.reply)
}

// 场景 D：重规划闭环——计划被安全闸门拒绝后，原因回灌规划与回复；无允许方案时
// 空计划转直答说明限制，而非耗尽重试后报错（ADR-0098 决策六）。
func TestTurnContractEval_RejectionFeedbackLoop(t *testing.T) {
	p := newScriptedTurnProvider(map[string][]scriptedReply{
		"perceive": {{content: `{"Goal":"执行 shell 命令","NeedsTools":true}`}},
		"plan": {
			{toolCalls: []types.InferToolCall{{ID: "call_1", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)}}},
			{content: `{"nodes":[],"edges":[]}`},
		},
		"respond": {{content: "出于安全策略，这次无法执行 shell 命令。"}},
	})
	out := runScriptedTurn(t, p, &denyAllGate{}, &mockToolExecutor{}, "帮我执行 ls")

	if out.final != types.AgentStateComplete {
		t.Fatalf("应以回复收尾而非失败：final=%v errors=%v", out.final, out.errors)
	}
	plans := p.promptsOf("plan")
	if len(plans) != 2 || !strings.Contains(plans[1], "previous_attempts_failed") || !strings.Contains(plans[1], `"bash"`) {
		t.Fatalf("重规划 prompt 必须携带被拒工具与原因（共 %d 次规划）", len(plans))
	}
	if rp := p.promptsOf("respond"); len(rp) != 1 || !strings.Contains(rp[0], "previous_attempts_failed") {
		t.Error("回复阶段必须看到被拒原因，才能如实说明限制")
	}
	// ADR-0101 决策六：安全拒绝换更贵的模型也照样被拒，重规划不得升级到 reasoning 池。
	p.mu.Lock()
	pools := append([]string(nil), p.pools["plan"]...)
	p.mu.Unlock()
	for i, pool := range pools {
		if pool != string(types.ModelPoolGeneral) {
			t.Errorf("第 %d 次规划池 = %q，安全拒绝后不应升级", i+1, pool)
		}
	}
	assertNoInternalArtifacts(t, out.reply)
}

// 场景 E：规划阶段偶发只输出思考链（既无正文也无工具调用），自环重试后正常完成
// （ADR-0098 决策七；2026-09-25 DeepSeek 思考模式挂载工具时实测）。
func TestTurnContractEval_EmptyPlanOutputRetried(t *testing.T) {
	exec := &mockToolExecutor{}
	p := newScriptedTurnProvider(map[string][]scriptedReply{
		"perceive": {{content: `{"Goal":"读取 README","NeedsTools":true}`}},
		"plan": {
			{content: ""},
			{toolCalls: []types.InferToolCall{{ID: "call_1", Name: "read_file", Input: json.RawMessage(`{"path":"README.md"}`)}}},
		},
		"reflect": {{content: `{"GoalAchieved":true,"Errors":[],"Learnings":[]}`}},
		"respond": {{content: "README 读取成功。"}},
	})
	out := runScriptedTurn(t, p, &allowPolicyGate{}, exec, "读一下 README")

	if out.final != types.AgentStateComplete || out.reply != "README 读取成功。" || !exec.ExecuteWithTaintCalled {
		t.Fatalf("final=%v reply=%q executed=%v errors=%v", out.final, out.reply, exec.ExecuteWithTaintCalled, out.errors)
	}
	if n := len(p.promptsOf("plan")); n != 2 {
		t.Fatalf("空输出应恰好重试一次，实际规划 %d 次", n)
	}
}

// 场景 F：观察—再规划循环（ADR-0098 决策八）——首轮结果不足、反思显式判定未达成，
// 第二轮规划看得到首轮观察，回复据全部观察作答。
func TestTurnContractEval_ObservationLoop(t *testing.T) {
	exec := &mockToolExecutor{}
	p := newScriptedTurnProvider(map[string][]scriptedReply{
		"perceive": {{content: `{"Goal":"查内存和 CPU 型号","NeedsTools":true}`}},
		"plan": {
			{toolCalls: []types.InferToolCall{{ID: "call_1", Name: "sys_probe", Input: json.RawMessage(`{}`)}}},
			{toolCalls: []types.InferToolCall{{ID: "call_2", Name: "read_file", Input: json.RawMessage(`{"path":"cpuinfo"}`)}}},
		},
		"reflect": {
			{content: `{"GoalAchieved":false,"Errors":["缺少 CPU 型号"],"Learnings":[]}`},
			{content: `{"GoalAchieved":true,"Errors":[],"Learnings":[]}`},
		},
		"respond": {{content: "内存 16GB，CPU 为 Apple M 系列。"}},
	})
	out := runScriptedTurn(t, p, &allowPolicyGate{}, exec, "查一下内存和 CPU 型号")

	if out.final != types.AgentStateComplete || out.degraded {
		t.Fatalf("final=%v degraded=%v errors=%v", out.final, out.degraded, out.errors)
	}
	plans := p.promptsOf("plan")
	if len(plans) != 2 || !strings.Contains(plans[1], "<observations>") || !strings.Contains(plans[1], "缺少 CPU 型号") {
		t.Fatalf("第二轮规划必须看到首轮观察与未达成原因（规划 %d 次）", len(plans))
	}
	if rp := p.promptsOf("respond"); len(rp) != 1 || strings.Count(rp[0], "[round ") != 2 {
		t.Error("回复阶段应看到两轮观察")
	}
	assertNoInternalArtifacts(t, out.reply)
}

// 场景 G：重规划耗尽转回复（ADR-0098 决策九）——模型反复选择被拒工具，回合以
// 说明性回复结束而非技术报错，且标记为降级（终态指标按失败计）。
func TestTurnContractEval_ReplanExhaustedReplies(t *testing.T) {
	bash := scriptedReply{toolCalls: []types.InferToolCall{{ID: "c", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)}}}
	p := newScriptedTurnProvider(map[string][]scriptedReply{
		"perceive": {{content: `{"Goal":"执行 ls","NeedsTools":true}`}},
		"plan":     {bash},
		"respond":  {{content: "多次尝试均被安全策略拦截，未能执行。"}},
	})
	out := runScriptedTurn(t, p, &denyAllGate{}, &mockToolExecutor{}, "执行 ls")

	if out.final != types.AgentStateComplete || !out.degraded || len(out.errors) != 0 {
		t.Fatalf("应以说明性回复收尾并标记降级：final=%v degraded=%v errors=%v", out.final, out.degraded, out.errors)
	}
	if out.reply != "多次尝试均被安全策略拦截，未能执行。" {
		t.Fatalf("reply=%q", out.reply)
	}
	if rp := p.promptsOf("respond"); len(rp) != 1 || !strings.Contains(rp[0], "replan budget exhausted") {
		t.Error("回复阶段必须知道预算已耗尽，才能如实说明")
	}
}
