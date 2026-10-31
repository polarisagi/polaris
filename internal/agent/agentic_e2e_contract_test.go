package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/polarisagi/polaris/internal/action"
	"github.com/polarisagi/polaris/internal/action/codeact"
	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/execute/orchestrator"
	"github.com/polarisagi/polaris/internal/memory"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/internal/tool"
	"github.com/polarisagi/polaris/internal/tool/builtin"
	"github.com/polarisagi/polaris/internal/tool/builtin/web_fetch"
	"github.com/polarisagi/polaris/internal/tool/catalog"
	"github.com/polarisagi/polaris/internal/tool/dispatch"
	toolsb "github.com/polarisagi/polaris/internal/tool/sandbox"
	"github.com/polarisagi/polaris/pkg/types"
)

var (
	e2eMetricOnce   sync.Once
	e2eMetricReader *sdkmetric.ManualReader
)

func getE2EAutoTerminations() int64 {
	e2eMetricOnce.Do(func() {
		e2eMetricReader = sdkmetric.NewManualReader()
		mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(e2eMetricReader))
		_ = metrics.InitMetrics(mp.Meter("polaris.e2e"))
	})
	var rm metricdata.ResourceMetrics
	if err := e2eMetricReader.Collect(context.Background(), &rm); err != nil {
		return 0
	}
	var count int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "polaris.agent.loop_termination_total" {
				if sum, ok := m.Data.(metricdata.Sum[int64]); ok {
					for _, dp := range sum.DataPoints {
						for _, kv := range dp.Attributes.ToSlice() {
							if string(kv.Key) == "reason" && strings.Contains(fmt.Sprint(kv), "auto") {
								count += dp.Value
							}
						}
					}
				}
			}
		}
	}
	return count
}

type e2eContractCognitive struct{ docID string }

func (c *e2eContractCognitive) FTSSearch(context.Context, string, int) ([]fsm.CogResult, error) {
	return nil, nil
}
func (c *e2eContractCognitive) FTSEpisodic(context.Context, string, int) ([]fsm.CogResult, error) {
	if c.docID == "" {
		return nil, nil
	}
	return []fsm.CogResult{{DocID: c.docID, Score: 9.0}}, nil
}

type e2eContractProvider struct {
	mu        sync.Mutex
	requests  [][]types.Message
	responses []*types.ProviderResponse
	onInfer   func(idx int, req []types.Message)
}

func (p *e2eContractProvider) Infer(_ context.Context, msgs []types.Message, _ ...types.InferOption) (*types.ProviderResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	copied := make([]types.Message, len(msgs))
	copy(copied, msgs)
	p.requests = append(p.requests, copied)

	idx := len(p.requests) - 1
	if p.onInfer != nil {
		p.onInfer(idx, copied)
	}
	if idx >= len(p.responses) {
		return nil, fmt.Errorf("e2eContractProvider: no response configured for idx=%d", idx)
	}
	return p.responses[idx], nil
}

func (p *e2eContractProvider) StreamInfer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	// custom-nolint:bare-infer
	resp, err := p.Infer(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	ch := make(chan types.StreamEvent, len(resp.ToolCalls)+3)
	if resp.Content != "" {
		ch <- types.StreamEvent{Type: types.StreamTextDelta, Content: resp.Content}
	}
	for _, tc := range resp.ToolCalls {
		raw := tc.Input
		if len(raw) == 0 {
			raw = []byte("{}")
		}
		pl, _ := json.Marshal(map[string]any{"id": tc.ID, "name": tc.Name, "input": json.RawMessage(raw)})
		ch <- types.StreamEvent{Type: types.StreamToolCall, Content: string(pl)}
	}
	close(ch)
	return ch, nil
}

func (p *e2eContractProvider) Capabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{SupportsStreaming: true, SupportsTools: true}
}
func (p *e2eContractProvider) Tokenizer() protocol.TokenizerAdapter { return nil }
func (p *e2eContractProvider) ModelID() string                      { return "e2e-contract-mock" }

type e2eSubagentProvider struct{}

