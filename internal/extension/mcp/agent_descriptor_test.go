package mcp

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// TestListA2AAgents_ExcludesServersWithoutDelegateTool 验证只有暴露
// A2ADelegateToolName 的 Server 才被视为具备 A2A 能力（ADR-0084）。
func TestListA2AAgents_ExcludesServersWithoutDelegateTool(t *testing.T) {
	m := NewMCPManager(nil, http.DefaultClient, &mockPolicyGate{})
	m.entries["srv-plain"] = &mcpEntry{
		name:  "plain-server",
		tools: []MCPTool{{Name: "some_other_tool"}},
	}

	descriptors, err := m.ListA2AAgents(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(descriptors) != 0 {
		t.Fatalf("expected 0 descriptors for server without a2a_delegate, got %d: %v", len(descriptors), descriptors)
	}
}

// TestListA2AAgents_ExcludesTombstonedEntries 验证连接失败（errMsg 非空）的
// tombstone entry 不参与枚举，即便其 tools 列表恰好包含 a2a_delegate（残留
// 数据，理论上不应出现，但防御性排除）。
func TestListA2AAgents_ExcludesTombstonedEntries(t *testing.T) {
	m := NewMCPManager(nil, http.DefaultClient, &mockPolicyGate{})
	m.entries["srv-dead"] = &mcpEntry{
		name:   "dead-server",
		errMsg: "connection refused",
		tools:  []MCPTool{{Name: A2ADelegateToolName}},
	}

	descriptors, err := m.ListA2AAgents(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(descriptors) != 0 {
		t.Fatalf("expected tombstoned entry excluded, got %d descriptors", len(descriptors))
	}
}

// TestListA2AAgents_NoListToolFallsBackToDefault 验证暴露 a2a_delegate 但未
// 暴露 a2a_list_agents 的 Server 退化为单条 "default" 描述符。
func TestListA2AAgents_NoListToolFallsBackToDefault(t *testing.T) {
	m := NewMCPManager(nil, http.DefaultClient, &mockPolicyGate{})
	m.entries["srv-1"] = &mcpEntry{
		name:  "linear",
		tools: []MCPTool{{Name: A2ADelegateToolName}},
	}

	descriptors, err := m.ListA2AAgents(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(descriptors) != 1 {
		t.Fatalf("expected exactly 1 fallback descriptor, got %d: %v", len(descriptors), descriptors)
	}
	if descriptors[0].Server != "linear" || descriptors[0].Agent != "default" {
		t.Errorf("expected {linear, default}, got %+v", descriptors[0])
	}
}

// TestListA2AAgents_ListToolPresentDegradesGracefullyOnCallFailure 验证
// a2a_list_agents 存在但底层 CallTool 失败（本测试未注入 Envelope，天然失败）
// 时不 panic、不中断，而是退化为携带错误说明的 "default" 描述符。
func TestListA2AAgents_ListToolPresentDegradesGracefullyOnCallFailure(t *testing.T) {
	m := NewMCPManager(nil, http.DefaultClient, &mockPolicyGate{})
	m.entries["srv-1"] = &mcpEntry{
		name:  "linear",
		tools: []MCPTool{{Name: A2ADelegateToolName}, {Name: A2AListAgentsToolName}},
	}

	descriptors, err := m.ListA2AAgents(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(descriptors) != 1 {
		t.Fatalf("expected 1 degraded descriptor, got %d: %v", len(descriptors), descriptors)
	}
	if descriptors[0].Server != "linear" || descriptors[0].Agent != "default" {
		t.Errorf("expected degraded {linear, default}, got %+v", descriptors[0])
	}
	if !strings.Contains(descriptors[0].Description, "a2a_list_agents call failed") {
		t.Errorf("expected description to explain the call failure, got %q", descriptors[0].Description)
	}
}

// TestResolveServerIDByName_FoundAndNotFound 验证按 LLM 侧服务器名反查
// serverID，且跳过 tombstone entry。
func TestResolveServerIDByName_FoundAndNotFound(t *testing.T) {
	m := NewMCPManager(nil, http.DefaultClient, &mockPolicyGate{})
	m.entries["srv-1"] = &mcpEntry{name: "linear"}
	m.entries["srv-dead"] = &mcpEntry{name: "dead", errMsg: "boom"}

	if id, ok := m.ResolveServerIDByName("linear"); !ok || id != "srv-1" {
		t.Errorf("expected (srv-1, true), got (%q, %v)", id, ok)
	}
	if _, ok := m.ResolveServerIDByName("dead"); ok {
		t.Error("expected tombstoned server name to not resolve")
	}
	if _, ok := m.ResolveServerIDByName("nonexistent"); ok {
		t.Error("expected unknown server name to not resolve")
	}
}

// TestMCPToolCaller_SatisfiedByMCPManager 编译期/运行期双重确认
// *MCPManager 结构子类型满足 orchestrator.MCPToolCaller（HE-3 consumer-side
// 接口，internal/execute 不 import 本包，靠结构子类型自然满足）。
func TestMCPToolCaller_SatisfiedByMCPManager(t *testing.T) {
	var _ interface {
		CallTool(ctx context.Context, serverID, toolName string, args map[string]any) (string, error)
		ResolveServerIDByName(name string) (string, bool)
	} = (*MCPManager)(nil)
}
