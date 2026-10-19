package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// TestAgenticLoop_SteerQueue_InjectedAtRoundBoundary 验证用户插话（Steer）在轮次边界注入为 user 消息。
func TestAgenticLoop_SteerQueue_InjectedAtRoundBoundary(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	agent := NewAgentWithDefaults("test-steer-boundary")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := agent.SubscribeStream(ctx)

	// 1. 模拟插话到达并广播
	ok := agent.Steer("用户插话：请优先关注错误日志")
	if !ok {
		t.Fatal("Steer 应返回 true")
	}

	// 验证事件流中收到 steer_accepted
	var gotSteerEvent bool
	select {
	case ev := <-stream:
		if ev.Type == types.AgentStreamEventSteerAccepted && strings.Contains(ev.Content, "错误日志") {
			gotSteerEvent = true
		}
	case <-time.After(time.Second):
		t.Fatal("未在规定时间内收到 steer_accepted 流事件")
	}
	if !gotSteerEvent {
		t.Fatal("流中未收到 steer_accepted 事件")
	}

	// 2. 模拟轮次边界消费
	steers := agent.drainSteerQueue()
	if len(steers) != 1 || steers[0] != "用户插话：请优先关注错误日志" {
		t.Fatalf("出队插话不符: %v", steers)
	}

	agent.sCtx.Mu.Lock()
	for _, st := range steers {
		agent.sCtx.LoopMessages = append(agent.sCtx.LoopMessages, types.Message{
			Role:    "user",
			Content: st,
		})
	}
	agent.sCtx.Mu.Unlock()

	agent.sCtx.Mu.Lock()
	defer agent.sCtx.Mu.Unlock()
	if len(agent.sCtx.LoopMessages) != 1 || agent.sCtx.LoopMessages[0].Role != "user" {
		t.Fatalf("插话必须作为 user 消息注入模型上下文: %+v", agent.sCtx.LoopMessages)
	}
}

// TestAgenticLoop_ObservationMasking_T3Batch 验证 T3 批量观察遮蔽与前缀稳定性。
func TestAgenticLoop_ObservationMasking_T3Batch(t *testing.T) {
	agent := NewAgentWithDefaults("test-obs-mask")
	agent.sCtx.LoopRound = 5
	agent.sCtx.LoopObservationMaskAfterRounds = 2
	agent.sCtx.LoopObservationMaskBatch = 2

	// 注入两条工具结果消息
	agent.sCtx.LoopMessages = []types.Message{
		{
			Role:    "user",
			Content: "large output 1",
			Parts: []any{
				map[string]any{"type": "tool_result", "tool_use_id": "c1", "content": "large output 1"},
			},
		},
		{
			Role:    "user",
			Content: "large output 2",
			Parts: []any{
				map[string]any{"type": "tool_result", "tool_use_id": "c2", "content": "large output 2"},
			},
		},
	}

	agent.applyLoopObservationMasking(context.Background())

	agent.sCtx.Mu.Lock()
	defer agent.sCtx.Mu.Unlock()

	if len(agent.sCtx.LoopMessages) != 2 {
		t.Fatalf("消息数不应改变，实际: %d", len(agent.sCtx.LoopMessages))
	}
	for i, m := range agent.sCtx.LoopMessages {
		if !strings.HasPrefix(m.Content, "[output masked: full output: read_tool_ref") {
			t.Fatalf("第 %d 条消息未被遮蔽: %q", i, m.Content)
		}
	}
}

// TestAgenticLoop_JITTokenVerification_NoBypass 验证未经 S_VALIDATE 校验的工具无法获得 JIT Token，防止旁路。
func TestAgenticLoop_JITTokenVerification_NoBypass(t *testing.T) {
	agent := NewAgentWithDefaults("test-jit-bypass")
	mockExec := &mockToolExecutor{}
	agent.InjectToolExecutor(mockExec)

	// 未经 a.recordValidatedPlan 记录的工具调用，直接执行必须被拦截
	call := types.InferToolCall{ID: "c_bypass", Name: "unauthorized_write", Input: []byte(`{"cmd":"rm"}`)}
	msg := agent.executeLoopToolCall(context.Background(), call)

	if !strings.Contains(msg.Content, "Error") || !strings.Contains(msg.Content, "refusing to mint capability token") {
		t.Fatalf("未授权工具必须被 JIT 拒绝签发令牌，返回: %s", msg.Content)
	}

	// 记录计划后，相同工具名与参数可正常签发并执行
	plan := &protocol.DAGPlan{
		Nodes: []protocol.ExecNode{
			{ID: "c_bypass", ToolName: "unauthorized_write", Args: []byte(`{"cmd":"rm"}`)},
		},
	}
	agent.recordValidatedPlan(plan)

	msgSuccess := agent.executeLoopToolCall(context.Background(), call)
	if strings.Contains(msgSuccess.Content, "refusing to mint capability token") {
		t.Fatalf("已验证计划中的工具应正常签发令牌并执行，实际返回: %s", msgSuccess.Content)
	}
}

