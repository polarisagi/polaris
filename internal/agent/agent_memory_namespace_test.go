package agent

import (
	"context"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

// TestAgent_MemoryPartitionKey_DefaultsToSessionID 验证未设置命名空间时，
// memoryPartitionKey() 返回 SessionID——与引入 GD-14-001 前完全一致的默认行为。
func TestAgent_MemoryPartitionKey_DefaultsToSessionID(t *testing.T) {
	agent := NewAgentWithDefaults("test-ns-default")
	agent.sCtx.SessionID = "session-abc"

	if got := agent.memoryPartitionKey(); got != "session-abc" {
		t.Errorf("expected memoryPartitionKey()=session-abc when namespace unset, got %q", got)
	}
}

// TestAgent_MemoryPartitionKey_UsesNamespaceWhenSet 验证 SetMemoryNamespace 后，
// memoryPartitionKey() 优先返回 NamespaceID 而非 SessionID。
func TestAgent_MemoryPartitionKey_UsesNamespaceWhenSet(t *testing.T) {
	agent := NewAgentWithDefaults("test-ns-set")
	agent.sCtx.SessionID = "session-abc"
	agent.SetMemoryNamespace("swarm-ns-shared")

	if got := agent.memoryPartitionKey(); got != "swarm-ns-shared" {
		t.Errorf("expected memoryPartitionKey()=swarm-ns-shared after SetMemoryNamespace, got %q", got)
	}
}

// TestAgent_WriteEpisodicWithExtract_TaggedWithNamespace 验证 GD-14-001 端到端
// 布线：两个模拟 Worker Agent（不同 SessionID，代表不同 Agent 实例）在
// SetMemoryNamespace 到相同命名空间后，写入的 episodic 事件 TaskID 均落在该
// 命名空间下——这正是 internal/memory/store.EpisodicMem.Query 现有的
// ev.TaskID==q.SessionID 过滤机制得以让二者共享记忆的前提（该机制本身已在
// internal/memory/store/episodic_namespace_test.go 独立验证）。
func TestAgent_WriteEpisodicWithExtract_TaggedWithNamespace(t *testing.T) {
	const sharedNamespace = "swarm-task-77"

	mem := &mockMemoryForIntegration{
		episodic: &mockEpisodicMemForIntegration{},
		working:  &mockWorkingMemForIntegration{immutable: &mockImmutableCoreForIntegration{}},
	}

	agentA := NewAgentWithDefaults("worker-a")
	agentA.sCtx.SessionID = "session-a"
	agentA.InjectMemory(mem)
	agentA.SetMemoryNamespace(sharedNamespace)

	agentB := NewAgentWithDefaults("worker-b")
	agentB.sCtx.SessionID = "session-b"
	agentB.InjectMemory(mem)
	agentB.SetMemoryNamespace(sharedNamespace)

	ctx := context.Background()
	agentA.writeEpisodicWithExtract(ctx, types.Event{
		ID:        "evt-a",
		Type:      "execution_completed",
		TaskID:    agentA.memoryPartitionKey(),
		Payload:   []byte(`{"from":"a"}`),
		CreatedAt: time.Now(),
	})
	agentB.writeEpisodicWithExtract(ctx, types.Event{
		ID:        "evt-b",
		Type:      "execution_completed",
		TaskID:    agentB.memoryPartitionKey(),
		Payload:   []byte(`{"from":"b"}`),
		CreatedAt: time.Now(),
	})

	if len(mem.episodic.events) != 2 {
		t.Fatalf("expected 2 events written to shared memory, got %d", len(mem.episodic.events))
	}
	for _, ev := range mem.episodic.events {
		if ev.TaskID != sharedNamespace {
			t.Errorf("expected event %s TaskID=%s (shared namespace), got %q", ev.ID, sharedNamespace, ev.TaskID)
		}
	}
}
