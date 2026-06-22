package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/agent/dag"
	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

type mockToolRegistry struct{}

func (r *mockToolRegistry) Register(_ types.Tool) error { return nil }
func (r *mockToolRegistry) Lookup(name string) (types.Tool, error) {
	return types.Tool{Name: name, Source: types.ToolBuiltin, Capability: types.CapReadOnly}, nil
}
func (r *mockToolRegistry) List() []types.Tool { return nil }
func (r *mockToolRegistry) ExecuteTool(_ context.Context, name string, _ []byte, taintLevel types.TaintLevel) (*types.ToolResult, error) {
	return &types.ToolResult{Success: true, Output: []byte(`{"ok":true}`)}, nil
}

func TestAgent_HappyPath(t *testing.T) {
	agent := NewAgentWithDefaults("test-agent-1")
	agent.InjectProvider(&mockProvider{})
	// 注入 allow-all PolicyGate，使 L1-Policy 通过
	agent.InjectPolicyGate(&allowPolicyGate{})
	// 注入 mockToolRegistry，使 S_EXECUTE 工具调用也通过
	agent.InjectToolRegistry(&mockToolRegistry{})
	// 注入合法 fsm.DAGModel，使 L0/L1 通过后 ValidateOk 被推送
	agent.sCtx.DAGModel = &fsm.DAGModel{
		Nodes: []dag.ExecNode{{ID: "n1", ToolName: "read_file"}},
	}

	// 监听状态机完成
	done := make(chan struct{})
	go func() {
		err := agent.Run(context.Background())
		if err != nil {
			t.Errorf("agent run failed: %v", err)
		}
		close(done)
	}()

	// 发送启动信号
	agent.SendIntent(types.TriggerIntentReceived)

	select {
	case <-done:
		// 校验终态
		if agent.StateMachine().Current() != types.AgentStateComplete {
			t.Errorf("expected state Complete, got %v", agent.StateMachine().Current())
		}

		// 校验历史链路
		history := agent.StateMachine().History()
		expectedTransitions := []types.AgentState{
			types.AgentStateIdle,
			types.AgentStatePerceive,
			types.AgentStatePlan,
			types.AgentStateValidate,
			types.AgentStateExecute,
			types.AgentStateReflect,
		}
		if len(history) != len(expectedTransitions) {
			t.Fatalf("history length mismatch, got %v, want %v, history: %v", len(history), len(expectedTransitions), history)
		}
		for i, h := range history {
			if h != expectedTransitions[i] {
				t.Errorf("history[%d] mismatch: got %v, want %v", i, h, expectedTransitions[i])
			}
		}

	case <-time.After(3 * time.Second):
		t.Fatal("agent run timeout")
	}
}

func TestAgent_ReplanExhausted(t *testing.T) {
	agent := NewAgentWithDefaults("test-agent-2")
	// 强制在 Perceive 阶段无限失败
	agent.InjectProvider(&mockProvider{failOn: "将用户意图结构化为 TaskModel JSON"})
	agent.sCtx.MaxReplan = 3

	// 直接验证它能自动完成 run，并在错误之后走到 FAILED。
	// 这里通过 Run 的自动驱动
	done := make(chan error)
	go func() {
		done <- agent.Run(context.Background())
	}()

	agent.SendIntent(types.TriggerIntentReceived)

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, ErrReplanExhausted) {
			t.Errorf("expected ErrReplanExhausted or nil, got: %v", err)
		}
		if agent.StateMachine().Current() != types.AgentStateFailed {
			t.Errorf("expected state Failed, got %v", agent.StateMachine().Current())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("agent run timeout during exhausted")
	}
}

type dummyContextBuilder struct{}

func (d *dummyContextBuilder) BuildPerceiveContext(ctx context.Context, memory protocol.Memory, sCtx *fsm.StateContext, cognitive fsm.CognitiveSearcher) ([]types.Message, error) {
	return nil, nil
}
func (d *dummyContextBuilder) BuildPlanContext(ctx context.Context, memory protocol.Memory, sCtx *fsm.StateContext, tools protocol.ToolRegistry, cognitive fsm.CognitiveSearcher) ([]types.Message, error) {
	return nil, nil
}
func (d *dummyContextBuilder) BuildReflectContext(ctx context.Context, memory protocol.Memory, sCtx *fsm.StateContext) ([]types.Message, error) {
	return nil, nil
}
func (d *dummyContextBuilder) BuildToolListSection(tools protocol.ToolRegistry) string { return "" }