func (p *e2eSubagentProvider) Infer(context.Context, []types.Message, ...types.InferOption) (*types.ProviderResponse, error) {
	return &types.ProviderResponse{Content: "subagent task executed"}, nil
}
func (p *e2eSubagentProvider) StreamInfer(context.Context, []types.Message, ...types.InferOption) (<-chan types.StreamEvent, error) {
	ch := make(chan types.StreamEvent, 1)
	ch <- types.StreamEvent{Type: types.StreamTextDelta, Content: "subagent task executed"}
	close(ch)
	return ch, nil
}
func (p *e2eSubagentProvider) Capabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{SupportsStreaming: true}
}
func (p *e2eSubagentProvider) Tokenizer() protocol.TokenizerAdapter { return nil }
func (p *e2eSubagentProvider) ModelID() string                      { return "e2e-subagent-mock" }

func assertPrefixMonotonic(t *testing.T, a, b []types.Message, name string) {
	t.Helper()
	if len(a) > len(b) {
		t.Fatalf("%s: prefix violation: len(a)=%d > len(b)=%d", name, len(a), len(b))
	}
	for i := range a {
		if a[i].Role != b[i].Role || a[i].Content != b[i].Content {
			t.Fatalf("%s: mismatch at %d: %s vs %s", name, i, a[i].Role, b[i].Role)
		}
	}
}

