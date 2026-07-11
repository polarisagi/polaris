package agent

import (
	"context"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// TestAgent_MemoryPersistenceFailure_TriggersInterrupt 验证 GD-13-003 FSM 熔断：
// 当 Episodic 写入返回 CodeStorageUnavailable 时，Agent 必须经由
// asyncIntent(TriggerInterruptReceived) 进入 S_INTERRUPT（fsm/state_machine.go
// Dispatch() 中的状态无关全局处理分支），并设置 sCtx.SuspendReason，
// 而不是像旧实现那样静默吞掉错误继续在残缺状态上运行。
func TestAgent_MemoryPersistenceFailure_TriggersInterrupt(t *testing.T) {
	agent := NewAgentWithDefaults("test-mem-failure-agent")
	agent.InjectProvider(&mockProvider{})
	agent.InjectPolicyGate(&allowPolicyGate{})
	agent.InjectToolExecutor(&mockToolExecutor{})

	mem := &mockMemoryForIntegration{
		episodic: &mockEpisodicMemForIntegration{
			failWith: apperr.Wrap(apperr.CodeStorageUnavailable, "mock store put failed", apperr.New(apperr.CodeInternal, "disk full")),
		},
		working: &mockWorkingMemForIntegration{immutable: &mockImmutableCoreForIntegration{}},
	}
	agent.InjectMemory(mem)

	// 注意：agent.Shutdown() 取消的是 Agent 内部的 a.ctx（供 asyncIntent 的
	// SafeGo 使用），而非传给 Run() 的 ctx 参数——两者是不同的 context。
	// Run() 的退出依赖其收到的 ctx 参数被取消，因此这里必须显式构造可取消
	// 的 runCtx 并传给 Run，测试结束时 cancel() 它，而不能只调用 Shutdown()。
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		_ = agent.Run(runCtx)
		close(done)
	}()

	// 等待 Run 循环进入 select 就绪态（阻塞在 a.intent 上）。
	time.Sleep(50 * time.Millisecond)

	// 直接触发 episodic 写入路径（模拟任一执行阶段的事件写入失败）。
	agent.writeEpisodicWithExtract(context.Background(), types.Event{
		ID:     "ev-storage-fail-1",
		Type:   types.EventType("task_perceived"),
		TaskID: "t-mem-fail",
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if agent.StateMachine().Current() == types.AgentStateInterrupt {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := agent.StateMachine().Current(); got != types.AgentStateInterrupt {
		t.Fatalf("expected AgentStateInterrupt after memory persistence failure, got %v", got)
	}
	if agent.sCtx.SuspendReason != "memory_persistence_failure" {
		t.Errorf("expected SuspendReason=memory_persistence_failure, got %q", agent.sCtx.SuspendReason)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("agent.Run did not exit after ctx cancel")
	}
}

// TestAgent_MemoryPersistenceFailure_NonStorageErrorDoesNotInterrupt 验证熔断
// 只对 CodeStorageUnavailable 生效——序列化失败等其他 CodeInternal 错误只记日志，
// 不应误杀正在进行的执行链路（否则单条事件构造失败会导致整个任务被打断）。
func TestAgent_MemoryPersistenceFailure_NonStorageErrorDoesNotInterrupt(t *testing.T) {
	agent := NewAgentWithDefaults("test-mem-nonstorage-agent")
	agent.InjectProvider(&mockProvider{})
	agent.InjectPolicyGate(&allowPolicyGate{})
	agent.InjectToolExecutor(&mockToolExecutor{})

	mem := &mockMemoryForIntegration{
		episodic: &mockEpisodicMemForIntegration{
			failWith: apperr.Wrap(apperr.CodeInternal, "mock marshal failed", apperr.New(apperr.CodeInternal, "bad json")),
		},
		working: &mockWorkingMemForIntegration{immutable: &mockImmutableCoreForIntegration{}},
	}
	agent.InjectMemory(mem)

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		_ = agent.Run(runCtx)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)

	agent.writeEpisodicWithExtract(context.Background(), types.Event{
		ID:     "ev-internal-fail-1",
		Type:   types.EventType("task_perceived"),
		TaskID: "t-mem-nonstorage",
	})

	// 非存储层错误不应触发熔断：短暂等待后状态机应仍停留在 S_IDLE。
	time.Sleep(200 * time.Millisecond)
	if got := agent.StateMachine().Current(); got != types.AgentStateIdle {
		t.Errorf("expected state to remain Idle for non-storage error, got %v", got)
	}
	if agent.sCtx.SuspendReason == "memory_persistence_failure" {
		t.Errorf("SuspendReason should not be set for non-storage error")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("agent.Run did not exit after ctx cancel")
	}
}
