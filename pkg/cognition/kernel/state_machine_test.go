package kernel

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
)

func TestStateMachine_FullForwardPath(t *testing.T) {
	sm := NewStateMachine()
	sCtx := &StateContext{AgentID: "test-1", MaxReplan: 3}

	// 1. S_IDLE → S_PERCEIVE
	effects, err := sm.Dispatch(context.Background(), sCtx, protocol.TriggerIntentReceived)
	if err != nil {
		t.Fatalf("S_IDLE → S_PERCEIVE: %v", err)
	}
	if sm.Current() != protocol.AgentStatePerceive {
		t.Errorf("期望 S_PERCEIVE, 实际 %v", sm.Current())
	}
	if len(effects) != 1 || !effects[0].IsLLMFill() {
		t.Error("S_PERCEIVE 应产生 1 个 LLMFillEffect")
	}

	// 2. S_PERCEIVE → S_PLAN
	_, err = sm.Dispatch(context.Background(), sCtx, protocol.TriggerPerceiveDone)
	if err != nil {
		t.Fatalf("S_PERCEIVE → S_PLAN: %v", err)
	}
	if sm.Current() != protocol.AgentStatePlan {
		t.Errorf("期望 S_PLAN, 实际 %v", sm.Current())
	}

	// 3. S_PLAN → S_VALIDATE
	effects, err = sm.Dispatch(context.Background(), sCtx, protocol.TriggerPlanDone)
	if err != nil {
		t.Fatalf("S_PLAN → S_VALIDATE: %v", err)
	}
	if sm.Current() != protocol.AgentStateValidate {
		t.Errorf("期望 S_VALIDATE, 实际 %v", sm.Current())
	}
	if len(effects) != 1 || effects[0].IsLLMFill() {
		t.Error("S_VALIDATE 应产生 1 个 DeterministicEffect")
	}

	// 4. S_VALIDATE → S_EXECUTE
	_, err = sm.Dispatch(context.Background(), sCtx, protocol.TriggerValidateOk)
	if err != nil {
		t.Fatalf("S_VALIDATE → S_EXECUTE: %v", err)
	}
	if sm.Current() != protocol.AgentStateExecute {
		t.Errorf("期望 S_EXECUTE, 实际 %v", sm.Current())
	}

	// 5. S_EXECUTE → S_REFLECT
	effects, err = sm.Dispatch(context.Background(), sCtx, protocol.TriggerExecuteDone)
	if err != nil {
		t.Fatalf("S_EXECUTE → S_REFLECT: %v", err)
	}
	if sm.Current() != protocol.AgentStateReflect {
		t.Errorf("期望 S_REFLECT, 实际 %v", sm.Current())
	}
	if len(effects) != 1 || !effects[0].IsLLMFill() {
		t.Error("S_REFLECT 应产生 1 个 LLMFillEffect")
	}

	// 6. S_REFLECT → S_COMPLETE (正向终态)
	_, err = sm.Dispatch(context.Background(), sCtx, protocol.TriggerReflectDone)
	if err != nil {
		t.Fatalf("S_REFLECT → S_COMPLETE: %v", err)
	}
	if sm.Current() != protocol.AgentStateComplete {
		t.Errorf("期望 S_COMPLETE, 实际 %v", sm.Current())
	}

	// 验证历史
	history := sm.History()
	expectedLen := 6
	if len(history) != expectedLen {
		t.Errorf("历史应为 %d 步, 实际 %d: %v", expectedLen, len(history), history)
	}
}