// TestAgentic_E2EContract_FullAutonomyLifecycle 端到端契约测试（R4-07）。
func TestAgentic_E2EContract_FullAutonomyLifecycle(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required for e2e contract test")
	}
	t.Setenv("POLARIS_LOOP_MODE", "agentic")

	tmpDir := t.TempDir()
	sampleGoFile := filepath.Join(tmpDir, "kernel.go")
	_ = os.WriteFile(sampleGoFile, []byte("package main\n\nfunc PolarisRun() {}\n"), 0o600)

	beforeAuto := getE2EAutoTerminations()

	st, err := store.OpenSQLite(filepath.Join(tmpDir, "polaris.db"), schema.FS)
	if err != nil {
		t.Fatalf("OpenSQLite failed: %v", err)
	}
	db := st.DB()
	bb := orchestrator.NewSQLiteBlackboard(db)

	memImpl := memory.NewMemImplWithDB(st, db)
	memFacade := memory.NewMemoryFacade(memory.NewMemorySystemFromMemImpl(memImpl))
	err = memFacade.AppendEpisodicEvent(context.Background(), types.Event{
		ID:         "ev_e2e_arch",
		Type:       "task_done",
		Payload:    []byte(`{"summary":"已确认 Polaris 架构采用四层校验分层内核"}`),
		TaintLevel: types.TaintLow,
		CreatedAt:  time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	}, types.TaintLow)
	if err != nil {
		t.Fatalf("AppendEpisodicEvent failed: %v", err)
	}

	sbx := sandbox.NewInProcessSandbox(config.DefaultThresholds().M7Tool)
	router := sandbox.NewSandboxRouter(sbx, nil, nil, runtime.GOOS, 0)
	envelope := sandbox.NewExecEnvelope(&allowPolicyGate{}, router, 0, "", nil).WithExecutionAuditor(testExecutionAuditor{})
	toolReg := tool.NewInMemoryToolRegistry(envelope, config.DefaultThresholds().M7Tool)
	err = builtin.RegisterBuiltinTools(sbx, toolReg, []string{tmpDir}, nil, false, protocol.NetPolicyDeny, "", &config.Config{}, nil, "", nil, nil, nil, web_fetch.Deps{}, policy.StandardPolicy(), "", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("RegisterBuiltinTools failed: %v", err)
	}

	memCat := catalog.NewMemoryCatalog()
	for _, tl := range toolReg.List() {
		memCat.Register(protocol.CatalogEntry{
			Name:        tl.Name,
			Description: tl.Description,
			Parameters:  tl.InputSchema,
			Source:      tl.Source,
			TrustTier:   tl.TrustTier,
		})
	}
	disp := dispatch.New(memCat, toolReg, nil)

	container := sandbox.NewContainerSandbox("", runtime.GOOS, 1, toolsb.NewWrapBashCmdRunner(), config.DefaultThresholds().M7Tool)
	caRouter := sandbox.NewSandboxRouter(nil, container, nil, runtime.GOOS, 1)
	caEnvelope := sandbox.NewExecEnvelope(&allowPolicyGate{}, caRouter, 1, runtime.GOOS, nil).WithExecutionAuditor(testExecutionAuditor{})
	caEngine := codeact.NewCodeAct(caEnvelope, nil,
		codeact.WithGovernanceAgent(testGovAgent{}),
		codeact.WithASTChecker(&codeact.DefaultASTChecker{}),
		codeact.WithTokenManager(action.GetTokenManager()),
		codeact.WithToolExecutor(toolReg),
		codeact.WithPolicy(policy.ProfilePolicy(policy.ProfileStandard)),
	)

	workerPool := NewPool(func(sessionID string) *Agent {
		child := NewAgentWithDefaults(sessionID)
		child.InjectProvider(&e2eSubagentProvider{})
		child.InjectPolicyGate(&allowPolicyGate{})
		child.InjectToolExecutor(disp)
		return child
	}, 10)
	worker := orchestrator.NewDefaultTaskWorker(bb, workerPool)
	workerCtx, workerCancel := context.WithCancel(context.Background())
	defer workerCancel()
	go func() { _ = worker.RunLoop(workerCtx) }()

	sessionID := "sess-e2e-contract"
	agent := NewAgentWithDefaults(sessionID)
	agent.extQuerier = db
	agent.InjectMemory(memFacade)
	agent.SetCognitiveSearcher(&e2eContractCognitive{docID: "ev_e2e_arch"})
	agent.InjectToolExecutor(disp)
	agent.InjectHandoffPoster(bb)
	agent.SetCodeAct(&testCodeActAdapter{inner: caEngine})
	agent.InjectPolicyGate(&allowPolicyGate{})

	pyCode := fmt.Sprintf("import polaris\nres = polaris.call('grep', pattern='PolarisRun', path=%q)\nprint('MATCHED:' + str(res))\n", sampleGoFile)
	codeActInput, _ := json.Marshal(map[string]any{"language": "python", "code": pyCode})
	spawnInput := []byte(`{"tasks":[{"role":"tester-1","prompt":"verify net"},{"role":"tester-2","prompt":"verify store"}],"wait":"all"}`)

	responses := []*types.ProviderResponse{
		// Turn 1 Req 1: read_file
		{ToolCalls: []types.InferToolCall{{ID: "c_read", Name: "read_file", Input: []byte(fmt.Sprintf(`{"path":%q}`, sampleGoFile))}}},
		// Turn 1 Req 2: code_act calling grep
		{ToolCalls: []types.InferToolCall{{ID: "c_code_act", Name: "code_act", Input: codeActInput}}},
		// Turn 1 Req 3: finish turn 1
		{Content: "Turn 1 架构分析与代码检索完成，已确认 PolarisRun 实现。"},
		// Turn 2 Req 1: spawn_agents
		{ToolCalls: []types.InferToolCall{{ID: "c_spawn", Name: "spawn_agents", Input: spawnInput}}},
		// Turn 2 Req 2: finish turn 2
		{Content: "Turn 2 子任务与插话指令执行完毕，全部模块验证通过。"},
	}

	intent1, intent2, steerText := "分析架构并用Python检索PolarisRun代码", "启动2个子任务执行模块验证", "请优先验证存储引擎"

	prov := &e2eContractProvider{
		responses: responses,
		onInfer: func(idx int, _ []types.Message) {
			if idx == 3 {
				if !agent.Steer(steerText) {
					t.Error("Turn 2 Req 1 期间 Steer 必须入队成功")
				}
			}
		},
	}
	agent.InjectProvider(prov)

	// === 执行 Turn 1 ===
	agent.SetTaskIntent(taint.NewTaintedString(intent1, taint.TaintSource{OriginTaintLevel: types.TaintLow}, "e2e"))
	runAgenticTurnWait(t, agent)
	if agent.sm.Current() != types.AgentStateComplete {
		t.Fatalf("Turn 1 预期完成，实际状态: %v", agent.sm.Current())
	}

	// === 执行 Turn 2 ===
	agent.sm.ForceState(types.AgentStateIdle)
	agent.SetTaskIntent(taint.NewTaintedString(intent2, taint.TaintSource{OriginTaintLevel: types.TaintLow}, "e2e"))
	runAgenticTurnWait(t, agent)
	if agent.sm.Current() != types.AgentStateComplete {
		t.Fatalf("Turn 2 预期完成，实际状态: %v", agent.sm.Current())
	}

	// === 契约断言 1: 请求总次数与各轮问题包含断言 ===
	prov.mu.Lock()
	reqs := prov.requests
	prov.mu.Unlock()

	if len(reqs) != 5 {
		t.Fatalf("预期 5 次 LLM 请求（Turn 1 三次 + Turn 2 两次），实际: %d", len(reqs))
	}

	countIntent := func(req []types.Message, intent string) int {
		var c int
		for _, m := range req {
			if strings.Contains(m.Content, intent) {
				c++
			}
		}
		return c
	}
	for i := 0; i < 3; i++ {
		if c := countIntent(reqs[i], intent1); c != 1 {
			t.Fatalf("Turn 1 第 %d 次请求包含用户意图数量期望 1 次，实际: %d", i+1, c)
		}
	}
	if !strings.Contains(reqs[0][len(reqs[0])-1].Content, "## Recalled Context") {
		t.Fatal("Turn 1 首条用户消息必须包含 ## Recalled Context 召回内容")
	}

	for i := 3; i < 5; i++ {
		if c := countIntent(reqs[i], intent2); c != 1 {
			t.Fatalf("Turn 2 第 %d 次请求包含用户意图数量期望 1 次，实际: %d", i-2, c)
		}
	}

	// === 契约断言 2: 同一回合内各轮前缀单调（T1） ===
	assertPrefixMonotonic(t, reqs[0], reqs[1], "Turn 1 Req 1 -> Req 2")
	assertPrefixMonotonic(t, reqs[1], reqs[2], "Turn 1 Req 2 -> Req 3")
	assertPrefixMonotonic(t, reqs[3], reqs[4], "Turn 2 Req 1 -> Req 2")

	// === 契约断言 3: 第 2 回合请求不含第 1 回合的工具往返 ===
	for i := 3; i < 5; i++ {
		for _, m := range reqs[i] {
			if strings.Contains(m.Content, "c_read") || strings.Contains(m.Content, "c_code_act") {
				t.Fatalf("Turn 2 请求 %d 严禁包含 Turn 1 工具往返: %+v", i, m)
			}
		}
	}

	// === 契约断言 4: 所有 tool_use_id 与 tool_result 一一对应 ===
	toolUseIDs, toolResultIDs := map[string]int{}, map[string]int{}
	for _, req := range reqs {
		for _, m := range req {
			for _, p := range m.Parts {
				pm, ok := p.(map[string]any)
				if !ok {
					continue
				}
				switch pm["type"] {
				case "tool_use":
					if id, ok := pm["id"].(string); ok {
						toolUseIDs[id]++
					}
				case "tool_result":
					if id, ok := pm["tool_use_id"].(string); ok {
						toolResultIDs[id]++
					}
				}
			}
		}
	}
	for id := range toolUseIDs {
		if toolResultIDs[id] == 0 {
			t.Fatalf("tool_use_id %s 未找到对应的 tool_result", id)
		}
	}

	// === 契约断言 5: agent_loop_rounds 中两个 turn 都是 done ===
	rows, qErr := db.Query("SELECT DISTINCT turn_id, status FROM agent_loop_rounds WHERE session_id = ?", sessionID)
	if qErr != nil {
		t.Fatalf("query agent_loop_rounds failed: %v", qErr)
	}
	defer rows.Close()

	var doneTurns int
	for rows.Next() {
		var tID, st string
		if err := rows.Scan(&tID, &st); err != nil || st != "done" {
			t.Fatalf("turn %s 必须为 done, err: %v, st: %s", tID, err, st)
		}
		doneTurns++
	}
	if doneTurns != 2 {
		t.Fatalf("agent_loop_rounds 中必须记录恰好 2 个 done turn，实际: %d", doneTurns)
	}

	// === 契约断言 6: polaris.agent.loop_termination_total 计数正确 ===
	deltaAuto := getE2EAutoTerminations() - beforeAuto
	if deltaAuto != 2 {
		t.Fatalf("期望 auto termination 累计增加 2 次，实际差值: %d (before: %d, after: %d)", deltaAuto, beforeAuto, beforeAuto+deltaAuto)
	}
}
