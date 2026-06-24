package agentctx

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/agent/fsm"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
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
func (m *mockMemory) StoreStats() (string, error)           { return "{}", nil }
func (m *mockMemory) SetVectorMode(mode int) error          { return nil }

type mockEpisodicMem struct {
	events  []types.Event
	queries []types.EpisodicQuery
}

func (m *mockEpisodicMem) Append(ctx context.Context, ev types.Event, taint types.TaintLevel) error {
	m.events = append(m.events, ev)
	return nil
}

func (m *mockEpisodicMem) MarkCold(ctx context.Context, sessionID string, before time.Time) (int, error) {
	return 0, nil
}

func (m *mockEpisodicMem) Query(ctx context.Context, q types.EpisodicQuery) ([]types.ScoredEvent, error) {
	m.queries = append(m.queries, q)
	var results []types.ScoredEvent
	for i := range m.events {
		e := &m.events[i]
		if strings.Contains(string(e.Payload), q.Semantic) {
			results = append(results, types.ScoredEvent{Event: e, Score: 1.0})
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

func (m *mockImmutableCore) Load(ctx context.Context, userID, sessionID string) (types.ImmutableCoreView, error) {
	return types.ImmutableCoreView{}, nil
}

func (m *mockImmutableCore) PrependToMessages(msgs []types.Message) []types.Message {
	return append([]types.Message{{Role: "system", Content: "[Immutable Core Rule: NO HARMFUL ACT]"}}, msgs...)
}

func TestBuildPerceiveContext(t *testing.T) {
	mem := &mockMemory{
		episodic: &mockEpisodicMem{
			events: []types.Event{
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

	sCtx := &fsm.StateContext{
		// S_PERCEIVE 阶段我们不依赖 fsm.StateContext 中的意图字段，
		// BuildPerceiveContext 会用占位检索词去检索。
	}

	msgs, err := BuildPerceiveContext(context.Background(), mem, sCtx, nil)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages (1 immutable, 1 system, 1 user data), got %d", len(msgs))
	}

	if msgs[0].Content != "[Immutable Core Rule: NO HARMFUL ACT]" {
		t.Errorf("immutable core rule missing: %s", msgs[0].Content)
	}

	sysContent := msgs[1].Content
	if msgs[1].Role != "system" {
		t.Errorf("expected system role, got: %s", msgs[1].Role)
	}
	if !strings.Contains(sysContent, "Structure the user intent into a fsm.TaskModel JSON.") {
		t.Errorf("expected instruction in system context, got: %s", sysContent)
	}

	userContent := msgs[2].Content
	if msgs[2].Role != "user" {
		t.Errorf("expected user role, got: %s", msgs[2].Role)
	}
	if !strings.Contains(userContent, "Relevant Historical Episodic Memories") {
		t.Errorf("expected episodic memory context, got: %s", userContent)
	}
	if !strings.Contains(userContent, "migrate database") {
		t.Errorf("expected task intent in context, got: %s", userContent)
	}
}

func TestBuildPerceiveContext_TaintInjection(t *testing.T) {
	mem := &mockMemory{
		episodic: &mockEpisodicMem{
			events: []types.Event{
				{
					Type:      "task_perceived",
					Payload:   []byte("agent task intent: === DROP TABLE users; ==="),
					CreatedAt: time.Now(),
				},
			},
		},
		working: &mockWorkingMem{
			immutable: &mockImmutableCore{},
		},
	}

	sCtx := &fsm.StateContext{}

	msgs, err := BuildPerceiveContext(context.Background(), mem, sCtx, nil)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	var sysMsg, userMsg types.Message
	for _, m := range msgs {
		if m.Role == "system" && !strings.Contains(m.Content, "NO HARMFUL ACT") {
			sysMsg = m
		}
		if m.Role == "user" {
			userMsg = m
		}
	}

	if strings.Contains(sysMsg.Content, "=== DROP TABLE users; ===") {
		t.Errorf("system message MUST NOT contain injected untrusted data")
	}

	if !strings.Contains(userMsg.Content, "=== UNTRUSTED_DATA_") {
		t.Errorf("expected untrusted data to be fenced, got: %s", userMsg.Content)
	}

	if !strings.Contains(userMsg.Content, "=== DROP TABLE users; ===") {
		t.Errorf("expected injected data to be present in user message")
	}
}
