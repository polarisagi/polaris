package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/execute/dag"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// mcpUIToolExecutor 模拟一次成功的 MCP 工具调用，返回携带 MCPRaw 的 ToolResult
// （M8f-1：MCPRaw 是"结果对象上的附加字段"，不改变 Output 本身）。
type mcpUIToolExecutor struct{ raw json.RawMessage }

func (m *mcpUIToolExecutor) Lookup(name string) (types.Tool, error) {
	return types.Tool{Name: name, Source: types.ToolMCP, Capability: types.CapReadOnly}, nil
}
func (m *mcpUIToolExecutor) ExecuteWithTaint(_ context.Context, name string, _ []byte, _ types.TaintLevel) (*types.ToolResult, error) {
	return &types.ToolResult{Success: true, Output: []byte("ok"), MCPRaw: m.raw}, nil
}

// fakeUICatalog 最小 catalog.Catalog 假实现，仅用于 Lookup 断言。
type fakeUICatalog struct {
	entries map[string]protocol.CatalogEntry
}

func (f *fakeUICatalog) List(context.Context, types.TrustTier) []protocol.CatalogEntry { return nil }
func (f *fakeUICatalog) Lookup(name string) (protocol.CatalogEntry, bool) {
	e, ok := f.entries[name]
	return e, ok
}
func (f *fakeUICatalog) Register(protocol.CatalogEntry) {}
func (f *fakeUICatalog) Unregister(string)              {}
func (f *fakeUICatalog) Invalidate()                    {}
func (f *fakeUICatalog) Schemas(context.Context, types.TrustTier) []types.ToolSchema {
	return nil
}

// waitForToolResultEvent 从流事件通道中取出第一条 tool_result 事件（带超时，
// 避免测试挂起）；channel 缓冲 100，runExecuteDAG 已同步执行完毕，不存在竞态。
func waitForToolResultEvent(t *testing.T, ch <-chan types.AgentStreamEvent) types.AgentStreamEvent {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Type == types.AgentStreamEventToolResult {
				return ev
			}
		case <-deadline:
			t.Fatal("timed out waiting for tool_result event")
		}
	}
}

// TestAgent_ExecuteDAG_PublishesToolUIRefForMCPToolWithResourceURI 验证带 UI 的
// MCP 工具调用产生 AgentStreamEvent.UI（session 编排层据此发 KindStatus
// "tool_ui"），且模型可见输出（Content）不受影响。
func TestAgent_ExecuteDAG_PublishesToolUIRefForMCPToolWithResourceURI(t *testing.T) {
	agentInst := NewAgentWithDefaults("test-agent-ui")
	agentInst.InjectDAGRunner(&dummyDAGRunner{})
	raw := json.RawMessage(`{"content":[{"type":"text","text":"hi"}]}`)
	agentInst.InjectToolExecutor(&mcpUIToolExecutor{raw: raw})
	agentInst.InjectCatalog(&fakeUICatalog{entries: map[string]protocol.CatalogEntry{
		"mcp__srv1__dash": {Name: "mcp__srv1__dash", ResourceURI: "ui://srv1/dash", MCPServerID: "srv1", MCPToolName: "dash"},
	}})
	plan := &fsm.DAGModel{
		Nodes: []dag.ExecNode{{ID: "n1", ToolName: "mcp__srv1__dash", Args: []byte(`{"x":1}`)}},
	}
	agentInst.sCtx.DAGModel = plan
	// withJITCapability 要求本次调用与已通过 S_VALIDATE 的计划节点逐字节一致
	// （agent_capability.go），测试直接调用 runExecuteDAG 跳过了 S_VALIDATE 阶段，
	// 需手动补上这一步，否则工具调用在能力令牌闸门就被拒绝，与本用例无关。
	agentInst.recordValidatedPlan(&protocol.DAGPlan{Nodes: plan.Nodes})

	ch := agentInst.SubscribeStream(context.Background())
	if err := agentInst.runExecuteDAG(context.Background()); err != nil {
		t.Fatalf("runExecuteDAG: %v", err)
	}

	ev := waitForToolResultEvent(t, ch)
	if ev.UI == nil {
		t.Fatal("expected non-nil UI ref for MCP tool with ResourceURI")
	}
	if ev.UI.ServerID != "srv1" || ev.UI.ResourceURI != "ui://srv1/dash" || ev.UI.ToolName != "dash" || ev.UI.ViewID == "" {
		t.Fatalf("unexpected UI ref: %+v", ev.UI)
	}
	if string(ev.UI.ToolResult) != string(raw) {
		t.Fatalf("ToolResult must carry raw MCPRaw JSON: %s", ev.UI.ToolResult)
	}
	if ev.Content != "ok" {
		t.Fatalf("model-visible Content must be unaffected by UI ref construction: %q", ev.Content)
	}
}

// TestAgent_ExecuteDAG_NoToolUIRefWithoutResourceURI 验证普通（无 UI 元数据）工具
// 调用不会被误判为带 UI。
func TestAgent_ExecuteDAG_NoToolUIRefWithoutResourceURI(t *testing.T) {
	agentInst := NewAgentWithDefaults("test-agent-no-ui")
	agentInst.InjectDAGRunner(&dummyDAGRunner{})
	agentInst.InjectToolExecutor(&mockToolExecutor{})
	plan := &fsm.DAGModel{
		Nodes: []dag.ExecNode{{ID: "n1", ToolName: "read_file"}},
	}
	agentInst.sCtx.DAGModel = plan
	agentInst.recordValidatedPlan(&protocol.DAGPlan{Nodes: plan.Nodes})

	ch := agentInst.SubscribeStream(context.Background())
	if err := agentInst.runExecuteDAG(context.Background()); err != nil {
		t.Fatalf("runExecuteDAG: %v", err)
	}

	ev := waitForToolResultEvent(t, ch)
	if ev.UI != nil {
		t.Fatalf("plain tool without catalog ResourceURI must not carry UI ref: %+v", ev.UI)
	}
}
