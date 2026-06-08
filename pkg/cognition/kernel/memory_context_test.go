package kernel

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
)

// mockMemory 用于测试记忆上下文组装
type mockMemory struct {
	episodic *mockEpisodicMem
	working  *mockWorkingMem
}

func (m *mockMemory) Working() protocol.WorkingMemory       { return m.working }
func (m *mockMemory) Episodic() protocol.EpisodicMemory     { return m.episodic }
func (m *mockMemory) Semantic() protocol.SemanticMemory     { return nil }
func (m *mockMemory) Procedural() protocol.ProceduralMemory { return nil }
func (m *mockMemory) Retriever() protocol.HybridRetriever   { return nil }
func (m *mockMemory) Reflection() protocol.ReflectionMemory { return nil }

type mockEpisodicMem struct {
	events  []protocol.Event
	queries []protocol.EpisodicQuery
}

func (m *mockEpisodicMem) Append(ctx context.Context, ev protocol.Event) error {
	m.events = append(m.events, ev)
	return nil
}

func (m *mockEpisodicMem) Query(ctx context.Context, q protocol.EpisodicQuery) ([]protocol.ScoredEvent, error) {
	m.queries = append(m.queries, q)
	var results []protocol.ScoredEvent
	for _, e := range m.events {
		if strings.Contains(string(e.Payload), q.Semantic) {
			results = append(results, protocol.ScoredEvent{Event: e, Score: 1.0})
		}
	}
	return results, nil
}

type mockWorkingMem struct {
	immutable *mockImmutableCore
}

func (m *mockWorkingMem) Immutable() protocol.ImmutableCore { return m.immutable }
func (m *mockWorkingMem) Context() protocol.ContextWindow   { return nil }
func (m *mockWorkingMem) Scratch() protocol.ScratchPad      { return nil }
func (m *mockWorkingMem) Notes() protocol.NotesStore        { return nil }

type mockImmutableCore struct{}

func (m *mockImmutableCore) Load(ctx context.Context, userID, sessionID string) (protocol.ImmutableCoreView, error) {
	return protocol.ImmutableCoreView{}, nil
}

func (m *mockImmutableCore) PrependToMessages(msgs []protocol.Message) []protocol.Message {
	return append([]protocol.Message{{Role: "system", Content: "[Immutable Core Rule: NO HARMFUL ACT]"}}, msgs...)
}

func TestBuildPerceiveContext(t *testing.T) {
	mem := &mockMemory{
		episodic: &mockEpisodicMem{
			events: []protocol.Event{
				{
					Type:      "task_perceived",
					Payload:   []byte("agent task intent: migrate database"),
					CreatedAt: time.Now(),
				},
			},
		},
		working: &mockWorkingMem{
			immutable: &mockImmutableCore{},
		},
	}

	sCtx := &StateContext{
		// S_PERCEIVE 阶段我们不依赖 StateContext 中的意图字段，
		// buildPerceiveContext 会用占位检索词去检索。
	}

	msgs, err := buildPerceiveContext(context.Background(), mem, sCtx)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (1 immutable, 1 generated), got %d", len(msgs))
	}

	if msgs[0].Content != "[Immutable Core Rule: NO HARMFUL ACT]" {
		t.Errorf("immutable core rule missing: %s", msgs[0].Content)
	}

	content := msgs[1].Content
	if !strings.Contains(content, "Relevant Historical Episodic Memories") {
		t.Errorf("expected episodic memory context, got: %s", content)
	}
	if !strings.Contains(content, "migrate database") {
		t.Errorf("expected task intent in context, got: %s", content)
	}
}

func TestAgent_MemoryIntegration_HappyPath(t *testing.T) {
	agent := NewAgentWithDefaults("test-mem-agent")
	agent.InjectProvider(&mockProvider{})
	agent.InjectPolicyGate(&allowPolicyGate{})
	agent.InjectToolRegistry(&mockToolRegistry{})

	mem := &mockMemory{
		episodic: &mockEpisodicMem{},
		working:  &mockWorkingMem{immutable: &mockImmutableCore{}},
	}
	agent.InjectMemory(mem)

	agent.sCtx.DAGModel = &DAGModel{
		Nodes: []ExecNode{{ID: "n1", ToolName: "read_file"}},
	}
	// 执行过程中会注入 ExecuteResult
	agent.sCtx.ExecuteResult = []byte("cluster deployed successfully")

	done := make(chan struct{})
	go func() {
		_ = agent.Run(context.Background())
		close(done)
	}()

	// 触发全流转
	agent.SendIntent(protocol.TriggerIntentReceived)

	select {
	case <-done:
		// wait
	case <-time.After(2 * time.Second):
		t.Fatal("agent run timeout")
	}

	// 验证 EpisodicMemory 的写入记录
	if len(mem.episodic.events) < 3 {
		t.Errorf("expected at least 3 episodic events (perceive, plan, exec), got %d", len(mem.episodic.events))
	}

	var perceiveFound, planFound, execFound bool
	for _, e := range mem.episodic.events {
		if e.Type == "task_perceived" {
			perceiveFound = true
		}
		if e.Type == "plan_generated" {
			planFound = true
		}
		if e.Type == "execution_completed" {
			execFound = true
			if string(e.Payload) != `{"ok":true}` {
				t.Errorf("unexpected execution payload: %s", e.Payload)
			}
		}
	}

	if !perceiveFound || !planFound || !execFound {
		t.Errorf("missing memory events: perceive=%v, plan=%v, exec=%v", perceiveFound, planFound, execFound)
	}
}