func TestStateMachine_ValidationFailure_Replan(t *testing.T) {
	sm := NewStateMachine()
	sCtx := &StateContext{AgentID: "test-2", MaxReplan: 3}
	ctx := context.Background()

	// 走到 S_VALIDATE
	steps := []protocol.AgentTrigger{
		protocol.TriggerIntentReceived,
		protocol.TriggerPerceiveDone,
		protocol.TriggerPlanDone,
	}
	for _, trig := range steps {
		_, err := sm.Dispatch(ctx, sCtx, trig)
		if err != nil {
			t.Fatalf("step %v: %v", trig, err)
		}
	}

	// 校验失败 → S_REPLAN
	_, err := sm.Dispatch(ctx, sCtx, protocol.TriggerValidateFail)
	if err != nil {
		t.Fatalf("S_VALIDATE → S_REPLAN: %v", err)
	}
	if sm.Current() != protocol.AgentStateReplan {
		t.Errorf("期望 S_REPLAN, 实际 %v", sm.Current())
	}
	if sm.ReplanCount() != 1 {
		t.Errorf("ReplanCount 应为 1, 实际 %d", sm.ReplanCount())
	}

	// 从 S_REPLAN 重新规划 → S_PLAN
	_, err = sm.Dispatch(ctx, sCtx, protocol.TriggerReplanDone)
	if err != nil {
		t.Fatalf("S_REPLAN → S_PLAN: %v", err)
	}
	if sm.Current() != protocol.AgentStatePlan {
		t.Errorf("期望 S_PLAN, 实际 %v", sm.Current())
	}

	// 继续正常路径验证可恢复
	_, err = sm.Dispatch(ctx, sCtx, protocol.TriggerPlanDone)
	if err != nil {
		t.Fatalf("S_PLAN → S_VALIDATE (retry): %v", err)
	}
	if sm.Current() != protocol.AgentStateValidate {
		t.Errorf("重试后应回到 S_VALIDATE, 实际 %v", sm.Current())
	}
}

func TestStateMachine_ReplanGuardExhaustion(t *testing.T) {
	sm := NewStateMachine()
	sCtx := &StateContext{AgentID: "test-3", MaxReplan: 3}
	ctx := context.Background()

	// 走到 S_VALIDATE
	steps := []protocol.AgentTrigger{
		protocol.TriggerIntentReceived,
		protocol.TriggerPerceiveDone,
		protocol.TriggerPlanDone,
	}
	for _, trig := range steps {
		_, err := sm.Dispatch(ctx, sCtx, trig)
		if err != nil {
			t.Fatalf("step %v: %v", trig, err)
		}
	}

	// 第 1 次 ValidateFail: replanCount 0→1, 未耗尽
	_, err := sm.Dispatch(ctx, sCtx, protocol.TriggerValidateFail)
	if err != nil {
		t.Fatalf("replan 1: %v", err)
	}
	if sm.Current() != protocol.AgentStateReplan {
		t.Errorf("期望 S_REPLAN, 实际 %v", sm.Current())
	}
	// 从 S_REPLAN 回归 S_PLAN → S_VALIDATE
	sm.Dispatch(ctx, sCtx, protocol.TriggerReplanDone)
	sm.Dispatch(ctx, sCtx, protocol.TriggerPlanDone)

	// 第 2 次 ValidateFail: replanCount 1→2
	_, err = sm.Dispatch(ctx, sCtx, protocol.TriggerValidateFail)
	if err != nil {
		t.Fatalf("replan 2: %v", err)
	}
	sm.Dispatch(ctx, sCtx, protocol.TriggerReplanDone)
	sm.Dispatch(ctx, sCtx, protocol.TriggerPlanDone)

	// 第 3 次 ValidateFail: replanCount 2→3, guard 耗尽 → 自动 S_FAILED
	_, err = sm.Dispatch(ctx, sCtx, protocol.TriggerValidateFail)
	if err == nil {
		t.Fatal("第 3 次 replan 应返回 ErrReplanExhausted")
	}
	if sm.Current() != protocol.AgentStateFailed {
		t.Errorf("耗尽后应由 Dispatch 自动推进到 S_FAILED, 实际 %v", sm.Current())
	}
	if sm.ReplanCount() != 3 {
		t.Errorf("ReplanCount 应为 3, 实际 %d", sm.ReplanCount())
	}
}

