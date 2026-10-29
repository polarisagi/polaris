package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/tool/builtin/agent_result"
	"github.com/polarisagi/polaris/internal/tool/builtin/spawn_agents"
	"github.com/polarisagi/polaris/pkg/types"
)

type awaitTestToolExecutor struct {
	mu            sync.Mutex
	executedCalls []string
	spawnFn       func(ctx context.Context, input []byte) ([]byte, error)
	resultFn      func(ctx context.Context, input []byte) ([]byte, error)
}

func newAwaitTestToolExecutor() *awaitTestToolExecutor {
	return &awaitTestToolExecutor{
		spawnFn:  spawn_agents.MakeSpawnAgentsFn(),
		resultFn: agent_result.MakeAgentResultFn(),
	}
}

func (e *awaitTestToolExecutor) Lookup(name string) (types.Tool, error) {
	return types.Tool{
		Name:       name,
		Source:     types.ToolBuiltin,
		Capability: types.CapReadOnly,
	}, nil
}

func (e *awaitTestToolExecutor) ExecuteWithTaint(ctx context.Context, name string, input []byte, _ types.TaintLevel) (*types.ToolResult, error) {
	e.mu.Lock()
	e.executedCalls = append(e.executedCalls, name)
	e.mu.Unlock()

	switch name {
	case "spawn_agents":
		out, err := e.spawnFn(ctx, input)
		if err != nil {
			return nil, err
		}
		return &types.ToolResult{Success: true, Output: out}, nil
	case "agent_result":
		out, err := e.resultFn(ctx, input)
		if err != nil {
			return nil, err
		}
		return &types.ToolResult{Success: true, Output: out}, nil
	case "read_file":
		return &types.ToolResult{Success: true, Output: []byte("file content: mock data")}, nil
	default:
		return &types.ToolResult{Success: true, Output: []byte("ok")}, nil
	}
}

type dynamicLoopProvider struct {
	mu           sync.Mutex
	getResponses func(idx int, msgs []types.Message) *types.ProviderResponse
	idx          int
	history      [][]types.Message
}

func (p *dynamicLoopProvider) Infer(_ context.Context, _ []types.Message, _ ...types.InferOption) (*types.ProviderResponse, error) {
	return nil, fmt.Errorf("Infer not used in streaming loop")
}

func (p *dynamicLoopProvider) StreamInfer(_ context.Context, msgs []types.Message, _ ...types.InferOption) (<-chan types.StreamEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.history = append(p.history, msgs)
	resp := p.getResponses(p.idx, msgs)
	p.idx++
	if resp == nil {
		return nil, fmt.Errorf("dynamicLoopProvider: nil response for idx=%d", p.idx-1)
	}

	ch := make(chan types.StreamEvent, len(resp.ToolCalls)+2)
	if resp.Content != "" {
		ch <- types.StreamEvent{Type: types.StreamTextDelta, Content: resp.Content}
	}
	for _, tc := range resp.ToolCalls {
		rawInput := tc.Input
		if len(rawInput) == 0 {
			rawInput = []byte("{}")
		}
		payload := fmt.Sprintf(`{"id":%q,"name":%q,"input":%s}`, tc.ID, tc.Name, string(rawInput))
		ch <- types.StreamEvent{Type: types.StreamToolCall, Content: payload}
	}
	close(ch)
	return ch, nil
}

func (p *dynamicLoopProvider) Capabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{SupportsStreaming: true, SupportsTools: true}
}
func (p *dynamicLoopProvider) Tokenizer() protocol.TokenizerAdapter { return nil }
func (p *dynamicLoopProvider) ModelID() string                      { return "dynamic-loop-provider" }

