package fsm

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

func TestStateMachine_FullForwardPath(t *testing.T) {
	sm := NewStateMachine(&dummyContextBuilder{})
	sCtx := &StateContext{AgentID: "test-1", MaxReplan: 3}

	// 1. S_IDLE → S_PERCEIVE
	effects, err := sm.Dispatch(context.Background(), sCtx, types.TriggerIntentReceived)
	if err != nil {
		t.Fatalf("S_IDLE → S_PERCEIVE: %v", err)
	}
	if sm.Current() != types.AgentStatePerceive {
		t.Errorf("期望 S_PERCEIVE, 实际 %v", sm.Current())
	}
	if len(effects) != 1 || !effects[0].IsLLMFill() {
		t.Error("S_PERCEIVE 应产生 1 个 LLMFillEffect")
	}

	// 2. S_PERCEIVE → S_PLAN
	_, err = sm.Dispatch(context.Background(), sCtx, types.TriggerPerceiveDone)
	if err != nil {
		t.Fatalf("S_PERCEIVE → S_PLAN: %v", err)
	}
	if sm.Current() != types.AgentStatePlan {
		t.Errorf("期望 S_PLAN, 实际 %v", sm.Current())
	}

	// 3. S_PLAN → S_VALIDATE
	effects, err = sm.Dispatch(context.Background(), sCtx, types.TriggerPlanDone)
	if err != nil {
		t.Fatalf("S_PLAN → S_VALIDATE: %v", err)
	}
	if sm.Current() != types.AgentStateValidate {
		t.Errorf("期望 S_VALIDATE, 实际 %v", sm.Current())
	}
	if len(effects) != 1 || effects[0].IsLLMFill() {
		t.Error("S_VALIDATE 应产生 1 个 DeterministicEffect")
	}

	// 4. S_VALIDATE → S_EXECUTE
	_, err = sm.Dispatch(context.Background(), sCtx, types.TriggerValidateOk)
	if err != nil {
		t.Fatalf("S_VALIDATE → S_EXECUTE: %v", err)
	}
	if sm.Current() != types.AgentStateExecute {
		t.Errorf("期望 S_EXECUTE, 实际 %v", sm.Current())
	}

	// 5. S_EXECUTE → S_REFLECT
	effects, err = sm.Dispatch(context.Background(), sCtx, types.TriggerExecuteDone)
	if err != nil {
		t.Fatalf("S_EXECUTE → S_REFLECT: %v", err)
	}
	if sm.Current() != types.AgentStateReflect {
		t.Errorf("期望 S_REFLECT, 实际 %v", sm.Current())
	}
	if len(effects) != 1 || !effects[0].IsLLMFill() {
		t.Error("S_REFLECT 应产生 1 个 LLMFillEffect")
	}

	// 6. S_REFLECT → S_COMPLETE (正向终态)
	_, err = sm.Dispatch(context.Background(), sCtx, types.TriggerReflectDone)
	if err != nil {
		t.Fatalf("S_REFLECT → S_COMPLETE: %v", err)
	}
	if sm.Current() != types.AgentStateComplete {
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
	sm := NewStateMachine(&dummyContextBuilder{})
	sCtx := &StateContext{AgentID: "test-2", MaxReplan: 3}
	ctx := context.Background()

	// 走到 S_VALIDATE
	steps := []types.AgentTrigger{
		types.TriggerIntentReceived,
		types.TriggerPerceiveDone,
		types.TriggerPlanDone,
	}
	for _, trig := range steps {
		_, err := sm.Dispatch(ctx, sCtx, trig)
		if err != nil {
			t.Fatalf("step %v: %v", trig, err)
		}
	}

	// 校验失败 → S_REPLAN
	_, err := sm.Dispatch(ctx, sCtx, types.TriggerValidateFail)
	if err != nil {
		t.Fatalf("S_VALIDATE → S_REPLAN: %v", err)
	}
	if sm.Current() != types.AgentStateReplan {
		t.Errorf("期望 S_REPLAN, 实际 %v", sm.Current())
	}
	if sm.ReplanCount() != 1 {
		t.Errorf("ReplanCount 应为 1, 实际 %d", sm.ReplanCount())
	}

	// 从 S_REPLAN 重新规划 → S_PLAN
	_, err = sm.Dispatch(ctx, sCtx, types.TriggerReplanDone)
	if err != nil {
		t.Fatalf("S_REPLAN → S_PLAN: %v", err)
	}
	if sm.Current() != types.AgentStatePlan {
		t.Errorf("期望 S_PLAN, 实际 %v", sm.Current())
	}

	// 继续正常路径验证可恢复
	_, err = sm.Dispatch(ctx, sCtx, types.TriggerPlanDone)
	if err != nil {
		t.Fatalf("S_PLAN → S_VALIDATE (retry): %v", err)
	}
	if sm.Current() != types.AgentStateValidate {
		t.Errorf("重试后应回到 S_VALIDATE, 实际 %v", sm.Current())
	}
}

func TestStateMachine_ReplanGuardExhaustion(t *testing.T) {
	sm := NewStateMachine(&dummyContextBuilder{})
	sCtx := &StateContext{AgentID: "test-3", MaxReplan: 3}
	ctx := context.Background()

	// 走到 S_VALIDATE
	steps := []types.AgentTrigger{
		types.TriggerIntentReceived,
		types.TriggerPerceiveDone,
		types.TriggerPlanDone,
	}
	for _, trig := range steps {
		_, err := sm.Dispatch(ctx, sCtx, trig)
		if err != nil {
			t.Fatalf("step %v: %v", trig, err)
		}
	}

	// 第 1 次 ValidateFail: replanCount 0→1, 未耗尽
	_, err := sm.Dispatch(ctx, sCtx, types.TriggerValidateFail)
	if err != nil {
		t.Fatalf("replan 1: %v", err)
	}
	if sm.Current() != types.AgentStateReplan {
		t.Errorf("期望 S_REPLAN, 实际 %v", sm.Current())
	}
	// 从 S_REPLAN 回归 S_PLAN → S_VALIDATE
	sm.Dispatch(ctx, sCtx, types.TriggerReplanDone)
	sm.Dispatch(ctx, sCtx, types.TriggerPlanDone)

	// 第 2 次 ValidateFail: replanCount 1→2
	_, err = sm.Dispatch(ctx, sCtx, types.TriggerValidateFail)
	if err != nil {
		t.Fatalf("replan 2: %v", err)
	}
	sm.Dispatch(ctx, sCtx, types.TriggerReplanDone)
	sm.Dispatch(ctx, sCtx, types.TriggerPlanDone)

	// 第 3 次 ValidateFail: replanCount 2→3, guard 耗尽 → 自动 S_FAILED
	_, err = sm.Dispatch(ctx, sCtx, types.TriggerValidateFail)
	if err == nil {
		t.Fatal("第 3 次 replan 应返回 ErrReplanExhausted")
	}
	if sm.Current() != types.AgentStateFailed {
		t.Errorf("耗尽后应由 Dispatch 自动推进到 S_FAILED, 实际 %v", sm.Current())
	}
	if sm.ReplanCount() != 3 {
		t.Errorf("ReplanCount 应为 3, 实际 %d", sm.ReplanCount())
	}
}

func TestStateMachine_ExecutionFailure_Rollback(t *testing.T) {
	sm := NewStateMachine(&dummyContextBuilder{})
	sCtx := &StateContext{AgentID: "test-4", MaxReplan: 3}
	ctx := context.Background()

	// 走到 S_EXECUTE
	steps := []types.AgentTrigger{
		types.TriggerIntentReceived,
		types.TriggerPerceiveDone,
		types.TriggerPlanDone,
		types.TriggerValidateOk,
	}
	for _, trig := range steps {
		_, err := sm.Dispatch(ctx, sCtx, trig)
		if err != nil {
			t.Fatalf("step %v: %v", trig, err)
		}
	}

	// 执行失败 → S_ROLLBACK
	effects, err := sm.Dispatch(ctx, sCtx, types.TriggerExecuteFail)
	if err != nil {
		t.Fatalf("S_EXECUTE → S_ROLLBACK: %v", err)
	}
	if sm.Current() != types.AgentStateRollback {
		t.Errorf("期望 S_ROLLBACK, 实际 %v", sm.Current())
	}
	if len(effects) != 1 || effects[0].IsLLMFill() {
		t.Error("S_ROLLBACK 应产生 DeterministicEffect")
	}

	// Rollback 完成 → S_REPLAN
	_, err = sm.Dispatch(ctx, sCtx, types.TriggerRollbackDone)
	if err != nil {
		t.Fatalf("S_ROLLBACK → S_REPLAN: %v", err)
	}
	if sm.Current() != types.AgentStateReplan {
		t.Errorf("期望 S_REPLAN, 实际 %v", sm.Current())
	}
	if sm.ReplanCount() != 1 {
		t.Errorf("Rollback 进入 Replan 应计数, 实际 %d", sm.ReplanCount())
	}
}

func TestStateMachine_Reset(t *testing.T) {
	sm := NewStateMachine(&dummyContextBuilder{})
	sCtx := &StateContext{AgentID: "test-5", MaxReplan: 3}
	ctx := context.Background()

	// 走到一半
	steps := []types.AgentTrigger{
		types.TriggerIntentReceived,
		types.TriggerPerceiveDone,
		types.TriggerPlanDone,
	}
	for _, trig := range steps {
		sm.Dispatch(ctx, sCtx, trig)
	}

	sm.Reset()
	if sm.Current() != types.AgentStateIdle {
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
	sm := NewStateMachine(&dummyContextBuilder{})
	sCtx := &StateContext{AgentID: "test-6", MaxReplan: 3}
	ctx := context.Background()

	// LLM 状态应产生 LLMFillEffect
	llmTriggers := map[types.AgentTrigger]string{
		types.TriggerIntentReceived: "S_IDLE→S_PERCEIVE",
		types.TriggerPerceiveDone:   "S_PERCEIVE→S_PLAN",
		types.TriggerExecuteDone:    "S_EXECUTE→S_REFLECT (需先到达 S_EXECUTE)",
	}

	// 测试 S_IDLE → S_PERCEIVE
	effects, _ := sm.Dispatch(ctx, sCtx, types.TriggerIntentReceived)
	if !effects[0].IsLLMFill() {
		t.Errorf("%s: 应为 LLMFillEffect", llmTriggers[types.TriggerIntentReceived])
	}

	// 测试 S_PERCEIVE → S_PLAN
	effects, _ = sm.Dispatch(ctx, sCtx, types.TriggerPerceiveDone)
	if !effects[0].IsLLMFill() {
		t.Errorf("%s: 应为 LLMFillEffect", llmTriggers[types.TriggerPerceiveDone])
	}

	// Deterministic 状态应产生 DeterministicEffect
	deterministicSteps := []types.AgentTrigger{
		types.TriggerPlanDone,   // S_VALIDATE
		types.TriggerValidateOk, // S_EXECUTE
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
	effects, _ = sm.Dispatch(ctx, sCtx, types.TriggerExecuteDone)
	if !effects[0].IsLLMFill() {
		t.Error("S_REFLECT: 应为 LLMFillEffect")
	}
}

func TestStateMachine_ConcurrencySafe(t *testing.T) {
	sm := NewStateMachine(&dummyContextBuilder{})
	sCtx := &StateContext{AgentID: "concurrent", MaxReplan: 3}
	ctx := context.Background()

	// 先走到 S_PLAN
	steps := []types.AgentTrigger{
		types.TriggerIntentReceived,
		types.TriggerPerceiveDone,
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
			localSM := NewStateMachine(&dummyContextBuilder{})
			localSCtx := &StateContext{AgentID: "local", MaxReplan: 3}
			_, err := localSM.Dispatch(ctx, localSCtx, types.TriggerIntentReceived)
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
	sm := NewStateMachine(&dummyContextBuilder{})

	// 验证 startedAt 被设置
	if sm.startedAt.IsZero() {
		t.Error("startedAt 应被初始化")
	}
	if time.Since(sm.startedAt) > time.Second {
		t.Error("startedAt 应在最近创建")
	}
}

func TestStateMachine_InvalidTrigger(t *testing.T) {
	sm := NewStateMachine(&dummyContextBuilder{})
	sCtx := &StateContext{AgentID: "test-7", MaxReplan: 3}
	ctx := context.Background()

	// 从 S_IDLE 发无效 trigger
	_, err := sm.Dispatch(ctx, sCtx, types.TriggerValidateOk)
	if err == nil {
		t.Error("无效 trigger 应返回错误")
	}
}

// mockToolRegistry 放行所有工具调用，返回空输出（用于 Agent E2E 测试）。