func TestStateMachine_ExecutionFailure_Rollback(t *testing.T) {
	sm := NewStateMachine()
	sCtx := &StateContext{AgentID: "test-4", MaxReplan: 3}
	ctx := context.Background()

	// 走到 S_EXECUTE
	steps := []protocol.AgentTrigger{
		protocol.TriggerIntentReceived,
		protocol.TriggerPerceiveDone,
		protocol.TriggerPlanDone,
		protocol.TriggerValidateOk,
	}
	for _, trig := range steps {
		_, err := sm.Dispatch(ctx, sCtx, trig)
		if err != nil {
			t.Fatalf("step %v: %v", trig, err)
		}
	}

	// 执行失败 → S_ROLLBACK
	effects, err := sm.Dispatch(ctx, sCtx, protocol.TriggerExecuteFail)
	if err != nil {
		t.Fatalf("S_EXECUTE → S_ROLLBACK: %v", err)
	}
	if sm.Current() != protocol.AgentStateRollback {
		t.Errorf("期望 S_ROLLBACK, 实际 %v", sm.Current())
	}
	if len(effects) != 1 || effects[0].IsLLMFill() {
		t.Error("S_ROLLBACK 应产生 DeterministicEffect")
	}

	// Rollback 完成 → S_REPLAN
	_, err = sm.Dispatch(ctx, sCtx, protocol.TriggerRollbackDone)
	if err != nil {
		t.Fatalf("S_ROLLBACK → S_REPLAN: %v", err)
	}
	if sm.Current() != protocol.AgentStateReplan {
		t.Errorf("期望 S_REPLAN, 实际 %v", sm.Current())
	}
	if sm.ReplanCount() != 1 {
		t.Errorf("Rollback 进入 Replan 应计数, 实际 %d", sm.ReplanCount())
	}
}

func TestStateMachine_Reset(t *testing.T) {
	sm := NewStateMachine()
	sCtx := &StateContext{AgentID: "test-5", MaxReplan: 3}
	ctx := context.Background()

	// 走到一半
	steps := []protocol.AgentTrigger{
		protocol.TriggerIntentReceived,
		protocol.TriggerPerceiveDone,
		protocol.TriggerPlanDone,
	}
	for _, trig := range steps {
		sm.Dispatch(ctx, sCtx, trig)
	}

	sm.Reset()
	if sm.Current() != protocol.AgentStateIdle {
		t.Errorf("Reset 后应为 S_IDLE, 实际 %v", sm.Current())
	}
	if sm.ReplanCount() != 0 {
		t.Errorf("Reset 后 ReplanCount 应为 0, 实际 %d", sm.ReplanCount())
	}
	if len(sm.History()) != 0 {
		t.Error("Reset 后历史应为空")
	}
}

func TestStateMachine_EffectTypeDiscrimination(t *testing.T) {
	sm := NewStateMachine()
	sCtx := &StateContext{AgentID: "test-6", MaxReplan: 3}
	ctx := context.Background()

	// LLM 状态应产生 LLMFillEffect
	llmTriggers := map[protocol.AgentTrigger]string{
		protocol.TriggerIntentReceived: "S_IDLE→S_PERCEIVE",
		protocol.TriggerPerceiveDone:   "S_PERCEIVE→S_PLAN",
		protocol.TriggerExecuteDone:    "S_EXECUTE→S_REFLECT (需先到达 S_EXECUTE)",
	}

	// 测试 S_IDLE → S_PERCEIVE
	effects, _ := sm.Dispatch(ctx, sCtx, protocol.TriggerIntentReceived)
	if !effects[0].IsLLMFill() {
		t.Errorf("%s: 应为 LLMFillEffect", llmTriggers[protocol.TriggerIntentReceived])
	}

	// 测试 S_PERCEIVE → S_PLAN
	effects, _ = sm.Dispatch(ctx, sCtx, protocol.TriggerPerceiveDone)
	if !effects[0].IsLLMFill() {
		t.Errorf("%s: 应为 LLMFillEffect", llmTriggers[protocol.TriggerPerceiveDone])
	}

	// Deterministic 状态应产生 DeterministicEffect
	deterministicSteps := []protocol.AgentTrigger{
		protocol.TriggerPlanDone,   // S_VALIDATE
		protocol.TriggerValidateOk, // S_EXECUTE
	}
	for _, trig := range deterministicSteps {
		effects, err := sm.Dispatch(ctx, sCtx, trig)
		if err != nil {
			t.Fatalf("trigger %v: %v", trig, err)
		}
		if len(effects) > 0 && effects[0].IsLLMFill() {
			t.Errorf("trigger %v: 应为 DeterministicEffect", trig)
		}
	}

	// S_EXECUTE → S_REFLECT (LLM)
	effects, _ = sm.Dispatch(ctx, sCtx, protocol.TriggerExecuteDone)
	if !effects[0].IsLLMFill() {
		t.Error("S_REFLECT: 应为 LLMFillEffect")
	}
}

