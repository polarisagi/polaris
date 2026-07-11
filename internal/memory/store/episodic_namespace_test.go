package store

import (
	"context"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/memory/testutil"
	"github.com/polarisagi/polaris/pkg/types"
)

// TestEpisodicMem_NamespaceSharing 验证 GD-14-001 最小实现：多个"Agent"（不同
// AgentID）在 episodic 写入时使用相同的 TaskID（承载 NamespaceID 语义，见
// internal/agent/agent_wiring.go memoryPartitionKey）时，Query 按该 TaskID 过滤
// 能检索到彼此写入的记忆片段；不同 TaskID（不同命名空间/任务）之间保持隔离。
//
// 本测试不涉及 Agent/FSM 层，直接验证 EpisodicMem.Query 现有的
// ev.TaskID == q.SessionID 过滤机制（本身未作任何改动）在"多写入方共享同一
// TaskID"场景下的行为，这正是命名空间共享得以成立的底层机制。
func TestEpisodicMem_NamespaceSharing(t *testing.T) {
	store := testutil.NewMockStore()
	mem := NewEpisodicMemWithGraph(store, nil)
	ctx := context.Background()

	const sharedNamespace = "swarm-task-42"
	const otherNamespace = "swarm-task-99"

	// Worker Agent A 写入一条事件，TaskID 打上共享命名空间。
	evA := types.Event{
		ID:        "evt-agent-a",
		Type:      types.EventType("execution_completed"),
		TaskID:    sharedNamespace,
		AgentID:   "worker-a",
		Payload:   []byte(`{"finding":"row 1-50 all valid"}`),
		CreatedAt: time.Now(),
	}
	if err := mem.Append(ctx, evA, types.TaintNone); err != nil {
		t.Fatalf("Append (agent A) failed: %v", err)
	}

	// Worker Agent B（不同 AgentID）写入另一条事件，同样打上共享命名空间。
	evB := types.Event{
		ID:        "evt-agent-b",
		Type:      types.EventType("execution_completed"),
		TaskID:    sharedNamespace,
		AgentID:   "worker-b",
		Payload:   []byte(`{"finding":"row 51-100 found 2 anomalies"}`),
		CreatedAt: time.Now(),
	}
	if err := mem.Append(ctx, evB, types.TaintNone); err != nil {
		t.Fatalf("Append (agent B) failed: %v", err)
	}

	// 不同命名空间下的第三个 Agent 写入，不应污染 sharedNamespace 的检索结果。
	evC := types.Event{
		ID:        "evt-agent-c",
		Type:      types.EventType("execution_completed"),
		TaskID:    otherNamespace,
		AgentID:   "worker-c",
		Payload:   []byte(`{"finding":"unrelated task"}`),
		CreatedAt: time.Now(),
	}
	if err := mem.Append(ctx, evC, types.TaintNone); err != nil {
		t.Fatalf("Append (agent C) failed: %v", err)
	}

	// Agent B 按共享命名空间检索，应能看到 A 和 B 自己写入的两条记忆，看不到 C 的。
	sharedResults, err := mem.Query(ctx, types.EpisodicQuery{
		SessionID:     sharedNamespace,
		MaxTaintLevel: types.TaintHigh,
		K:             10,
	})
	if err != nil {
		t.Fatalf("Query (shared namespace) failed: %v", err)
	}
	if len(sharedResults) != 2 {
		t.Fatalf("expected 2 events visible in shared namespace, got %d: %+v", len(sharedResults), sharedResults)
	}
	seenIDs := map[string]bool{}
	for _, r := range sharedResults {
		seenIDs[r.Event.(*types.Event).ID] = true
	}
	if !seenIDs["evt-agent-a"] || !seenIDs["evt-agent-b"] {
		t.Errorf("expected both evt-agent-a and evt-agent-b visible, got: %v", seenIDs)
	}
	if seenIDs["evt-agent-c"] {
		t.Errorf("evt-agent-c (different namespace) leaked into shared namespace query")
	}

	// 不同命名空间的检索应完全隔离，只看到自己的记忆。
	otherResults, err := mem.Query(ctx, types.EpisodicQuery{
		SessionID:     otherNamespace,
		MaxTaintLevel: types.TaintHigh,
		K:             10,
	})
	if err != nil {
		t.Fatalf("Query (other namespace) failed: %v", err)
	}
	if len(otherResults) != 1 || otherResults[0].Event.(*types.Event).ID != "evt-agent-c" {
		t.Fatalf("expected only evt-agent-c visible in other namespace, got: %+v", otherResults)
	}
}

// TestEpisodicMem_NamespaceSharing_TaintStillEnforced 验证 GD-14-001 明确的安全
// 要求："Namespace 内共享不等于无限制共享"——命名空间共享只放宽了检索范围，
// 不能绕过既有的 TaintLevel 校验。同一命名空间内，高污点等级的事件仍应被
// MaxTaintLevel 过滤掉。
func TestEpisodicMem_NamespaceSharing_TaintStillEnforced(t *testing.T) {
	store := testutil.NewMockStore()
	mem := NewEpisodicMemWithGraph(store, nil)
	ctx := context.Background()

	const ns = "swarm-task-taint"

	lowTaintEv := types.Event{
		ID:         "evt-low-taint",
		Type:       types.EventType("execution_completed"),
		TaskID:     ns,
		AgentID:    "worker-a",
		Payload:    []byte(`{"finding":"public info"}`),
		TaintLevel: types.TaintLow,
		CreatedAt:  time.Now(),
	}
	highTaintEv := types.Event{
		ID:         "evt-high-taint",
		Type:       types.EventType("execution_completed"),
		TaskID:     ns,
		AgentID:    "worker-b",
		Payload:    []byte(`{"finding":"contains PII"}`),
		TaintLevel: types.TaintHigh,
		CreatedAt:  time.Now(),
	}
	if err := mem.Append(ctx, lowTaintEv, types.TaintNone); err != nil {
		t.Fatalf("Append (low taint) failed: %v", err)
	}
	if err := mem.Append(ctx, highTaintEv, types.TaintNone); err != nil {
		t.Fatalf("Append (high taint) failed: %v", err)
	}

	// 请求方 MaxTaintLevel 只到 TaintMedium：即使在同一共享命名空间内，高污点
	// 事件也必须被过滤掉，不能因为"同命名空间"而绕过 Taint 校验。
	results, err := mem.Query(ctx, types.EpisodicQuery{
		SessionID:     ns,
		MaxTaintLevel: types.TaintMedium,
		K:             10,
	})
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}
	if len(results) != 1 || results[0].Event.(*types.Event).ID != "evt-low-taint" {
		t.Fatalf("expected only low-taint event visible under MaxTaintLevel=TaintMedium, got: %+v", results)
	}
}
