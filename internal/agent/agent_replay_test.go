package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/execute/dag"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// reconstructReplayResponse
// ============================================================================

func TestReconstructReplayResponse_NilMap(t *testing.T) {
	resp := reconstructReplayResponse(nil)
	if resp == nil || resp.Content != "" {
		t.Fatalf("expected zero-value ProviderResponse for nil map, got %+v", resp)
	}
}

func TestReconstructReplayResponse_RoundTripsFields(t *testing.T) {
	m := map[string]any{
		"content":           "hello world",
		"reasoning_content": "because reasons",
		// usage/tool_calls 模拟 TrajectoryRecorderImpl 从 JSON 泛读回来的形状：
		// 嵌套对象经 json.Unmarshal 进 map[string]any{} 后，字段名与写入时的
		// Go 结构体导出字段名一致（types.Usage/types.InferToolCall 均无 json tag）。
		"usage": map[string]any{
			"InputTokens":  float64(10),
			"OutputTokens": float64(20),
		},
		"tool_calls": []any{
			map[string]any{"ID": "call_1", "Name": "fetch_url"},
		},
	}
	resp := reconstructReplayResponse(m)
	if resp.Content != "hello world" {
		t.Errorf("Content mismatch: %q", resp.Content)
	}
	if resp.ReasoningContent != "because reasons" {
		t.Errorf("ReasoningContent mismatch: %q", resp.ReasoningContent)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 20 {
		t.Errorf("Usage mismatch: %+v", resp.Usage)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "fetch_url" {
		t.Errorf("ToolCalls mismatch: %+v", resp.ToolCalls)
	}
}

// ============================================================================
// executeEffect 崩溃恢复回放替换 + 队列耗尽自动切回真实调用
// ============================================================================

// failIfCalledProvider 断言"不应被调用"的 Provider——真实 Infer/StreamInfer
// 一旦被触发即记录调用次数，供测试判定回放替换是否真的短路了真实 LLM 调用。
type failIfCalledProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *failIfCalledProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *failIfCalledProvider) Infer(_ context.Context, _ []types.Message, _ ...types.InferOption) (*types.ProviderResponse, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return &types.ProviderResponse{Content: "should-not-be-called"}, nil
}

func (p *failIfCalledProvider) StreamInfer(_ context.Context, _ []types.Message, _ ...types.InferOption) (<-chan types.StreamEvent, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	ch := make(chan types.StreamEvent, 1)
	ch <- types.StreamEvent{Type: types.StreamTextDelta, Content: "should-not-be-called"}
	close(ch)
	return ch, nil
}

func (p *failIfCalledProvider) Capabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{}
}
func (p *failIfCalledProvider) Tokenizer() protocol.TokenizerAdapter { return nil }
func (p *failIfCalledProvider) ModelID() string                      { return "fail-if-called" }

// planDAGJSON 与 prm_test.go mockProvider 默认 S_PLAN 分支返回的 DAG JSON 一致
// （action 名 "test_tool" 对应 mockToolExecutor 无条件放行任意工具名）。
const planDAGJSON = `{"nodes":[{"id":"n1","action":"test_tool","params":{},"retry":0,"timeout":""}],"edges":[]}`

// TestAgent_ReplayMode_FullTrajectory_NoRealCallsNoDuplicateToolExec 验证
// M04 §8 崩溃恢复回放的核心安全属性：当录像覆盖 Perceive/Plan/Reflect 全部
// 三次 LLM 调用时（对应"崩溃发生在 S_REFLECT 之后、Complete 状态转移事件
// 落盘之前"这一安全窗口），重放期间：
//  1. 真实 Provider 一次都不应被调用（全部由录像覆盖）；
//  2. S_EXECUTE 的真实工具调用不应发生——原始崩溃会话里这一步已经真实执行
//     过（否则不会有后续的 Reflect 调用被录制），重放时必须短路而不是重复
//     执行副作用（executor_node.go 的 IsReplaying 物理短路）；
//  3. FSM 最终仍能凭借录像 + 队列耗尽后的真实收尾推进到 Complete 终态；
//  4. 队列耗尽后全局 ReplayMode 被正确复位为 false。
func TestAgent_ReplayMode_FullTrajectory_NoRealCallsNoDuplicateToolExec(t *testing.T) {
	agent := NewAgentWithDefaults("test-replay-full")
	provider := &failIfCalledProvider{}
	agent.InjectProvider(provider)
	agent.InjectPolicyGate(&allowPolicyGate{})
	mockExec := &mockToolExecutor{}
	agent.InjectToolExecutor(mockExec)
	agent.sCtx.DAGModel = &fsm.DAGModel{
		Nodes: []dag.ExecNode{{ID: "n1", ToolName: "read_file"}},
	}

	agent.InjectReplayData([]protocol.ReplayLLMCall{
		{Response: map[string]any{"content": "mock_success"}}, // S_PERCEIVE
		{Response: map[string]any{"content": planDAGJSON}},    // S_PLAN
		{Response: map[string]any{"content": "mock_success"}}, // S_REFLECT
	})
	protocol.SetReplayMode(true)
	defer protocol.SetReplayMode(false) // 测试失败提前返回时的安全网，不影响其余测试的全局状态

	done := make(chan struct{})
	go func() {
		_ = agent.Run(context.Background())
		close(done)
	}()
	agent.SendIntent(types.TriggerIntentReceived)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("agent run timeout")
	}

	if got := provider.callCount(); got != 0 {
		t.Errorf("expected 0 real provider calls (fully covered by replay), got %d", got)
	}
	if mockExec.ExecuteWithTaintCalled {
		t.Error("expected S_EXECUTE real tool call to be short-circuited during replay (already happened before crash)")
	}
	if agent.StateMachine().Current() != types.AgentStateComplete {
		t.Errorf("expected AgentStateComplete, got %v", agent.StateMachine().Current())
	}
	if protocol.IsReplaying() {
		t.Error("expected global ReplayMode to be flipped false after replay queue exhaustion")
	}
}