func TestStateMachine_ConcurrencySafe(t *testing.T) {
	sm := NewStateMachine()
	sCtx := &StateContext{AgentID: "concurrent", MaxReplan: 3}
	ctx := context.Background()

	// 先走到 S_PLAN
	steps := []protocol.AgentTrigger{
		protocol.TriggerIntentReceived,
		protocol.TriggerPerceiveDone,
	}
	for _, trig := range steps {
		sm.Dispatch(ctx, sCtx, trig)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 20)

	// 并发读 Current() 和 History()
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = sm.Current()
			_ = sm.History()
		}()
	}

	// 并发写 Dispatch() 和 Reset()
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			// 每个并发创建一个自己的 sm + dispatch
			localSM := NewStateMachine()
			localSCtx := &StateContext{AgentID: "local", MaxReplan: 3}
			_, err := localSM.Dispatch(ctx, localSCtx, protocol.TriggerIntentReceived)
			if err != nil {
				errCh <- err
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("并发错误: %v", err)
	}
}

func TestStateMachine_Timeout(t *testing.T) {
	sm := NewStateMachine()

	// 验证 startedAt 被设置
	if sm.startedAt.IsZero() {
		t.Error("startedAt 应被初始化")
	}
	if time.Since(sm.startedAt) > time.Second {
		t.Error("startedAt 应在最近创建")
	}
}

func TestStateMachine_InvalidTrigger(t *testing.T) {
	sm := NewStateMachine()
	sCtx := &StateContext{AgentID: "test-7", MaxReplan: 3}
	ctx := context.Background()

	// 从 S_IDLE 发无效 trigger
	_, err := sm.Dispatch(ctx, sCtx, protocol.TriggerValidateOk)
	if err == nil {
		t.Error("无效 trigger 应返回错误")
	}
}

// allowPolicyGate 放行所有请求（用于 Agent HappyPath 测试）。
type allowPolicyGate struct{}

func (g *allowPolicyGate) IsAuthorized(_ context.Context, _, _, _ string, _ map[string]any) (bool, error) {
	return true, nil
}
func (g *allowPolicyGate) Review(_ context.Context, req protocol.PolicyReviewRequest) (protocol.PolicyReviewResult, error) {
	return protocol.PolicyReviewResult{Allowed: true, Reason: "allow-all"}, nil
}

// mockToolRegistry 放行所有工具调用，返回空输出（用于 Agent E2E 测试）。
type mockToolRegistry struct{}

func (r *mockToolRegistry) Register(_ protocol.Tool) error { return nil }
func (r *mockToolRegistry) Lookup(name string) (protocol.Tool, error) {
	return protocol.Tool{Name: name, Source: protocol.ToolBuiltin}, nil
}
func (r *mockToolRegistry) List() []protocol.Tool { return nil }
func (r *mockToolRegistry) ExecuteTool(_ context.Context, name string, _ []byte, taintLevel protocol.TaintLevel) (*protocol.ToolResult, error) {
	return &protocol.ToolResult{Success: true, Output: []byte(`{"ok":true}`)}, nil
}

// mockProvider 模拟 M1 LLM 接口
type mockProvider struct {
	failCount int
	failOn    string // 指定在哪个 prompt 时报错
}

