package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/execute/dag"
	"github.com/polarisagi/polaris/internal/observability/budget"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/tool/catalog"
	"github.com/polarisagi/polaris/pkg/types"
)

type mockToolExecutor struct {
	ExecuteWithTaintCalled bool
}

func (r *mockToolExecutor) Lookup(name string) (types.Tool, error) {
	se := []types.SideEffect{}
	if name == "non_idempotent_tool" {
		se = []types.SideEffect{types.SideFileWrite}
	}
	return types.Tool{Name: name, Source: types.ToolBuiltin, Capability: types.CapReadOnly, SideEffects: se}, nil
}
func (r *mockToolExecutor) ExecuteWithTaint(_ context.Context, name string, _ []byte, taintLevel types.TaintLevel) (*types.ToolResult, error) {
	r.ExecuteWithTaintCalled = true
	return &types.ToolResult{Success: true, Output: []byte(`{"ok":true}`)}, nil
}

func TestAgent_HappyPath(t *testing.T) {
	agent := NewAgentWithDefaults("test-agent-1")
	agent.InjectProvider(&mockProvider{})
	// 注入 allow-all PolicyGate，使 L1-Policy 通过
	agent.InjectPolicyGate(&allowPolicyGate{})
	// 注入 mockToolExecutor，使 S_EXECUTE 工具调用也通过
	mockExec := &mockToolExecutor{}
	agent.InjectToolExecutor(mockExec)
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
		// 验证 ExecuteWithTaint 被正确调用一次
		if !mockExec.ExecuteWithTaintCalled {
			t.Fatalf("expected ExecuteWithTaint to be called")
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

func (d *dummyContextBuilder) BuildPerceiveContext(ctx context.Context, memory protocol.MemoryFacade, sCtx *fsm.StateContext, cognitive fsm.CognitiveSearcher) ([]types.Message, error) {
	return nil, nil
}
func (d *dummyContextBuilder) BuildPlanContext(ctx context.Context, memory protocol.MemoryFacade, sCtx *fsm.StateContext, cata catalog.Catalog, cognitive fsm.CognitiveSearcher) ([]types.Message, error) {
	return nil, nil
}
func (d *dummyContextBuilder) BuildReflectContext(ctx context.Context, memory protocol.MemoryFacade, sCtx *fsm.StateContext) ([]types.Message, error) {
	return nil, nil
}
func (d *dummyContextBuilder) BuildToolListSection(ctx context.Context, cata catalog.Catalog) string {
	return ""
}

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

func (m *mockMemoryForIntegration) GetMemoryPressure() *budget.ResourceBudget {
	return &budget.ResourceBudget{}
}

func (m *mockMemoryForIntegration) StoreStats() (string, error) { return "{}", nil }

func (m *mockMemoryForIntegration) SearchEntities(ctx context.Context, query string, topK int, maxTaint int) ([]types.Entity, error) {
	return nil, nil
}
func (m *mockMemoryForIntegration) GetUserProfile(ctx context.Context, userID string) (*types.UserProfile, error) {
	return nil, nil
}
func (m *mockMemoryForIntegration) ListEpisodicEvents(ctx context.Context, query types.EpisodicQuery) ([]types.ScoredEvent, error) {
	return m.episodic.Query(ctx, query)
}
func (m *mockMemoryForIntegration) AppendEpisodicEvent(ctx context.Context, event types.Event, taintLevel types.TaintLevel) error {
	return m.episodic.Append(ctx, event, taintLevel)
}
func (m *mockMemoryForIntegration) ArchiveEpisodic(ctx context.Context, sessionID string) error {
	return nil
}
func (m *mockMemoryForIntegration) AddWorkingContext(ctx context.Context, text string) error {
	return nil
}
func (m *mockMemoryForIntegration) SetWorkingScratch(key string, val []byte) {}
func (m *mockMemoryForIntegration) ImmutableCore() protocol.ImmutableCore {
	return m.working.Immutable()
}
func (m *mockMemoryForIntegration) ListCoreMemory(ctx context.Context, agentID, sessionID string) ([]types.CoreMemoryBlock, error) {
	return nil, nil
}
func (m *mockMemoryForIntegration) ListReflections(ctx context.Context, q types.ReflectionQuery) ([]types.ReflectionEntry, error) {
	return nil, nil
}
func (m *mockMemoryForIntegration) AppendReflection(ctx context.Context, entry types.ReflectionEntry) error {
	return nil
}
func (m *mockMemoryForIntegration) ScanHighSalienceEvents(ctx context.Context, sinceID int64, minSalience float64, limit int) ([]types.SalienceEvent, error) {
	return nil, nil
}
func (m *mockMemoryForIntegration) PruneMemoryGraph(ctx context.Context) error { return nil }
func (m *mockMemoryForIntegration) TrackToolCall(toolUseID, toolName string)   {}
func (m *mockMemoryForIntegration) TrackToolResult(toolUseID string, success bool, summary string) {
}
func (m *mockMemoryForIntegration) RenderTaskCanvas() string { return "" }

type mockEpisodicMemForIntegration struct {
	events []types.Event
	// failWith 非 nil 时 Append 直接返回该错误，不追加事件——
	// 用于模拟存储层持久化失败（GD-13-003 FSM 熔断测试）。
	failWith error
}

func (m *mockEpisodicMemForIntegration) Append(ctx context.Context, ev types.Event, taint types.TaintLevel) error {
	if m.failWith != nil {
		return m.failWith
	}
	m.events = append(m.events, ev)
	return nil
}
func (m *mockEpisodicMemForIntegration) MarkCold(ctx context.Context, sessionID string, before time.Time) (int, error) {
	return 0, nil
}
func (m *mockEpisodicMemForIntegration) Query(ctx context.Context, q types.EpisodicQuery) ([]types.ScoredEvent, error) {
	return nil, nil
}
func (m *mockEpisodicMemForIntegration) ScanHighSalience(ctx context.Context, sinceID int64, minSalience float64, limit int) ([]types.SalienceEvent, error) {
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
func (m *mockImmutableCoreForIntegration) Fields() *protocol.ImmutableCoreFields {
	return &protocol.ImmutableCoreFields{}
}

func TestAgent_MemoryIntegration_HappyPath(t *testing.T) {
	agent := NewAgentWithDefaults("test-mem-agent")
	agent.InjectProvider(&mockProvider{})
	agent.InjectPolicyGate(&allowPolicyGate{})
	agent.InjectToolExecutor(&mockToolExecutor{})

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
