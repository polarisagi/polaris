package agent

import (
	"context"
	"encoding/json"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/action"
	"github.com/polarisagi/polaris/internal/action/codeact"
	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/internal/tool/catalog"
	toolsb "github.com/polarisagi/polaris/internal/tool/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type codeActTestBridgeExecutor struct {
	tools map[string]types.Tool
}

func newCodeActTestBridgeExecutor() *codeActTestBridgeExecutor {
	return &codeActTestBridgeExecutor{
		tools: map[string]types.Tool{
			"read_file": {
				Name:        "read_file",
				Description: "Read file content",
				Capability:  types.CapReadOnly,
				SideEffects: []types.SideEffect{types.SideNone},
			},
			"write_file": {
				Name:        "write_file",
				Description: "Write content to file",
				Capability:  types.CapWriteLocal,
				SideEffects: []types.SideEffect{types.SideFileWrite},
			},
			"secret_scanner": {
				Name:        "secret_scanner",
				Description: "Scanner returning tainted secret",
				Capability:  types.CapReadOnly,
				SideEffects: []types.SideEffect{types.SideNone},
			},
		},
	}
}

func (m *codeActTestBridgeExecutor) Lookup(name string) (types.Tool, error) {
	t, ok := m.tools[name]
	if !ok {
		return types.Tool{}, apperr.New(apperr.CodeNotFound, "tool not found: "+name)
	}
	return t, nil
}

func (m *codeActTestBridgeExecutor) List() []types.Tool {
	out := make([]types.Tool, 0, len(m.tools))
	for _, t := range m.tools {
		out = append(out, t)
	}
	return out
}

func (m *codeActTestBridgeExecutor) ExecuteTool(_ context.Context, name string, input []byte, taintLevel types.TaintLevel) (*types.ToolResult, error) {
	switch name {
	case "read_file":
		var args map[string]any
		_ = json.Unmarshal(input, &args)
		path, _ := args["path"].(string)
		return &types.ToolResult{
			Success:    true,
			Output:     []byte(`{"content":"file content of ` + path + `"}`),
			TaintLevel: taintLevel,
		}, nil
	case "write_file":
		return &types.ToolResult{
			Success:    true,
			Output:     []byte(`{"bytes_written":42}`),
			TaintLevel: taintLevel,
		}, nil
	case "secret_scanner":
		return &types.ToolResult{
			Success:    true,
			Output:     []byte(`{"secret":"classified-token"}`),
			TaintLevel: types.TaintHigh,
		}, nil
	default:
		return &types.ToolResult{
			Success: false,
			Error:   "unknown tool: " + name,
		}, nil
	}
}

// testCodeActAdapter 生产装配适配器（与 cmd/polaris/adapters_agent.go 中的 codeActAdapter 保持一致）
type testCodeActAdapter struct {
	inner *codeact.CodeAct
}

func (a *testCodeActAdapter) Execute(ctx context.Context, req CodeActRequest) (*CodeActResult, error) {
	res, err := a.inner.Execute(ctx, protocol.CodeActRequest{
		Language:        req.Language,
		Code:            req.Code,
		CapabilityID:    req.CapabilityID,
		SessionID:       req.SessionID,
		AgentID:         req.AgentID,
		StatefulSession: req.StatefulSession,
	})
	if err != nil {
		return nil, err
	}
	return &CodeActResult{
		Output:    res.Output,
		ExitCode:  res.ExitCode,
		LatencyMs: res.LatencyMs,
	}, nil
}

func (a *testCodeActAdapter) IsAvailable() bool { return true }

type testGovAgent struct{}

func (testGovAgent) ValidateCode(_ string, _ []byte, _ map[string]bool) error {
	return nil
}

type testPolicyGate struct{}

func (testPolicyGate) IsAuthorized(ctx context.Context, principal, action, resource string, context map[string]any) (bool, error) {
	return true, nil
}

func (testPolicyGate) Review(ctx context.Context, req types.PolicyReviewRequest) (types.PolicyReviewResult, error) {
	return types.PolicyReviewResult{Allowed: true}, nil
}

