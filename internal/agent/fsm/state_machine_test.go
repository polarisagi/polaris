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

// TestStateMachine_InterruptStashAndResume_GD03 复现并验证 GD-03 死锁修复：
// S_INTERRUPT 期间收到未注册的业务 trigger（如已在途的异步 LLM 回调 TriggerPlanDone）
// 必须被暂存而非报错丢弃，Resume 后必须自动重投递，驱动状态机继续推进，
// 而不是永久停留在 interruptFrom 状态等待一个已经丢失的事件。
func TestStateMachine_InterruptStashAndResume_GD03(t *testing.T) {
	sm := NewStateMachine(&dummyContextBuilder{})
	sCtx := &StateContext{AgentID: "test-interrupt", MaxReplan: 3}
	ctx := context.Background()

	dispatched := make(chan types.AgentTrigger, 10)
	sm.SetIntentDispatcher(func(tr types.AgentTrigger) { dispatched <- tr })

	steps := []types.AgentTrigger{
		types.TriggerIntentReceived,
		types.TriggerPerceiveDone,
	}
	for _, trig := range steps {
		if _, err := sm.Dispatch(ctx, sCtx, trig); err != nil {
			t.Fatalf("step %v: %v", trig, err)
		}
	}
	if sm.Current() != types.AgentStatePlan {
		t.Fatalf("期望到达 S_PLAN, 实际 %v", sm.Current())
	}

	// 中断：切换到 S_INTERRUPT
	if _, err := sm.Dispatch(ctx, sCtx, types.TriggerInterruptReceived); err != nil {
		t.Fatalf("interrupt receive: %v", err)
	}
	if sm.Current() != types.AgentStateInterrupt {
		t.Fatalf("期望 S_INTERRUPT, 实际 %v", sm.Current())
	}

	// 中断期间，此前发起的异步 LLM 调用才返回并投递 TriggerPlanDone——
	// 修复前：Dispatch 会返回 "no transition" 错误，事件被整体丢弃。
	// 修复后：应静默暂存（不报错、不改变当前状态）。
	if _, err := sm.Dispatch(ctx, sCtx, types.TriggerPlanDone); err != nil {
		t.Fatalf("S_INTERRUPT 期间的未预期 trigger 应被暂存而非报错: %v", err)
	}
	if sm.Current() != types.AgentStateInterrupt {
		t.Fatalf("暂存期间状态不应改变, 实际 %v", sm.Current())
	}

	// 恢复：应回到中断前的 S_PLAN，并自动异步重投递暂存的 TriggerPlanDone
	if _, err := sm.Dispatch(ctx, sCtx, types.TriggerInterruptResume); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if sm.Current() != types.AgentStatePlan {
		t.Fatalf("恢复后应回到 S_PLAN, 实际 %v", sm.Current())
	}

	select {
	case tr := <-dispatched:
		if tr != types.TriggerPlanDone {
			t.Fatalf("期望重投递 TriggerPlanDone, 实际 %v", tr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("超时：暂存的 TriggerPlanDone 未被自动重新投递——死锁修复未生效")
	}

	// 模拟 Agent.Run 事件循环消费重投递的事件，驱动状态机继续推进（证明不再死锁）。
	if _, err := sm.Dispatch(ctx, sCtx, types.TriggerPlanDone); err != nil {
		t.Fatalf("重投递事件的 Dispatch 失败: %v", err)
	}
	if sm.Current() != types.AgentStateValidate {
		t.Fatalf("重投递事件处理后应推进到 S_VALIDATE, 实际 %v", sm.Current())
	}
}

// TestStateMachine_InterruptAbort_DiscardsStashedTriggers 验证 Abort 路径下
// 暂存队列被清空丢弃，且不会异步重投递任何事件（任务已判定放弃）。
func TestStateMachine_InterruptAbort_DiscardsStashedTriggers(t *testing.T) {
	sm := NewStateMachine(&dummyContextBuilder{})
	sCtx := &StateContext{AgentID: "test-abort", MaxReplan: 3}
	ctx := context.Background()

	dispatched := make(chan types.AgentTrigger, 10)
	sm.SetIntentDispatcher(func(tr types.AgentTrigger) { dispatched <- tr })

	if _, err := sm.Dispatch(ctx, sCtx, types.TriggerIntentReceived); err != nil {
		t.Fatalf("step 1: %v", err)
	}
	if _, err := sm.Dispatch(ctx, sCtx, types.TriggerPerceiveDone); err != nil {
		t.Fatalf("step 2: %v", err)
	}
	if _, err := sm.Dispatch(ctx, sCtx, types.TriggerInterruptReceived); err != nil {
		t.Fatalf("interrupt receive: %v", err)
	}

	// 暂存一个事件
	if _, err := sm.Dispatch(ctx, sCtx, types.TriggerPlanDone); err != nil {
		t.Fatalf("stash: %v", err)
	}

	if _, err := sm.Dispatch(ctx, sCtx, types.TriggerInterruptAbort); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if sm.Current() != types.AgentStateFailed {
		t.Fatalf("期望 S_FAILED, 实际 %v", sm.Current())
	}

	select {
	case tr := <-dispatched:
		t.Fatalf("Abort 后不应重投递任何暂存事件，但收到 %v", tr)
	case <-time.After(200 * time.Millisecond):
		// 期望超时：确认没有重投递
	}
}

// mockSlowActivator 模拟耗时的扩展激活（语义检索/MCP 握手），用于验证 GD-04 修复：
// S_REPLAN 的扩展激活不得阻塞 Dispatch/主事件循环。
type mockSlowActivator struct {
	delay time.Duration
	hints []ExtActivatedHint
}

func (m *mockSlowActivator) FindAndActivate(ctx context.Context, goal string) ([]ExtActivatedHint, error) {
	select {
	case <-time.After(m.delay):
		return m.hints, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TestStateMachine_ReplanExtensionActivation_NonBlocking_GD04 验证扩展激活异步化：
// 即使 FindAndActivate 耗时较长，Dispatch 本身必须立即返回，不能阻塞主事件循环
// （修复前：同步执行在 DeterministicEffect.Fn 内部，会占用 Agent.Run 的唯一 goroutine）。
func TestStateMachine_ReplanExtensionActivation_NonBlocking_GD04(t *testing.T) {
	sm := NewStateMachine(&dummyContextBuilder{})
	sCtx := &StateContext{AgentID: "test-gd04", MaxReplan: 3, TaskModel: &TaskModel{Goal: "test goal"}}
	ctx := context.Background()

	dispatched := make(chan types.AgentTrigger, 4)
	sm.SetIntentDispatcher(func(tr types.AgentTrigger) { dispatched <- tr })
	sm.WithExtensionActivator(&mockSlowActivator{delay: 300 * time.Millisecond})

	steps := []types.AgentTrigger{
		types.TriggerIntentReceived,
		types.TriggerPerceiveDone,
		types.TriggerPlanDone,
	}
	for _, trig := range steps {
		if _, err := sm.Dispatch(ctx, sCtx, trig); err != nil {
			t.Fatalf("step %v: %v", trig, err)
		}
	}

	start := time.Now()
	// 第一次 ValidateFail → replanCount 0→1，满足 shouldActivateExtensions 触发条件
	_, err := sm.Dispatch(ctx, sCtx, types.TriggerValidateFail)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("S_VALIDATE → S_REPLAN: %v", err)
	}
	if sm.Current() != types.AgentStateReplan {
		t.Fatalf("期望 S_REPLAN, 实际 %v", sm.Current())
	}
	if elapsed >= 100*time.Millisecond {
		t.Fatalf("Dispatch 不应被 300ms 的扩展激活阻塞，实际耗时 %v", elapsed)
	}

	select {
	case tr := <-dispatched:
		if tr != types.TriggerReplanDone {
			t.Fatalf("期望激活完成后投递 TriggerReplanDone, 实际 %v", tr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("超时：异步扩展激活完成后未投递 TriggerReplanDone")
	}
}

// mockToolRegistry 放行所有工具调用，返回空输出（用于 Agent E2E 测试）。