// TestAgent_ReplayMode_PartialTrajectory_FallsBackToRealExecution 验证"崩溃点
// 早于录像覆盖范围"场景：仅回放 S_PERCEIVE 一条（对应"崩溃发生在 S_PLAN 的
// LLM 调用完成之前"），S_PLAN/S_VALIDATE/S_EXECUTE/S_REFLECT 均需自动切回
// 真实调用/真实工具执行完成——因为这些步骤在原始崩溃会话里从未真正跑过，
// 不存在"重复副作用"风险。
func TestAgent_ReplayMode_PartialTrajectory_FallsBackToRealExecution(t *testing.T) {
	agent := NewAgentWithDefaults("test-replay-partial")
	provider := &mockProvider{} // 真实响应路径：S_PLAN 提示词命中默认 DAG 分支
	agent.InjectProvider(provider)
	agent.InjectPolicyGate(&allowPolicyGate{})
	mockExec := &mockToolExecutor{}
	agent.InjectToolExecutor(mockExec)
	agent.sCtx.DAGModel = &fsm.DAGModel{
		Nodes: []dag.ExecNode{{ID: "n1", ToolName: "read_file"}},
	}

	agent.InjectReplayData([]protocol.ReplayLLMCall{
		{Response: map[string]any{"content": "mock_success"}}, // 仅 S_PERCEIVE
	})
	protocol.SetReplayMode(true)
	defer protocol.SetReplayMode(false)

	done := make(chan struct{})
	go func() {
		_ = agent.Run(context.Background())
		close(done)
	}()
	agent.SendIntent(types.TriggerIntentReceived)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("agent run timeout")
	}

	if agent.StateMachine().Current() != types.AgentStateComplete {
		t.Errorf("expected AgentStateComplete, got %v", agent.StateMachine().Current())
	}
	if !mockExec.ExecuteWithTaintCalled {
		t.Error("expected real S_EXECUTE tool call after replay queue exhausted (never ran before crash)")
	}
	if protocol.IsReplaying() {
		t.Error("expected global ReplayMode to be flipped false once the single replay call was exhausted")
	}
}

// ============================================================================
// 崩溃检测 in-flight 标记（markInFlight/clearInFlight）
// ============================================================================

// fakeStore 是 protocol.Store 的最小内存实现，仅用于验证 markInFlight/
// clearInFlight 的 Put/Delete 行为，其余方法非本测试路径不会被调用。
type fakeStore struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newFakeStore() *fakeStore { return &fakeStore{data: make(map[string][]byte)} }

func (s *fakeStore) Get(_ context.Context, key []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[string(key)]
	if !ok {
		return nil, nil
	}
	return v, nil
}
func (s *fakeStore) Put(_ context.Context, key, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[string(key)] = value
	return nil
}
func (s *fakeStore) Delete(_ context.Context, key []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, string(key))
	return nil
}
func (s *fakeStore) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.data[key]
	return ok
}
func (s *fakeStore) Scan(_ context.Context, _ []byte) (protocol.Iterator, error) { return nil, nil }
func (s *fakeStore) BatchWrite(_ context.Context, _ []types.Op) error            { return nil }
func (s *fakeStore) Txn(_ context.Context, _ func(tx protocol.Transaction) error) error {
	return nil
}
func (s *fakeStore) Capabilities() types.StoreCapabilities { return types.StoreCapabilities{} }
func (s *fakeStore) Close() error                          { return nil }

// TestAgent_InFlightMarker_SetOnRunClearOnExit 验证 Run() 在处理循环期间
// 写入 in-flight 崩溃检测标记，正常终态退出时清除——崩溃恢复驱动器据此区分
// "干净退出的会话"与"进程崩溃时仍在处理中的会话"。
func TestAgent_InFlightMarker_SetOnRunClearOnExit(t *testing.T) {
	agent := NewAgentWithDefaults("test-inflight-marker")
	agent.InjectProvider(&mockProvider{})
	agent.InjectPolicyGate(&allowPolicyGate{})
	agent.InjectToolExecutor(&mockToolExecutor{})
	agent.sCtx.DAGModel = &fsm.DAGModel{
		Nodes: []dag.ExecNode{{ID: "n1", ToolName: "read_file"}},
	}

	store := newFakeStore()
	agent.InjectEventStore(store)

	done := make(chan struct{})
	go func() {
		_ = agent.Run(context.Background())
		close(done)
	}()
	agent.SendIntent(types.TriggerIntentReceived)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("agent run timeout")
	}

	// Run() 已返回（终态 Complete），defer clearInFlight 必然已执行完毕。
	if store.has("inflight:session:test-inflight-marker") {
		t.Error("expected in-flight marker to be cleared after Run() reached terminal state")
	}
}
