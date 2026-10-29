package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/types"
)

// TestAgenticLoop_NotTruncatedByMaxSteps 回归：agentic 回合每轮工具调用耗 2 次 FSM 迁移，
// 15 轮即超 24 步，不得再报 MAX_STEPS_EXCEEDED（ADR-0115 C2）。
func TestAgenticLoop_NotTruncatedByMaxSteps(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	agent := NewAgentWithDefaults("test-agentic-no-maxsteps")
	agent.sCtx.MaxStepsLimit = 24

	var resps []*types.ProviderResponse
	for i := 0; i < 15; i++ {
		resps = append(resps, &types.ProviderResponse{
			Content: "继续",
			ToolCalls: []types.InferToolCall{
				{ID: fmt.Sprintf("c%d", i), Name: "read_file", Input: []byte(fmt.Sprintf(`{"path":"a%d"}`, i))},
			},
		})
	}
	resps = append(resps, &types.ProviderResponse{Content: "完成"})
	prov := &loopTestProvider{responses: resps}
	agent.InjectProvider(prov)
	agent.InjectPolicyGate(&allowPolicyGate{})
	agent.InjectToolExecutor(&mockToolExecutor{})

	runAgenticTurnWait(t, agent)

	if st := agent.sm.Current(); st != types.AgentStateComplete {
		t.Fatalf("15 轮工具调用应正常完成，当前状态: %v", st)
	}
	if got := prov.callCount(); got != 16 {
		t.Fatalf("期望 16 次推理，实际 %d", got)
	}
	if agent.sCtx.StepsUsed <= 24 {
		t.Fatalf("用例未覆盖超过 24 步的场景: StepsUsed=%d", agent.sCtx.StepsUsed)
	}
}

// TestStepLimitApplies 固化各模式下 max_steps 是否生效。
// agentic 连 KillThrottle 也不走步数截断：Throttle 时累计步数早已 >3，按步数截断会当场 abort 回合，
// 改由 TestAgenticLoop_ThrottleGraceRoundsThenWrapUp 固化"再给 3 轮后收尾"。
func TestStepLimitApplies(t *testing.T) {
	agent := NewAgentWithDefaults("test-step-limit-applies")

	t.Setenv("POLARIS_LOOP_MODE", "staged")
	if !agent.stepLimitApplies() {
		t.Fatal("staged 模式必须施加 max_steps 截断")
	}

	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	prev := metrics.GlobalKillswitchStage.Load()
	t.Cleanup(func() { metrics.GlobalKillswitchStage.Store(prev) })
	for _, st := range []types.KillState{types.KillNormal, types.KillThrottle} {
		metrics.GlobalKillswitchStage.Store(int32(st))
		if agent.stepLimitApplies() {
			t.Fatalf("agentic 模式在 KillState=%v 下不得施加 max_steps 截断", st)
		}
	}
}

func distinctToolRounds(n int) []*types.ProviderResponse {
	var resps []*types.ProviderResponse
	for i := 0; i < n; i++ {
		resps = append(resps, &types.ProviderResponse{
			Content: "继续",
			ToolCalls: []types.InferToolCall{
				{ID: fmt.Sprintf("c%d", i), Name: "read_file", Input: []byte(fmt.Sprintf(`{"path":"a%d"}`, i))},
			},
		})
	}
	return resps
}

// TestAgenticLoop_ThrottleGraceRoundsThenWrapUp 回归：Throttle 在回合进行到 15 步之后才到来，
// 不得当场截断（旧行为：limit 被收紧到 3 而 StepsUsed 早已 >3 → 立即 abort）；
// 应再给 throttleGraceRounds 轮，注入收尾提示，模型仍不收尾才 abort。
func TestAgenticLoop_ThrottleGraceRoundsThenWrapUp(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	prev := metrics.GlobalKillswitchStage.Load()
	t.Cleanup(func() { metrics.GlobalKillswitchStage.Store(prev) })
	metrics.GlobalKillswitchStage.Store(int32(types.KillThrottle))

	agent := NewAgentWithDefaults("test-throttle-grace")
	agent.sCtx.MaxStepsLimit = 3
	prov := &loopTestProvider{responses: distinctToolRounds(30)}
	agent.InjectProvider(prov)
	agent.InjectPolicyGate(&allowPolicyGate{})
	agent.InjectToolExecutor(&mockToolExecutor{})

	runAgenticTurnWait(t, agent)

	if st := agent.sm.Current(); st != types.AgentStateFailed {
		t.Fatalf("Throttle 下模型不收尾应 abort，当前状态: %v", st)
	}
	got := prov.callCount()
	if got <= throttleGraceRounds {
		t.Fatalf("Throttle 不得按累计步数当场截断，仅推理 %d 次", got)
	}
	if got > throttleGraceRounds+3 {
		t.Fatalf("Throttle 宽限后应很快收尾，推理了 %d 次", got)
	}
	if !historyMentions(prov, "killswitch throttle") {
		t.Fatal("abort 之前应先在易变上下文注入收尾提示")
	}
}