// TestPool_TrySteer 验证 Pool.TrySteer 在 AgentStateLoop 态时直接接纳插话。
func TestPool_TrySteer(t *testing.T) {
	pool := NewPool(func(sessionID string) *Agent {
		return NewAgentWithDefaults(sessionID)
	}, 5)

	sessionID := "test-pool-steer-session"
	agentCtrl, release, err := pool.Acquire(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	defer release()

	ag, ok := agentCtrl.(*Agent)
	if !ok {
		t.Fatal("agentCtrl 应为 *Agent")
	}

	// 未在 Loop 态时 TrySteer 应返回 false
	if pool.TrySteer(sessionID, "steer text") {
		t.Fatal("非 Loop 状态下 TrySteer 不应接纳")
	}

	// 强制状态进入 AgentStateLoop
	ag.sm.ForceState(types.AgentStateLoop)

	// 再次 TrySteer 应返回 true
	if !pool.TrySteer(sessionID, "steer text in loop") {
		t.Fatal("Loop 状态下 TrySteer 必须接纳插话")
	}

	ag.sCtx.Mu.Lock()
	defer ag.sCtx.Mu.Unlock()
	if len(ag.sCtx.LoopSteerQueue) != 1 || ag.sCtx.LoopSteerQueue[0] != "steer text in loop" {
		t.Fatalf("插话队列内容不符: %v", ag.sCtx.LoopSteerQueue)
	}
}

type steerHookProvider struct {
	*loopTestProvider
	onInfer func(idx int)
}

func (p *steerHookProvider) StreamInfer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	if p.onInfer != nil {
		p.onInfer(p.callCount())
	}
	// custom-nolint:bare-infer 测试替身：只转发给被包装 provider，不是生产调用点
	return p.loopTestProvider.StreamInfer(ctx, msgs, opts...)
}

// TestAgenticLoop_SteerOnDirectTextCompletion 验证模型单轮无工具调用直接收尾时，
// 插话队列非空不得直接退出，必须注入插话并继续下一轮（R2-12）。
func TestAgenticLoop_SteerOnDirectTextCompletion(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	agent := NewAgentWithDefaults("test-steer-direct-text")
	rawProv := &loopTestProvider{
		responses: []*types.ProviderResponse{
			{Content: "你好！我是 polaris，请问有什么可以帮助您的？"},
			{Content: "收到补充说明，已为您处理。"},
		},
	}
	prov := &steerHookProvider{
		loopTestProvider: rawProv,
		onInfer: func(idx int) {
			if idx == 0 {
				// 在首轮推理执行中（AgentStateLoop 态）注入插话
				if !agent.Steer("请优先关注错误日志") {
					t.Error("Loop 态下 Steer 应成功入队")
				}
			}
		},
	}
	agent.InjectProvider(prov)
	agent.InjectPolicyGate(&allowPolicyGate{})
	agent.InjectToolExecutor(&mockToolExecutor{})

	done := make(chan struct{})
	go func() {
		_ = agent.Run(context.Background())
		close(done)
	}()

	if err := agent.SendIntent(types.TriggerIntentReceived); err != nil {
		t.Fatalf("SendIntent failed: %v", err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("agent run timeout")
	}

	if st := agent.sm.Current(); st != types.AgentStateComplete {
		t.Fatalf("agent 最终应完成，当前状态: %v", st)
	}

	// 必须触发了第 2 轮 LLM 调用来处理插话
	if prov.callCount() != 2 {
		t.Fatalf("期望 LLM 调用 2 次（第1轮文本回复+第2轮插话回复），实际调用次数: %d", prov.callCount())
	}

	agent.sCtx.Mu.Lock()
	defer agent.sCtx.Mu.Unlock()

	// 断言插话已被消费且进入 LoopMessages
	var foundSteerMsg bool
	for _, m := range agent.sCtx.LoopMessages {
		if m.Role == "user" && strings.Contains(m.Content, "错误日志") {
			foundSteerMsg = true
			break
		}
	}
	if !foundSteerMsg {
		t.Fatalf("LoopMessages 中未找到已消费的插话消息: %+v", agent.sCtx.LoopMessages)
	}
}

// TestPool_TrySteer_RaceCondition_TurnEnding 验证回合结束或收尾中时 TrySteer 返回 false（R2-12）。
func TestPool_TrySteer_RaceCondition_TurnEnding(t *testing.T) {
	pool := NewPool(func(sessionID string) *Agent {
		return NewAgentWithDefaults(sessionID)
	}, 5)

	sessionID := "test-pool-steer-ending"
	agentCtrl, release, err := pool.Acquire(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	defer release()

	ag := agentCtrl.(*Agent)
	ag.sm.ForceState(types.AgentStateLoop)

	// 1. Loop 状态下正常接纳
	if !pool.TrySteer(sessionID, "steer 1") {
		t.Fatal("正常 Loop 态应接纳插话")
	}

	// 2. 模拟回合收尾中（LoopEnding=true）
	ag.sCtx.Mu.Lock()
	ag.sCtx.LoopEnding = true
	ag.sCtx.Mu.Unlock()

	if pool.TrySteer(sessionID, "steer while ending") {
		t.Fatal("回合收尾中（LoopEnding=true）时 TrySteer 必须返回 false")
	}

	// 3. 终态（AgentStateComplete）
	ag.sCtx.Mu.Lock()
	ag.sCtx.LoopEnding = false
	ag.sCtx.Mu.Unlock()
	ag.sm.ForceState(types.AgentStateComplete)

	if pool.TrySteer(sessionID, "steer when complete") {
		t.Fatal("终态（AgentStateComplete）下 TrySteer 必须返回 false")
	}
}
