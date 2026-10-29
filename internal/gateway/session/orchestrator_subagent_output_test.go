package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/agent"
	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/execute/dag"
	bbexec "github.com/polarisagi/polaris/internal/execute/orchestrator"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/internal/tool/builtin/agent_result"
	"github.com/polarisagi/polaris/internal/tool/builtin/spawn_agents"
	"github.com/polarisagi/polaris/pkg/types"
)

// scriptedProvider 按调用顺序返回预置响应；每个 Agent 实例各自持有一份。
type scriptedProvider struct {
	mu        sync.Mutex
	responses []*types.ProviderResponse
	idx       int
}

func (p *scriptedProvider) Infer(context.Context, []types.Message, ...types.InferOption) (*types.ProviderResponse, error) {
	return nil, fmt.Errorf("Infer not used in streaming loop")
}

func (p *scriptedProvider) StreamInfer(context.Context, []types.Message, ...types.InferOption) (<-chan types.StreamEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.idx >= len(p.responses) {
		return nil, fmt.Errorf("scriptedProvider: 响应已耗尽 idx=%d", p.idx)
	}
	resp := p.responses[p.idx]
	p.idx++
	ch := make(chan types.StreamEvent, len(resp.ToolCalls)+2)
	if resp.Content != "" {
		ch <- types.StreamEvent{Type: types.StreamTextDelta, Content: resp.Content}
	}
	for _, tc := range resp.ToolCalls {
		payload, _ := json.Marshal(map[string]any{"id": tc.ID, "name": tc.Name, "input": json.RawMessage(tc.Input)})
		ch <- types.StreamEvent{Type: types.StreamToolCall, Content: string(payload)}
	}
	close(ch)
	return ch, nil
}

func (p *scriptedProvider) Capabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{SupportsStreaming: true, SupportsTools: true}
}
func (p *scriptedProvider) Tokenizer() protocol.TokenizerAdapter { return nil }
func (p *scriptedProvider) ModelID() string                      { return "scripted" }

// spawnOnlyExecutor 只提供 spawn_agents / agent_result 两个生产工具实现。
type spawnOnlyExecutor struct {
	spawnFn  func(ctx context.Context, input []byte) ([]byte, error)
	resultFn func(ctx context.Context, input []byte) ([]byte, error)
}

func (e *spawnOnlyExecutor) Lookup(name string) (types.Tool, error) {
	return types.Tool{Name: name, Source: types.ToolBuiltin, Capability: types.CapReadOnly}, nil
}

func (e *spawnOnlyExecutor) ExecuteWithTaint(ctx context.Context, name string, input []byte, _ types.TaintLevel) (*types.ToolResult, error) {
	fn := e.resultFn
	if name == "spawn_agents" {
		fn = e.spawnFn
	}
	out, err := fn(ctx, input)
	if err != nil {
		return nil, err
	}
	return &types.ToolResult{Success: true, Output: out}, nil
}

type allowAllGate struct{}

func (allowAllGate) IsAuthorized(context.Context, string, string, string, map[string]any) (bool, error) {
	return true, nil
}
func (allowAllGate) Review(context.Context, types.PolicyReviewRequest) (types.PolicyReviewResult, error) {
	return types.PolicyReviewResult{Allowed: true}, nil
}

// useAgenticLoopMode 显式钉死 agentic 模式：同包其他用例会用空 Config 覆盖全局
// 阈值（LoopMode 被清空），只靠环境变量会随用例顺序漂移回 staged。
func useAgenticLoopMode(t *testing.T) {
	t.Helper()
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	prev := config.Get()
	cfg := config.Config{}
	cfg.Thresholds = config.DefaultThresholds()
	cfg.Thresholds.M4Kernel.LoopMode = "agentic"
	config.Update(&cfg)
	t.Cleanup(func() { config.Update(prev) })
}

func newBlackboardDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	for _, f := range []string{"001_events.sql", "007_tasks.sql", "035_task_checkpoints.sql", "042_subagent_runs.sql", "051_agent_loop_rounds.sql"} {
		ddl, err := schema.FS.ReadFile(f)
		if err != nil {
			t.Fatalf("read schema %s: %v", f, err)
		}
		if _, err := db.Exec(string(ddl)); err != nil {
			t.Fatalf("apply schema %s: %v", f, err)
		}
	}
	return db
}

// TestR5_01_SubagentOutputNotInParentReply 生产装配（真实 agent.Pool + Blackboard +
// DefaultTaskWorker + session.orchestrator）：父智能体 spawn_agents(wait=all)，
// 子智能体回复 CHILD-OUT，父最终回复 PARENT-OUT。子输出不得进入父回复正文与持久化，
// 只能以 subagent_output 状态事件到达客户端。
func TestR5_01_SubagentOutputNotInParentReply(t *testing.T) {
	useAgenticLoopMode(t)

	db := newBlackboardDB(t)
	bb := bbexec.NewSQLiteBlackboard(db)

	pool := agent.NewPool(func(sessionID string) *agent.Agent {
		ag := agent.NewAgent(sessionID, nil, nil)
		ag.InjectDAGRunner(dag.NewRunner())
		ag.InjectDAGValidator(dag.NewValidator())
		ag.InjectPolicyGate(allowAllGate{})
		if strings.HasPrefix(sessionID, "sub-") {
			ag.InjectProvider(&scriptedProvider{responses: []*types.ProviderResponse{{Content: "CHILD-OUT"}, {Content: "CHILD-OUT"}}})
			// 子智能体为 explicit 完成模式：连续两轮纯文本走 implicit_finish 收尾。
			return ag
		}
		ag.InjectHandoffPoster(bb)
		ag.InjectToolExecutor(&spawnOnlyExecutor{
			spawnFn:  spawn_agents.MakeSpawnAgentsFn(),
			resultFn: agent_result.MakeAgentResultFn(),
		})
		ag.InjectProvider(&scriptedProvider{responses: []*types.ProviderResponse{
			{ToolCalls: []types.InferToolCall{{
				ID:    "call-spawn-1",
				Name:  "spawn_agents",
				Input: []byte(`{"tasks":[{"role":"worker","prompt":"do it"}],"wait":"all"}`),
			}}},
			{Content: "PARENT-OUT"},
		}})
		return ag
	}, 10)

	workerCtx, workerCancel := context.WithCancel(context.Background())
	defer func() {
		workerCancel()
		time.Sleep(20 * time.Millisecond)
	}()
	go func() { _ = bbexec.NewDefaultTaskWorker(bb, pool).RunLoop(workerCtx) }()

	persistence := newFakePersistence()
	orc := New(Deps{
		Hooks:         &fakeHooks{},
		SlashRouter:   &fakeSlash{},
		Compression:   &fakeCompression{},
		Persistence:   persistence,
		Prompt:        fakePrompt{},
		Registry:      fakeRegistry{},
		AgentPool:     pool,
		TranscriptDir: t.TempDir(),
		DataDir:       t.TempDir(),
	})
	sink := &recordingSink{}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := orc.RunTurn(ctx, Request{SessionID: "s-r5-01", Input: "go", Channel: "web"}, sink)
	if err != nil {
		t.Fatalf("RunTurn error: %v", err)
	}
	if res.Reply != "PARENT-OUT" {
		t.Errorf("Reply = %q, want %q", res.Reply, "PARENT-OUT")
	}
	for _, r := range persistence.savedAssistantReplies() {
		if strings.Contains(r, "CHILD-OUT") {
			t.Errorf("持久化的助手消息混入了子智能体输出: %q", r)
		}
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	foundStatus := false
	for _, e := range sink.events {
		if e.Kind == KindDelta && strings.Contains(e.Text, "CHILD-OUT") {
			t.Errorf("子智能体输出被当作 KindDelta 推送: %q", e.Text)
		}
		if e.Kind == KindStatus && e.Payload["type"] == "subagent_output" {
			if msg, _ := e.Payload["message"].(string); strings.Contains(msg, "CHILD-OUT") {
				foundStatus = true
			}
		}
	}
	if !foundStatus {
		t.Error("缺少含 CHILD-OUT 的 subagent_output 状态事件")
	}
}

// echoToolExecutor 对任意工具返回固定输出。
type echoToolExecutor struct{}

func (echoToolExecutor) Lookup(name string) (types.Tool, error) {
	return types.Tool{Name: name, Source: types.ToolBuiltin, Capability: types.CapReadOnly}, nil
}
func (echoToolExecutor) ExecuteWithTaint(context.Context, string, []byte, types.TaintLevel) (*types.ToolResult, error) {
	return &types.ToolResult{Success: true, Output: []byte("file content: mock")}, nil
}

// TestR5_02_GatewayDeltasEqualReply 生产装配：交互回合 3 轮（2 轮工具 + 1 轮文本），
// 网关收到的 Delta 按序拼接等于 Result.Reply 与持久化的助手消息，回复不重复；
// 并收到 2 条 tool_call 与 2 条 tool_result 状态事件。
func TestR5_02_GatewayDeltasEqualReply(t *testing.T) {
	useAgenticLoopMode(t)

	pool := agent.NewPool(func(sessionID string) *agent.Agent {
		ag := agent.NewAgent(sessionID, nil, nil)
		ag.InjectDAGRunner(dag.NewRunner())
		ag.InjectDAGValidator(dag.NewValidator())
		ag.InjectPolicyGate(allowAllGate{})
		ag.InjectToolExecutor(echoToolExecutor{})
		call := func(id string) []types.InferToolCall {
			return []types.InferToolCall{{ID: id, Name: "read_file", Input: []byte(`{"path":"a"}`)}}
		}
		ag.InjectProvider(&scriptedProvider{responses: []*types.ProviderResponse{
			{Content: "先读。", ToolCalls: call("c1")},
			{Content: "再读。", ToolCalls: call("c2")},
			{Content: "完成。"},
		}})
		return ag
	}, 10)

	persistence := newFakePersistence()
	orc := New(Deps{
		Hooks: &fakeHooks{}, SlashRouter: &fakeSlash{}, Compression: &fakeCompression{},
		Persistence: persistence, Prompt: fakePrompt{}, Registry: fakeRegistry{}, AgentPool: pool,
		TranscriptDir: t.TempDir(), DataDir: t.TempDir(),
	})
	sink := &recordingSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := orc.RunTurn(ctx, Request{SessionID: "s-r5-02", Input: "go", Channel: "web"}, sink)
	if err != nil {
		t.Fatalf("RunTurn error: %v", err)
	}

	const want = "先读。再读。完成。"
	if res.Reply != want {
		t.Errorf("Reply = %q, want %q", res.Reply, want)
	}
	if replies := persistence.savedAssistantReplies(); len(replies) != 1 || replies[0] != want {
		t.Errorf("持久化助手消息 = %q, want [%q]", replies, want)
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	var deltas strings.Builder
	calls, results := 0, 0
	for _, e := range sink.events {
		switch {
		case e.Kind == KindDelta:
			deltas.WriteString(e.Text)
		case e.Kind == KindStatus && e.Payload["type"] == "tool_call":
			calls++
		case e.Kind == KindStatus && e.Payload["type"] == "tool_result":
			results++
		}
	}
	if deltas.String() != want {
		t.Errorf("Delta 拼接 = %q, want %q", deltas.String(), want)
	}
	if calls != 2 || results != 2 {
		t.Errorf("tool_call=%d tool_result=%d, want 2/2", calls, results)
	}
}