// allowPolicyGate 放行所有请求（用于 Agent HappyPath 测试）。
type allowPolicyGate struct{}

func (g *allowPolicyGate) IsAuthorized(_ context.Context, _, _, _ string, _ map[string]any) (bool, error) {
	return true, nil
}
func (g *allowPolicyGate) Review(_ context.Context, req types.PolicyReviewRequest) (types.PolicyReviewResult, error) {
	return types.PolicyReviewResult{Allowed: true}, nil
}

// mockMemoryForIntegration provides a minimal memory implementation for integration testing.
type mockMemoryForIntegration struct {
	episodic *mockEpisodicMemForIntegration
	working  *mockWorkingMemForIntegration
}

func (m *mockMemoryForIntegration) Working() protocol.WorkingMemory       { return m.working }
func (m *mockMemoryForIntegration) Episodic() protocol.EpisodicMemory     { return m.episodic }
func (m *mockMemoryForIntegration) Semantic() protocol.SemanticMemory     { return nil }
func (m *mockMemoryForIntegration) Procedural() protocol.ProceduralMemory { return nil }
func (m *mockMemoryForIntegration) Retriever() protocol.HybridRetriever   { return nil }
func (m *mockMemoryForIntegration) Reflection() protocol.ReflectionMemory { return nil }
func (m *mockMemoryForIntegration) StoreStats() (string, error)           { return "{}", nil }
func (m *mockMemoryForIntegration) SetVectorMode(mode int) error          { return nil }

type mockEpisodicMemForIntegration struct {
	events []types.Event
}

func (m *mockEpisodicMemForIntegration) Append(ctx context.Context, ev types.Event) error {
	m.events = append(m.events, ev)
	return nil
}
func (m *mockEpisodicMemForIntegration) MarkCold(ctx context.Context, sessionID string, before time.Time) (int, error) {
	return 0, nil
}
func (m *mockEpisodicMemForIntegration) Query(ctx context.Context, q types.EpisodicQuery) ([]types.ScoredEvent, error) {
	return nil, nil
}

type mockWorkingMemForIntegration struct {
	immutable *mockImmutableCoreForIntegration
}

func (m *mockWorkingMemForIntegration) Immutable() protocol.ImmutableCore { return m.immutable }
func (m *mockWorkingMemForIntegration) Context() protocol.ContextWindow   { return nil }
func (m *mockWorkingMemForIntegration) Scratch() protocol.ScratchPad      { return nil }
func (m *mockWorkingMemForIntegration) Notes() protocol.NotesStore        { return nil }

type mockImmutableCoreForIntegration struct{}

func (m *mockImmutableCoreForIntegration) Load(ctx context.Context, userID, sessionID string) (types.ImmutableCoreView, error) {
	return types.ImmutableCoreView{}, nil
}
func (m *mockImmutableCoreForIntegration) PrependToMessages(msgs []types.Message) []types.Message {
	return append([]types.Message{{Role: "system", Content: "[Immutable Core Rule: NO HARMFUL ACT]"}}, msgs...)
}

func TestAgent_MemoryIntegration_HappyPath(t *testing.T) {
	agent := NewAgentWithDefaults("test-mem-agent")
	agent.InjectProvider(&mockProvider{})
	agent.InjectPolicyGate(&allowPolicyGate{})
	agent.InjectToolRegistry(&mockToolRegistry{})

	mem := &mockMemoryForIntegration{
		episodic: &mockEpisodicMemForIntegration{},
		working:  &mockWorkingMemForIntegration{immutable: &mockImmutableCoreForIntegration{}},
	}
	agent.InjectMemory(mem)

	agent.sCtx.DAGModel = &fsm.DAGModel{
		Nodes: []dag.ExecNode{{ID: "n1", ToolName: "read_file"}},
	}
	agent.sCtx.ExecuteResult = []byte("cluster deployed successfully")

	done := make(chan struct{})
	go func() {
		_ = agent.Run(context.Background())
		close(done)
	}()

	agent.SendIntent(types.TriggerIntentReceived)

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