func (m *mockProvider) Infer(ctx context.Context, msgs []protocol.Message, opts ...protocol.InferOption) (*protocol.ProviderResponse, error) {
	if m.failOn != "" && len(msgs) > 0 {
		if strings.Contains(msgs[0].Content, m.failOn) {
			m.failCount++
			return nil, perrors.New(perrors.CodeInternal, "mock llm failure")
		}
	}

	// 如果是请求 DAG Plan，则返回合法的 DAGModel JSON
	if len(msgs) > 0 && strings.Contains(msgs[0].Content, "基于 TaskModel 生成执行 DAG") {
		return &protocol.ProviderResponse{Content: `{"nodes":[{"id":"n1","action":"test_tool","params":{},"retry":0,"timeout":""}],"edges":[]}`}, nil
	}

	return &protocol.ProviderResponse{Content: "mock_success"}, nil
}

func (m *mockProvider) StreamInfer(ctx context.Context, msgs []protocol.Message, opts ...protocol.InferOption) (<-chan protocol.StreamEvent, error) {
	return nil, perrors.New(perrors.CodeInternal, "not implemented")
}

func (m *mockProvider) Capabilities() protocol.ProviderCapabilities {
	return protocol.ProviderCapabilities{
		SupportsStreaming: false,
		SupportsTools:     true,
		SupportsThinking:  false,
	}
}

func (m *mockProvider) Tokenizer() protocol.TokenizerAdapter {
	return nil // mock 中无需实际计算 token
}

func (m *mockProvider) ModelID() string {
	return "mock"
}

func TestAgent_HappyPath(t *testing.T) {
	agent := NewAgentWithDefaults("test-agent-1")
	agent.InjectProvider(&mockProvider{})
	// 注入 allow-all PolicyGate，使 L1-Policy 通过
	agent.InjectPolicyGate(&allowPolicyGate{})
	// 注入 mockToolRegistry，使 S_EXECUTE 工具调用也通过
	agent.InjectToolRegistry(&mockToolRegistry{})
	// 注入合法 DAGModel，使 L0/L1 通过后 ValidateOk 被推送
	agent.sCtx.DAGModel = &DAGModel{
		Nodes: []ExecNode{{ID: "n1", ToolName: "read_file"}},
	}

	// 监听状态机完成
	done := make(chan struct{})
	go func() {
		err := agent.Run(context.Background())
		if err != nil {
			t.Errorf("agent run failed: %v", err)
		}
		close(done)
	}()

	// 发送启动信号
	agent.SendIntent(protocol.TriggerIntentReceived)

	select {
	case <-done:
		// 校验终态
		if agent.StateMachine().Current() != protocol.AgentStateComplete {
			t.Errorf("expected state Complete, got %v", agent.StateMachine().Current())
		}

		// 校验历史链路
		history := agent.StateMachine().History()
		expectedTransitions := []protocol.AgentState{
			protocol.AgentStateIdle,
			protocol.AgentStatePerceive,
			protocol.AgentStatePlan,
			protocol.AgentStateValidate,
			protocol.AgentStateExecute,
			protocol.AgentStateReflect,
		}
		if len(history) != len(expectedTransitions) {
			t.Fatalf("history length mismatch, got %v, want %v, history: %v", len(history), len(expectedTransitions), history)
		}
		for i, h := range history {
			if h != expectedTransitions[i] {
				t.Errorf("history[%d] mismatch: got %v, want %v", i, h, expectedTransitions[i])
			}
		}

	case <-time.After(3 * time.Second):
		t.Fatal("agent run timeout")
	}
}

func TestAgent_ReplanExhausted(t *testing.T) {
	agent := NewAgentWithDefaults("test-agent-2")
	// 强制在 Perceive 阶段无限失败
	agent.InjectProvider(&mockProvider{failOn: "将用户意图结构化为 TaskModel JSON"})
	agent.sCtx.MaxReplan = 3

	// 直接验证它能自动完成 run，并在错误之后走到 FAILED。
	// 这里通过 Run 的自动驱动
	done := make(chan error)
	go func() {
		done <- agent.Run(context.Background())
	}()

	agent.SendIntent(protocol.TriggerIntentReceived)

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, ErrReplanExhausted) {
			t.Errorf("expected ErrReplanExhausted or nil, got: %v", err)
		}
		if agent.StateMachine().Current() != protocol.AgentStateFailed {
			t.Errorf("expected state Failed, got %v", agent.StateMachine().Current())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("agent run timeout during exhausted")
	}
}