func setupCodeActProductionEngine(t *testing.T, bridgeExec *codeActTestBridgeExecutor) *testCodeActAdapter {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found, skipping code_act production test")
	}

	container := sandbox.NewContainerSandbox("", runtime.GOOS, 1, toolsb.NewWrapBashCmdRunner(), config.DefaultThresholds().M7Tool)
	router := sandbox.NewSandboxRouter(nil, container, nil, runtime.GOOS, 1)
	envelope := sandbox.NewExecEnvelope(testPolicyGate{}, router, 1, runtime.GOOS, nil).WithExecutionAuditor(testExecutionAuditor{})

	caEngine := codeact.NewCodeAct(
		envelope,
		nil,
		codeact.WithGovernanceAgent(testGovAgent{}),
		codeact.WithASTChecker(&codeact.DefaultASTChecker{}),
		codeact.WithTokenManager(action.GetTokenManager()),
		codeact.WithToolExecutor(bridgeExec),
		codeact.WithPolicy(policy.ProfilePolicy(policy.ProfileStandard)),
	)

	return &testCodeActAdapter{inner: caEngine}
}

func runAgenticTurnWait(t *testing.T, agent *Agent) {
	t.Helper()
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
	case <-time.After(15 * time.Second):
		t.Fatal("agent run timeout")
	}
}

// TestAgenticCodeAct_ToolBridgeSuccess 验证在 agentic 模式下模型调用 code_act，脚本内通过 polaris.call("read_file") 执行成功。
func TestAgenticCodeAct_ToolBridgeSuccess(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	bridgeExec := newCodeActTestBridgeExecutor()
	caAdapter := setupCodeActProductionEngine(t, bridgeExec)

	agent := NewAgentWithDefaults("test-codeact-agentic-success")
	agent.SetCodeAct(caAdapter)
	agent.InjectPolicyGate(&allowPolicyGate{})
	agent.InjectToolExecutor(&mockToolExecutor{})

	pythonCode := `import polaris
res = polaris.call("read_file", path="doc.txt")
print("RESULT:" + res["content"])
`
	inputBytes, _ := json.Marshal(map[string]any{"language": "python", "code": pythonCode})
	prov := &loopTestProvider{
		responses: []*types.ProviderResponse{
			{ToolCalls: []types.InferToolCall{{ID: "call_code_act_1", Name: "code_act", Input: inputBytes}}},
			{Content: "已成功执行并读取文件内容"},
		},
	}
	agent.InjectProvider(prov)

	runAgenticTurnWait(t, agent)

	if st := agent.sm.Current(); st != types.AgentStateComplete {
		t.Fatalf("agent should complete, current state: %v", st)
	}

	agent.sCtx.Mu.Lock()
	defer agent.sCtx.Mu.Unlock()
	foundResult := false
	for _, m := range agent.sCtx.LoopMessages {
		if strings.Contains(m.Content, "RESULT:file content of doc.txt") {
			foundResult = true
			break
		}
	}
	if !foundResult {
		t.Fatalf("expected tool_result containing 'RESULT:file content of doc.txt', messages: %+v", agent.sCtx.LoopMessages)
	}
}

// TestAgenticCodeAct_ReadOnlyProfileRejection 验证在只读角色下，Python 脚本通过 polaris.call("write_file") 被拒绝。
func TestAgenticCodeAct_ReadOnlyProfileRejection(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	bridgeExec := newCodeActTestBridgeExecutor()
	caAdapter := setupCodeActProductionEngine(t, bridgeExec)

	agent := NewAgentWithDefaults("test-codeact-agentic-readonly")
	agent.SetCodeAct(caAdapter)
	agent.InjectPolicyGate(&allowPolicyGate{})
	agent.InjectToolExecutor(&mockToolExecutor{})
	agent.SetAgentProfile(&types.AgentProfileSpec{Name: "reader", Tools: []string{"Read", "Bash"}})

	pythonCode := `import polaris
try:
    res = polaris.call("write_file", path="out.txt", content="test")
    print("WRITE_RES:", res)
except Exception as e:
    print("WRITE_ERR:" + str(e))
`
	inputBytes, _ := json.Marshal(map[string]any{"language": "python", "code": pythonCode})
	prov := &loopTestProvider{
		responses: []*types.ProviderResponse{
			{ToolCalls: []types.InferToolCall{{ID: "call_code_act_2", Name: "code_act", Input: inputBytes}}},
			{Content: "写入已被安全拒绝"},
		},
	}
	agent.InjectProvider(prov)

	runAgenticTurnWait(t, agent)

	agent.sCtx.Mu.Lock()
	defer agent.sCtx.Mu.Unlock()
	foundErr := false
	for _, m := range agent.sCtx.LoopMessages {
		if strings.Contains(m.Content, "WRITE_ERR:") || strings.Contains(m.Content, "read-only") || strings.Contains(m.Content, "permission denied") || strings.Contains(m.Content, "not permitted") {
			foundErr = true
			break
		}
	}
	if !foundErr {
		t.Fatalf("expected write_file to be rejected by read-only profile, messages: %+v", agent.sCtx.LoopMessages)
	}
}