// TestAgenticLoop_ThrottleWrapUpCompletes 模型在收尾提示后给出文本即正常完成，不被 abort。
func TestAgenticLoop_ThrottleWrapUpCompletes(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	prev := metrics.GlobalKillswitchStage.Load()
	t.Cleanup(func() { metrics.GlobalKillswitchStage.Store(prev) })
	metrics.GlobalKillswitchStage.Store(int32(types.KillThrottle))

	agent := NewAgentWithDefaults("test-throttle-wrapup")
	resps := distinctToolRounds(throttleGraceRounds + 1)
	resps = append(resps, &types.ProviderResponse{Content: "阶段性结论"})
	prov := &loopTestProvider{responses: resps}
	agent.InjectProvider(prov)
	agent.InjectPolicyGate(&allowPolicyGate{})
	agent.InjectToolExecutor(&mockToolExecutor{})

	runAgenticTurnWait(t, agent)

	if st := agent.sm.Current(); st != types.AgentStateComplete {
		t.Fatalf("收尾提示后给出结论应正常完成，当前状态: %v", st)
	}
}

// TestAgenticLoop_ThrottleRecoveryCancelsWrapUp Throttle 自动回落后撤销收尾提示，回合继续。
func TestAgenticLoop_ThrottleRecoveryCancelsWrapUp(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	prev := metrics.GlobalKillswitchStage.Load()
	t.Cleanup(func() { metrics.GlobalKillswitchStage.Store(prev) })
	metrics.GlobalKillswitchStage.Store(int32(types.KillThrottle))

	agent := NewAgentWithDefaults("test-throttle-recover")
	agent.sCtx.Mu.Lock()
	agent.sCtx.LoopThrottleActive = true
	agent.sCtx.LoopThrottleStartRound = 0
	th, rounds := agent.syncThrottleLocked(throttleGraceRounds)
	agent.sCtx.LoopThrottleWarned = true
	agent.sCtx.LoopWarningInjected = true
	agent.sCtx.LoopBudgetWarning = "x"
	agent.composeLoopWarningLocked()
	agent.sCtx.Mu.Unlock()
	if !th || rounds != throttleGraceRounds {
		t.Fatalf("throttled=%v rounds=%d", th, rounds)
	}

	metrics.GlobalKillswitchStage.Store(int32(types.KillNormal))
	agent.sCtx.Mu.Lock()
	th, _ = agent.syncThrottleLocked(throttleGraceRounds + 1)
	warn, injected := agent.sCtx.LoopWarningPrompt, agent.sCtx.LoopWarningInjected
	agent.sCtx.Mu.Unlock()
	if th || warn != "" || injected {
		t.Fatalf("回落后应撤销提示: throttled=%v warn=%q injected=%v", th, warn, injected)
	}
}

// TestAgenticLoop_NoProgressRepeatWarnsThenStops 原地打转：同工具同参数得到同结果，
// 先提醒（易变上下文末尾），继续重复到收尾线才 abort。
func TestAgenticLoop_NoProgressRepeatWarnsThenStops(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	agent := NewAgentWithDefaults("test-no-progress")
	var resps []*types.ProviderResponse
	for i := 0; i < 30; i++ {
		resps = append(resps, &types.ProviderResponse{
			ToolCalls: []types.InferToolCall{{ID: fmt.Sprintf("c%d", i), Name: "read_file", Input: []byte(`{"path":"same"}`)}},
		})
	}
	prov := &loopTestProvider{responses: resps}
	agent.InjectProvider(prov)
	agent.InjectPolicyGate(&allowPolicyGate{})
	agent.InjectToolExecutor(&mockToolExecutor{})

	runAgenticTurnWait(t, agent)

	if st := agent.sm.Current(); st != types.AgentStateFailed {
		t.Fatalf("无进展重复应收尾，当前状态: %v", st)
	}
	stopAt := 8
	if got := prov.callCount(); got != stopAt {
		t.Fatalf("应在第 %d 次相同结果时收尾，推理 %d 次", stopAt, got)
	}
	if !historyMentions(prov, "无进展提醒") {
		t.Fatal("收尾前应已注入无进展提醒")
	}
}

// TestAgenticLoop_LegitRepeatNotFlagged 合法重复：参数不同的同名调用、结果在变的轮询，均不得被提醒或收尾。
func TestAgenticLoop_LegitRepeatNotFlagged(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	agent := NewAgentWithDefaults("test-legit-repeat")
	resps := distinctToolRounds(20)
	resps = append(resps, &types.ProviderResponse{Content: "完成"})
	prov := &loopTestProvider{responses: resps}
	agent.InjectProvider(prov)
	agent.InjectPolicyGate(&allowPolicyGate{})
	agent.InjectToolExecutor(&mockToolExecutor{})

	runAgenticTurnWait(t, agent)

	if st := agent.sm.Current(); st != types.AgentStateComplete {
		t.Fatalf("合法调用不得被收尾，当前状态: %v", st)
	}
	if historyMentions(prov, "无进展提醒") {
		t.Fatal("合法调用不得注入无进展提醒")
	}
}

func historyMentions(p *loopTestProvider, needle string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, msgs := range p.history {
		for _, m := range msgs {
			if strings.Contains(m.Content, needle) {
				return true
			}
		}
	}
	return false
}

// TestAgentProfile_MaxTurnsClampsRunawayLimit agentic 下 max_turns 映射到失控保护轮数。
func TestAgentProfile_MaxTurnsClampsRunawayLimit(t *testing.T) {
	a := NewAgentWithDefaults("profile-runaway")
	a.SetAgentProfile(&types.AgentProfileSpec{Name: "r", MaxTurns: 4})
	if a.sCtx.LoopRunawayLimit != 4 {
		t.Fatalf("max_turns 应收紧失控保护轮数，got %d", a.sCtx.LoopRunawayLimit)
	}
}
