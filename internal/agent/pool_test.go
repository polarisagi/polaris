package agent

import (
	"context"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/execute/dag"
	"github.com/polarisagi/polaris/pkg/types"
)

// newTestPoolAgent 复用 agent_fsm_integration_test.go 的 mockProvider/
// allowPolicyGate/mockToolExecutor 快乐路径装配，构造一个能被 FSM 完整推进到
// Complete 终态的最小可用 Agent，供 Pool 测试作为 factory 使用。
func newTestPoolAgent(sessionID string) *Agent {
	a := NewAgentWithDefaults(sessionID)
	a.InjectProvider(&mockProvider{})
	a.InjectPolicyGate(&allowPolicyGate{})
	a.InjectToolExecutor(&mockToolExecutor{})
	a.sCtx.DAGModel = &fsm.DAGModel{
		Nodes: []dag.ExecNode{{ID: "n1", ToolName: "read_file"}},
	}
	return a
}

// TestPool_Acquire_StartsKernelRunLoop 验证 2026-07-12 复核修复的核心缺陷：
// 此前 Pool.Acquire 仅调用 p.factory(sessionID) 构造 Agent，从未启动其 Run()
// 事件循环——SendIntent 写入的是带缓冲 channel（cap=10），短期内不会报错，
// 但没有任何 goroutine 消费 a.intent，FSM 永远不会推进状态。全仓库此前没有
// 任何测试覆盖 Pool.Acquire + SendIntent 端到端链路（本文件是首个 pool_test.go），
// 这正是该缺口长期未被发现的原因。本测试完全通过 AgentController 接口驱动
// （不手动调用 agent.Run()，也不能访问到具体类型的 Run 方法），若 Pool 内部
// 没有自行启动 Run()，FSM 会永远停留在初始状态，测试超时失败。
func TestPool_Acquire_StartsKernelRunLoop(t *testing.T) {
	pool := NewPool(newTestPoolAgent, 4)
	ctrl, release, err := pool.Acquire(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	defer release()

	if err := ctrl.SendIntent(types.TriggerIntentReceived); err != nil {
		t.Fatalf("SendIntent failed: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ctrl.CurrentState() == types.AgentStateComplete {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for FSM to reach Complete (got %v) — Run() likely never started consuming intents", ctrl.CurrentState())
}

// TestPool_Acquire_SecondSessionAlsoRuns 验证第二个（不同 sessionID）并发会话
// 同样能被独立启动的 Run() 循环正确驱动到终态——回归验证 newPoolEntry 的
// SafeGo 启动逻辑对每个 session 各自生效，不会互相干扰或共享同一个内核循环。
func TestPool_Acquire_SecondSessionAlsoRuns(t *testing.T) {
	pool := NewPool(newTestPoolAgent, 4)

	ctrl1, release1, err := pool.Acquire(context.Background(), "sess-a")
	if err != nil {
		t.Fatalf("Acquire sess-a failed: %v", err)
	}
	defer release1()
	ctrl2, release2, err := pool.Acquire(context.Background(), "sess-b")
	if err != nil {
		t.Fatalf("Acquire sess-b failed: %v", err)
	}
	defer release2()

	if err := ctrl1.SendIntent(types.TriggerIntentReceived); err != nil {
		t.Fatalf("SendIntent sess-a failed: %v", err)
	}
	if err := ctrl2.SendIntent(types.TriggerIntentReceived); err != nil {
		t.Fatalf("SendIntent sess-b failed: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ctrl1.CurrentState() == types.AgentStateComplete && ctrl2.CurrentState() == types.AgentStateComplete {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for both sessions to reach Complete (sess-a=%v, sess-b=%v)",
		ctrl1.CurrentState(), ctrl2.CurrentState())
}

// TestPool_GC_ShutsDownIdleAgent 验证 2026-07-12 复核修复的配套问题：一旦
// newPoolEntry 启动了常驻 Run() goroutine，GC() 若只从 p.sessions 摘除 entry
// 而不调用 agent.Shutdown()，会把"FSM 从未运行"的缺陷变成一个新的"goroutine
// 永久泄漏"缺陷——Suspend-on-Idle 循环在 Suspended 状态下会持续阻塞在
// a.intent/idleTimer 上等待，不会自行退出。本测试绕过 Acquire/release 的
// Interrupt-Abort 兜底路径，直接构造一个"从未 SendIntent、仍停留在 Idle 状态"
// 的 idle entry，聚焦验证 GC 自身的清理职责。
func TestPool_GC_ShutsDownIdleAgent(t *testing.T) {
	pool := NewPool(newTestPoolAgent, 4)
	entry := pool.newPoolEntry("sess-gc")

	pool.mu.Lock()
	pool.sessions["sess-gc"] = entry
	entry.refs = 0
	entry.lastUsed = time.Now().Add(-pool.idleTimeout - time.Second)
	pool.mu.Unlock()

	// GC 前：kernel goroutine 应仍在运行（Idle 状态，从未 SendIntent）。
	select {
	case <-entry.agent.Done():
		t.Fatal("agent kernel should still be running before GC")
	case <-time.After(50 * time.Millisecond):
	}

	pool.GC()

	select {
	case <-entry.agent.Done():
		// 期望：GC 清理 idle entry 时必须 Shutdown() 停止其 Run() goroutine。
	case <-time.After(2 * time.Second):
		t.Fatal("expected GC to Shutdown() the idle agent's kernel goroutine (goroutine leak)")
	}

	pool.mu.Lock()
	_, stillExists := pool.sessions["sess-gc"]
	pool.mu.Unlock()
	if stillExists {
		t.Fatal("expected GC to evict the idle session entry from p.sessions")
	}
}

// TestPool_GC_KeepsActiveSessions 回归验证 GC 不误伤仍在使用（refs>0）或未超
// idleTimeout 的 session。
func TestPool_GC_KeepsActiveSessions(t *testing.T) {
	pool := NewPool(newTestPoolAgent, 4)
	ctrl, release, err := pool.Acquire(context.Background(), "sess-active")
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	defer release()
	_ = ctrl

	pool.GC()

	pool.mu.Lock()
	_, stillExists := pool.sessions["sess-active"]
	pool.mu.Unlock()
	if !stillExists {
		t.Fatal("expected GC to keep a session still held by an active Acquire (refs>0)")
	}
}