// TestR2_03_SpawnAgents_WaitAll_SuspendAndResume 验证 wait=all 场景下的挂起与唤醒恢复，结果回填至 tool_result。
func TestR2_03_SpawnAgents_WaitAll_SuspendAndResume(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	agent := NewAgentWithDefaults("test-r2-03-wait-all")
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot)}
	agent.InjectHandoffPoster(poster)
	agent.InjectPolicyGate(&allowPolicyGate{})
	exec := newAwaitTestToolExecutor()
	agent.InjectToolExecutor(exec)

	prov := &loopTestProvider{
		responses: []*types.ProviderResponse{
			{
				Content: "派生研究子智能体...",
				ToolCalls: []types.InferToolCall{
					{
						ID:   "call-spawn-1",
						Name: "spawn_agents",
						Input: []byte(`{
							"tasks": [{"role":"researcher","prompt":"investigate memory leak"}],
							"wait": "all"
						}`),
					},
				},
			},
			{
				Content: "子智能体已完成，报告显示内存泄漏已定位。",
			},
		},
	}
	agent.InjectProvider(prov)

	done := make(chan struct{})
	go func() {
		_ = agent.Run(context.Background())
		close(done)
	}()

	if err := agent.SendIntent(types.TriggerIntentReceived); err != nil {
		t.Fatalf("SendIntent failed: %v", err)
	}

	// 等待子任务投递
	var childID string
	for i := 0; i < 50; i++ {
		poster.mu.Lock()
		for id := range poster.tasks {
			childID = id
			break
		}
		poster.mu.Unlock()
		if childID != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if childID == "" {
		t.Fatal("subagent task was not posted to blackboard")
	}

	// 模拟子任务执行完成
	poster.mu.Lock()
	poster.tasks[childID].Status = types.TaskDone
	poster.tasks[childID].Result = []byte("investigation report: leak in buffer pool")
	if poster.subscribeCh != nil {
		select {
		case poster.subscribeCh <- types.BlackboardEvent{Type: "task_completed", TaskID: childID}:
		default:
		}
	}
	poster.mu.Unlock()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("agent run timeout waiting for resume")
	}

	if st := agent.sm.Current(); st != types.AgentStateComplete {
		t.Fatalf("agent should complete after resume, got state: %v", st)
	}

	agent.sCtx.Mu.Lock()
	defer agent.sCtx.Mu.Unlock()

	var spawnToolResult *types.Message
	for i := range agent.sCtx.LoopMessages {
		msg := &agent.sCtx.LoopMessages[i]
		for _, part := range msg.Parts {
			if pMap, ok := part.(map[string]any); ok {
				if pMap["type"] == "tool_result" && pMap["tool_use_id"] == "call-spawn-1" {
					spawnToolResult = msg
					break
				}
			}
		}
	}

	if spawnToolResult == nil {
		t.Fatal("expected tool_result message for call-spawn-1 in LoopMessages")
	}
	if !strings.Contains(spawnToolResult.Content, "leak in buffer pool") {
		t.Fatalf("expected tool_result content to contain subagent result, got: %q", spawnToolResult.Content)
	}
	if len(agent.sCtx.HandoffTaskIDs) != 0 {
		t.Fatalf("HandoffTaskIDs should be cleared after resume, got: %v", agent.sCtx.HandoffTaskIDs)
	}
}

// TestR2_03_MixedBatch_ReadFileAndSpawn 验证混合批（1 读文件 + 1 spawn_agents）的顺序保证与结果回填。
func TestR2_03_MixedBatch_ReadFileAndSpawn(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	agent := NewAgentWithDefaults("test-r2-03-mixed")
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot)}
	agent.InjectHandoffPoster(poster)
	agent.InjectPolicyGate(&allowPolicyGate{})
	exec := newAwaitTestToolExecutor()
	agent.InjectToolExecutor(exec)

	prov := &loopTestProvider{
		responses: []*types.ProviderResponse{
			{
				Content: "先读配置再派生子任务...",
				ToolCalls: []types.InferToolCall{
					{
						ID:    "call-read-1",
						Name:  "read_file",
						Input: []byte(`{"path":"config.json"}`),
					},
					{
						ID:   "call-spawn-2",
						Name: "spawn_agents",
						Input: []byte(`{
							"tasks": [{"role":"worker","prompt":"work on task"}],
							"wait": "all"
						}`),
					},
				},
			},
			{
				Content: "全部完成。",
			},
		},
	}
	agent.InjectProvider(prov)

	done := make(chan struct{})
	go func() {
		_ = agent.Run(context.Background())
		close(done)
	}()

	if err := agent.SendIntent(types.TriggerIntentReceived); err != nil {
		t.Fatalf("SendIntent failed: %v", err)
	}

	var childID string
	for i := 0; i < 50; i++ {
		poster.mu.Lock()
		for id := range poster.tasks {
			childID = id
			break
		}
		poster.mu.Unlock()
		if childID != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if childID == "" {
		t.Fatal("subagent task was not posted")
	}

	poster.mu.Lock()
	poster.tasks[childID].Status = types.TaskDone
	poster.tasks[childID].Result = []byte("subagent worker finished")
	if poster.subscribeCh != nil {
		select {
		case poster.subscribeCh <- types.BlackboardEvent{Type: "task_completed", TaskID: childID}:
		default:
		}
	}
	poster.mu.Unlock()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("agent run timeout in mixed batch test")
	}

	if st := agent.sm.Current(); st != types.AgentStateComplete {
		t.Fatalf("mixed batch agent should complete, got: %v", st)
	}

	agent.sCtx.Mu.Lock()
	defer agent.sCtx.Mu.Unlock()

	var readIdx, spawnIdx = -1, -1
	for idx, msg := range agent.sCtx.LoopMessages {
		for _, part := range msg.Parts {
			if pMap, ok := part.(map[string]any); ok {
				if pMap["type"] == "tool_result" {
					if pMap["tool_use_id"] == "call-read-1" {
						readIdx = idx
					}
					if pMap["tool_use_id"] == "call-spawn-2" {
						spawnIdx = idx
					}
				}
			}
		}
	}

	if readIdx == -1 || spawnIdx == -1 {
		t.Fatalf("both tool results must be present: readIdx=%d, spawnIdx=%d", readIdx, spawnIdx)
	}
	if readIdx >= spawnIdx {
		t.Fatalf("read_file result (idx %d) must precede spawn_agents result (idx %d)", readIdx, spawnIdx)
	}
}