// TestAgenticCodeAct_TaintPropagation 验证在 full 档（不阻断高污点）下，脚本读到 TaintHigh 结果后，code_act 结果与全局污点为 High。
func TestAgenticCodeAct_TaintPropagation(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	origCfg := config.Get()
	defer config.Update(origCfg)

	var newCfg config.Config
	if origCfg != nil {
		newCfg = *origCfg
	}
	newCfg.Security.TrustProfile = "full"
	newCfg.Thresholds.M4Kernel.LoopMode = "agentic"
	config.Update(&newCfg)

	bridgeExec := newCodeActTestBridgeExecutor()
	caAdapter := setupCodeActProductionEngine(t, bridgeExec)

	agent := NewAgentWithDefaults("test-codeact-agentic-taint")
	agent.SetCodeAct(caAdapter)
	agent.InjectPolicyGate(&allowPolicyGate{})
	agent.InjectToolExecutor(&mockToolExecutor{})

	pythonCode := `import polaris
res = polaris.call("secret_scanner")
print("SECRET_SCANNED:" + res.get("secret", ""))
`
	inputBytes, _ := json.Marshal(map[string]any{"language": "python", "code": pythonCode})
	prov := &loopTestProvider{
		responses: []*types.ProviderResponse{
			{ToolCalls: []types.InferToolCall{{ID: "call_code_act_3", Name: "code_act", Input: inputBytes}}},
			{Content: "已扫描敏感信息"},
		},
	}
	agent.InjectProvider(prov)

	runAgenticTurnWait(t, agent)

	agent.sCtx.Mu.Lock()
	defer agent.sCtx.Mu.Unlock()
	if agent.sCtx.GlobalTaintLevel != types.TaintHigh {
		t.Fatalf("expected GlobalTaintLevel to be TaintHigh, got: %v", agent.sCtx.GlobalTaintLevel)
	}

	foundHighTaintMsg := false
	for _, m := range agent.sCtx.LoopMessages {
		if m.TaintLevel == types.TaintHigh {
			foundHighTaintMsg = true
			break
		}
	}
	if !foundHighTaintMsg {
		t.Fatalf("expected at least one loop message with TaintHigh, messages: %+v", agent.sCtx.LoopMessages)
	}
}

// TestAgenticCodeAct_ToolSchemaAndSpawnPlannerRemoval 验证 agentic 模式下注入 code_act schema 并移除 spawn_planner。
func TestAgenticCodeAct_ToolSchemaAndSpawnPlannerRemoval(t *testing.T) {
	agent := NewAgentWithDefaults("test-codeact-schema")
	cat := catalog.NewMemoryCatalog()
	cat.Register(protocol.CatalogEntry{Name: "read_file", Source: types.ToolBuiltin, TrustTier: types.TrustSystem})
	cat.Register(protocol.CatalogEntry{Name: "spawn_planner", Source: types.ToolBuiltin, TrustTier: types.TrustSystem})
	agent.InjectCatalog(cat)

	ctx := context.Background()
	opts := agent.toolInferOptions(ctx, types.AgentStateLoop, false)
	if len(opts) == 0 {
		t.Fatal("expected non-empty tool infer options in AgentStateLoop")
	}

	var inferOpts types.InferOptions
	for _, opt := range opts {
		opt(&inferOpts)
	}

	foundCodeAct := false
	foundSpawnPlanner := false
	for _, s := range inferOpts.Tools {
		if s.Name == "code_act" {
			foundCodeAct = true
			if !strings.Contains(s.Description, "polaris.call") {
				t.Errorf("code_act schema description should mention polaris.call, got: %s", s.Description)
			}
		}
		if s.Name == "spawn_planner" {
			foundSpawnPlanner = true
		}
	}

	if !foundCodeAct {
		t.Error("expected code_act schema to be present in agentic mode tools")
	}
	if foundSpawnPlanner {
		t.Error("expected spawn_planner to be removed from agentic mode tools")
	}
}
