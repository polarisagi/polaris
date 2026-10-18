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
