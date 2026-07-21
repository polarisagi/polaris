package agent

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// fakeOutboxWriter 记录每次 Write 调用的 IdempotencyKey，供幂等键唯一性断言。
type fakeOutboxWriter struct {
	keys []string
}

func (w *fakeOutboxWriter) Write(_ context.Context, entry protocol.OutboxEntry) error {
	w.keys = append(w.keys, entry.IdempotencyKey)
	return nil
}

// TestRecordLLMFillEffectMemory_OutboxIdempotencyKeyUniqueAcrossTurns 验证
// 2026-07-22 一致性审查修复：同一 (sessionID, agentID) 组合在"多轮对话"场景
// 下（Pool 在每轮终态后为同一 sessionID 重新构造全新 Agent 实例，
// AgentID/SessionID 跨轮次保持不变——见 NewAgent），S_PERCEIVE_DONE 触发的
// 记忆投影（TopicEpisodicProject）与语义抽取（TopicEpisodicExtract）所用的
// outbox 幂等键，在两轮"看起来完全一样"的调用之间不再相同。
//
// 修复前：
//   - TopicEpisodicProject 键固定为 "{sessionID}:perceive:{agentID}"；
//   - TopicEpisodicExtract 键为 "{ev.ID}:extract"，而 ev.ID 由每个 Agent 实例
//     私有的 sm.eventSeq 计数器生成、新实例重置为 0，两轮的 ev.ID 会重复。
//
// 两者都会撞 outbox.idempotency_key 的 UNIQUE 约束，导致第二轮起写入被
// `_ = ...Write(...)` 静默丢弃。
func TestRecordLLMFillEffectMemory_OutboxIdempotencyKeyUniqueAcrossTurns(t *testing.T) {
	protocol.SetReplayMode(false) // 确保未处于其他测试遗留的回放模式

	mem := &mockMemoryForIntegration{
		episodic: &mockEpisodicMemForIntegration{},
		working:  &mockWorkingMemForIntegration{immutable: &mockImmutableCoreForIntegration{}},
	}
	resp := &types.ProviderResponse{Content: "mock content"}

	newAgentForTurn := func() (*Agent, *fakeOutboxWriter) {
		a := NewAgentWithDefaults("sess-idem-test") // AgentID == SessionID == "sess-idem-test"，跨"轮次"不变
		a.InjectMemory(mem)
		ow := &fakeOutboxWriter{}
		a.InjectOutboxWriter(ow)
		return a, ow
	}

	// 模拟"第一轮对话"：一个全新 Agent 实例上触发一次 S_PERCEIVE_DONE 投影。
	agent1, ow1 := newAgentForTurn()
	agent1.recordLLMFillEffectMemory(context.Background(), "S_PERCEIVE_DONE", resp)

	// 模拟"第二轮对话"：同 sessionID 的全新 Agent 实例，同样触发一次
	// S_PERCEIVE_DONE 投影——这正是此前会因幂等键相同而被静默吞掉的场景。
	agent2, ow2 := newAgentForTurn()
	agent2.recordLLMFillEffectMemory(context.Background(), "S_PERCEIVE_DONE", resp)

	if len(ow1.keys) == 0 || len(ow2.keys) == 0 {
		t.Fatalf("expected at least 1 outbox write per turn, got %d and %d", len(ow1.keys), len(ow2.keys))
	}
	if len(ow1.keys) != len(ow2.keys) {
		t.Fatalf("expected identical write count across two structurally identical turns, got %d and %d", len(ow1.keys), len(ow2.keys))
	}

	seen := make(map[string]bool, len(ow1.keys)+len(ow2.keys))
	for _, k := range ow1.keys {
		if seen[k] {
			t.Errorf("duplicate idempotency key %q within turn 1 alone", k)
		}
		seen[k] = true
	}
	for _, k := range ow2.keys {
		if seen[k] {
			t.Errorf("idempotency key %q from turn 2 collides with a key already used in turn 1 — outbox UNIQUE constraint would silently drop this write", k)
		}
		seen[k] = true
	}
}

// TestRecordLLMFillEffectMemory_ReflectAndConsolidateIdempotencyKeysUniqueAcrossTurns
// 同上，针对 S_REFLECT_DONE 触发的反思投影（TopicEpisodicProject）+ 语义抽取
// （TopicEpisodicExtract）+ 记忆蒸馏触发（TopicMemoryConsolidate）三条 outbox
// 写入路径。
func TestRecordLLMFillEffectMemory_ReflectAndConsolidateIdempotencyKeysUniqueAcrossTurns(t *testing.T) {
	protocol.SetReplayMode(false)

	mem := &mockMemoryForIntegration{
		episodic: &mockEpisodicMemForIntegration{},
		working:  &mockWorkingMemForIntegration{immutable: &mockImmutableCoreForIntegration{}},
	}
	resp := &types.ProviderResponse{Content: "mock reflection"}

	newAgentForTurn := func() (*Agent, *fakeOutboxWriter) {
		a := NewAgentWithDefaults("sess-consolidate-test")
		a.InjectMemory(mem)
		ow := &fakeOutboxWriter{}
		a.InjectOutboxWriter(ow)
		return a, ow
	}

	agent1, ow1 := newAgentForTurn()
	agent1.recordLLMFillEffectMemory(context.Background(), "S_REFLECT_DONE", resp)

	agent2, ow2 := newAgentForTurn()
	agent2.recordLLMFillEffectMemory(context.Background(), "S_REFLECT_DONE", resp)

	if len(ow1.keys) == 0 || len(ow2.keys) == 0 {
		t.Fatalf("expected at least 1 outbox write per turn, got %d and %d", len(ow1.keys), len(ow2.keys))
	}
	if len(ow1.keys) != len(ow2.keys) {
		t.Fatalf("expected identical write count across two structurally identical turns, got %d and %d", len(ow1.keys), len(ow2.keys))
	}

	seen := make(map[string]bool, len(ow1.keys)+len(ow2.keys))
	for _, k := range ow1.keys {
		if seen[k] {
			t.Errorf("duplicate idempotency key %q within turn 1 alone", k)
		}
		seen[k] = true
	}
	for _, k := range ow2.keys {
		if seen[k] {
			t.Errorf("idempotency key %q from turn 2 collides with a key already used in turn 1 — outbox UNIQUE constraint would silently drop this write", k)
		}
		seen[k] = true
	}
}

// TestOutboxUniqueSuffix_UniqueUnderTightLoop 验证 outboxUniqueSuffix 即使在
// 同一 goroutine 内背靠背连续调用（时钟粒度可能不足以让 UnixNano() 变化）也
// 不会产生重复值——这正是 recordLLMFillEffectMemory 的 reflect 分支紧接着
// 触发 consolidate 分支时的真实调用模式。
func TestOutboxUniqueSuffix_UniqueUnderTightLoop(t *testing.T) {
	const n = 1000
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		s := outboxUniqueSuffix()
		if seen[s] {
			t.Fatalf("outboxUniqueSuffix produced duplicate value %q on iteration %d", s, i)
		}
		seen[s] = true
	}
}