// TestR2_03_SpawnAgents_WaitNone_AgentResult 验证 wait=none 模式不挂起，后续用 agent_result 获取结果。
func TestR2_03_SpawnAgents_WaitNone_AgentResult(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	agent := NewAgentWithDefaults("test-r2-03-wait-none")
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot)}
	agent.InjectHandoffPoster(poster)
	agent.InjectPolicyGate(&allowPolicyGate{})
	exec := newAwaitTestToolExecutor()
	agent.InjectToolExecutor(exec)

	prov := &dynamicLoopProvider{
		getResponses: func(idx int, msgs []types.Message) *types.ProviderResponse {
			if idx == 0 {
				return &types.ProviderResponse{
					Content: "启动异步后台子任务...",
					ToolCalls: []types.InferToolCall{
						{
							ID:   "call-async-spawn",
							Name: "spawn_agents",
							Input: []byte(`{
								"tasks": [{"role":"indexer","prompt":"build index"}],
								"wait": "none"
							}`),
						},
					},
				}
			}
			if idx == 1 {
				targetID := ""
				poster.mu.Lock()
				for id := range poster.tasks {
					targetID = id
					poster.tasks[id].Status = types.TaskDone
					poster.tasks[id].Result = []byte("index built successfully: 1200 docs")
					break
				}
				poster.mu.Unlock()
				return &types.ProviderResponse{
					Content: "查询子任务产物...",
					ToolCalls: []types.InferToolCall{
						{
							ID:    "call-result-1",
							Name:  "agent_result",
							Input: []byte(fmt.Sprintf(`{"id":%q}`, targetID)),
						},
					},
				}
			}
			return &types.ProviderResponse{
				Content: "已取到异步子任务结果，流程结束。",
			}
		},
	}
	agent.InjectProvider(prov)

	done := make(chan struct{})
	go func() {
		_ = agent.Run(context.Background())
		close(done)
	}()

	if err := agent.SendIntent(types.TriggerIntentReceived); err != nil {
		t.Fatalf("SendIntent failed: %v", err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("agent run timeout in wait=none test")
	}

	if st := agent.sm.Current(); st != types.AgentStateComplete {
		t.Fatalf("agent should complete, got: %v", st)
	}

	agent.sCtx.Mu.Lock()
	defer agent.sCtx.Mu.Unlock()
	foundResult := false
	for _, msg := range agent.sCtx.LoopMessages {
		if strings.Contains(msg.Content, "1200 docs") {
			foundResult = true
			break
		}
	}
	if !foundResult {
		t.Fatal("expected agent_result output to contain '1200 docs'")
	}
}

// TestR2_03_TransferToAgent_Agentic_EndToEnd 验证 transfer_to_agent 在 agentic 模式下统一转调 SpawnAgents(wait=all) 并挂起恢复。
func TestR2_03_TransferToAgent_Agentic_EndToEnd(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	agent := NewAgentWithDefaults("test-r2-03-transfer")
	poster := &fakeHandoffPoster{tasks: make(map[string]*types.TaskSnapshot)}
	agent.InjectHandoffPoster(poster)
	agent.InjectPolicyGate(&allowPolicyGate{})
	exec := newAwaitTestToolExecutor()
	agent.InjectToolExecutor(exec)

	prov := &loopTestProvider{
		responses: []*types.ProviderResponse{
			{
				Content: "委派给 librarian 专家...",
				ToolCalls: []types.InferToolCall{
					{
						ID:   "call-transfer-1",
						Name: "transfer_to_agent",
						Input: []byte(`{
							"target_agent_role": "librarian",
							"context_summary": "summarize project architecture"
						}`),
					},
				},
			},
			{
				Content: "图书管理员回复已收到，完成任务。",
			},
		},
	}
	agent.InjectProvider(prov)

	done := make(chan struct{})
	go func() {
		_ = agent.Run(context.Background())
		close(done)
	}()

	if err := agent.SendIntent(types.TriggerIntentReceived); err != nil {
		t.Fatalf("SendIntent failed: %v", err)
	}

	var childID string
	for i := 0; i < 50; i++ {
		poster.mu.Lock()
		for id, snap := range poster.tasks {
			if strings.Contains(snap.Type, "librarian") {
				childID = id
				break
			}
		}
		poster.mu.Unlock()
		if childID != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if childID == "" {
		t.Fatal("transfer_to_agent did not post task to blackboard")
	}

	poster.mu.Lock()
	poster.tasks[childID].Status = types.TaskDone
	poster.tasks[childID].Result = []byte("architecture summary: 13 modules, clean layered")
	if poster.subscribeCh != nil {
		select {
		case poster.subscribeCh <- types.BlackboardEvent{Type: "task_completed", TaskID: childID}:
		default:
		}
	}
	poster.mu.Unlock()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("agent run timeout in transfer_to_agent test")
	}

	if st := agent.sm.Current(); st != types.AgentStateComplete {
		t.Fatalf("expected AgentStateComplete, got: %v", st)
	}

	agent.sCtx.Mu.Lock()
	defer agent.sCtx.Mu.Unlock()
	found := false
	for _, msg := range agent.sCtx.LoopMessages {
		if strings.Contains(msg.Content, "clean layered") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected transfer_to_agent result to contain 'clean layered'")
	}
}
