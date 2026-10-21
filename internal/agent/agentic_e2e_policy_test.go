package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/internal/tool"
	"github.com/polarisagi/polaris/internal/tool/builtin"
	"github.com/polarisagi/polaris/internal/tool/builtin/web_fetch"
	"github.com/polarisagi/polaris/internal/tool/catalog"
	"github.com/polarisagi/polaris/internal/tool/dispatch"
	"github.com/polarisagi/polaris/pkg/types"
)

// fetchStubExecutor 在生产装配的 dispatcher 之上桩出一个返回 TaintHigh 结果的 web_fetch：
// 真实 web_fetch 要出网，而本场景只关心「高污点结果之后」策略层如何处置写类调用。
type fetchStubExecutor struct{ inner protocol.AgentToolExecutor }

func (e fetchStubExecutor) Lookup(name string) (types.Tool, error) {
	if name == "web_fetch" {
		return types.Tool{Name: name, Source: types.ToolBuiltin, Capability: types.CapReadOnly}, nil
	}
	return e.inner.Lookup(name)
}

func (e fetchStubExecutor) ExecuteWithTaint(ctx context.Context, name string, input []byte, lvl types.TaintLevel) (*types.ToolResult, error) {
	if name == "web_fetch" {
		return &types.ToolResult{Success: true, Output: []byte("FETCHED-PAGE"), TaintLevel: types.TaintHigh}, nil
	}
	return e.inner.ExecuteWithTaint(ctx, name, input, lvl)
}

// TestAgentic_E2EContract_StandardPolicyTaint 契约（R5-05）：生产默认策略门（policy.NewGate，
// deny-by-default 的 Cedar/Go 规则）+ standard 档 + 用户意图 TaintHigh（系统约定）。
// web_fetch 的高污点结果之后，写类工具调用必须被拒绝（HITL 未接入时即拒绝），
// 文件不得落盘，拒绝原因以 tool_result 形式回到模型。
func TestAgentic_E2EContract_StandardPolicyTaint(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	origCfg := config.Get()
	defer config.Update(origCfg)
	var newCfg config.Config
	if origCfg != nil {
		newCfg = *origCfg
	}
	newCfg.Security.TrustProfile = "standard"
	newCfg.Thresholds = config.DefaultThresholds()
	newCfg.Thresholds.M4Kernel.LoopMode = "agentic"
	config.Update(&newCfg)

	tmpDir := t.TempDir()
	target := filepath.Join(tmpDir, "out.txt")

	gate := policy.NewGate(nil).WithEvalTimeout(2 * time.Second)
	sbx := sandbox.NewInProcessSandbox(config.DefaultThresholds().M7Tool)
	router := sandbox.NewSandboxRouter(sbx, nil, nil, runtime.GOOS, 0)
	envelope := sandbox.NewExecEnvelope(gate, router, 0, "", nil)
	toolReg := tool.NewInMemoryToolRegistry(envelope, config.DefaultThresholds().M7Tool)
	err := builtin.RegisterBuiltinTools(sbx, toolReg, []string{tmpDir}, nil, false, protocol.NetPolicyDeny, "", &config.Config{}, nil, "", nil, nil, nil, web_fetch.Deps{}, policy.StandardPolicy(), "", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("RegisterBuiltinTools failed: %v", err)
	}
	memCat := catalog.NewMemoryCatalog()
	for _, tl := range toolReg.List() {
		memCat.Register(protocol.CatalogEntry{Name: tl.Name, Description: tl.Description, Parameters: tl.InputSchema, Source: tl.Source, TrustTier: tl.TrustTier})
	}

	ag := NewAgentWithDefaults("sess-e2e-policy-taint")
	ag.InjectToolExecutor(fetchStubExecutor{inner: dispatch.New(memCat, toolReg, nil)})
	ag.InjectPolicyGate(gate)

	prov := &e2eContractProvider{responses: []*types.ProviderResponse{
		{ToolCalls: []types.InferToolCall{{ID: "c_fetch", Name: "web_fetch", Input: []byte(`{"url":"https://example.com"}`)}}},
		{ToolCalls: []types.InferToolCall{{ID: "c_write", Name: "write_file", Input: []byte(fmt.Sprintf(`{"path":%q,"content":"pwned"}`, target))}}},
		{Content: "写入被策略拒绝，已向你说明。"},
	}}
	ag.InjectProvider(prov)
	ag.SetTaskIntent(taint.NewTaintedString("抓取网页并把内容写入文件", taint.TaintSource{OriginTaintLevel: types.TaintHigh}, "e2e"))

	runAgenticTurnWait(t, ag)

	if _, statErr := os.Stat(target); statErr == nil {
		t.Fatal("高污点之后的写类调用被放行：文件已落盘")
	}
	prov.mu.Lock()
	reqs := prov.requests
	prov.mu.Unlock()
	if len(reqs) != 3 {
		t.Fatalf("预期 3 次 LLM 请求，实际 %d", len(reqs))
	}

	// 第 3 次请求里必须带着 c_write 的 tool_result，且是拒绝原因而非成功输出。
	var writeResult string
	for _, m := range reqs[2] {
		for _, p := range m.Parts {
			if pm, ok := p.(map[string]any); ok && pm["type"] == "tool_result" && pm["tool_use_id"] == "c_write" {
				writeResult, _ = pm["content"].(string)
			}
		}
	}
	if writeResult == "" {
		t.Fatal("拒绝原因没有以 tool_result 形式回到模型")
	}
	if !strings.Contains(writeResult, "Forbidden") {
		t.Errorf("tool_result 应表明被安全策略拒绝，实际: %q", writeResult)
	}
	// 读类的高污点结果本身应正常回流（拒绝的是写，不是整个回合）。
	var fetchOK bool
	for _, m := range reqs[1] {
		if strings.Contains(m.Content, "FETCHED-PAGE") {
			fetchOK = true
		}
	}
	if !fetchOK {
		t.Error("web_fetch 的结果应回流给模型")
	}
}
