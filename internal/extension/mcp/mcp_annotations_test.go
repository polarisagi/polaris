package mcp

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

func boolPtr(b bool) *bool {
	return &b
}

// TestMCPToolAnnotations_JSONParsing 验证 tools/list 返回的 annotations 字段反序列化。
func TestMCPToolAnnotations_JSONParsing(t *testing.T) {
	rawJSON := `{
		"name": "query_items",
		"description": "Read items from external API",
		"inputSchema": {"type": "object"},
		"annotations": {
			"title": "Query Items Tool",
			"readOnlyHint": true,
			"destructiveHint": false,
			"idempotentHint": true,
			"openWorldHint": false
		}
	}`

	var tool MCPTool
	if err := json.Unmarshal([]byte(rawJSON), &tool); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if tool.Annotations == nil {
		t.Fatal("expected Annotations to be parsed, got nil")
	}
	if tool.Annotations.Title != "Query Items Tool" {
		t.Errorf("expected Title 'Query Items Tool', got %q", tool.Annotations.Title)
	}
	if tool.Annotations.ReadOnlyHint == nil || !*tool.Annotations.ReadOnlyHint {
		t.Errorf("expected readOnlyHint=true, got %v", tool.Annotations.ReadOnlyHint)
	}
	if tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
		t.Errorf("expected destructiveHint=false, got %v", tool.Annotations.DestructiveHint)
	}
	if tool.Annotations.IdempotentHint == nil || !*tool.Annotations.IdempotentHint {
		t.Errorf("expected idempotentHint=true, got %v", tool.Annotations.IdempotentHint)
	}

	// 验证无 annotations 的反序列化
	noAnnoJSON := `{"name": "do_something", "description": "some tool", "inputSchema": {}}`
	var tool2 MCPTool
	if err := json.Unmarshal([]byte(noAnnoJSON), &tool2); err != nil {
		t.Fatalf("Unmarshal noAnno failed: %v", err)
	}
	if tool2.Annotations != nil {
		t.Errorf("expected nil Annotations, got %+v", tool2.Annotations)
	}
}

// TestMCPTool_RegistryToolReadOnlyHint 验证 registryTool 对 ReadOnlyHint 的推导规则：
// 1. 无 annotations -> ReadOnlyHint = false
// 2. readOnlyHint=true 且 destructiveHint!=true -> ReadOnlyHint = true
// 3. readOnlyHint=true 但 destructiveHint=true -> ReadOnlyHint = false (destructive 优先覆盖)
func TestMCPTool_RegistryToolReadOnlyHint(t *testing.T) {
	client := &MCPClient{cfg: MCPClientConfig{ServerName: "test_srv"}}

	tests := []struct {
		name         string
		annotations  *MCPToolAnnotations
		wantReadOnly bool
	}{
		{
			name:         "nil annotations",
			annotations:  nil,
			wantReadOnly: false,
		},
		{
			name: "readOnlyHint=true, no destructiveHint",
			annotations: &MCPToolAnnotations{
				ReadOnlyHint: boolPtr(true),
			},
			wantReadOnly: true,
		},
		{
			name: "readOnlyHint=true, destructiveHint=false",
			annotations: &MCPToolAnnotations{
				ReadOnlyHint:    boolPtr(true),
				DestructiveHint: boolPtr(false),
			},
			wantReadOnly: true,
		},
		{
			name: "readOnlyHint=true, destructiveHint=true (destructive overrides)",
			annotations: &MCPToolAnnotations{
				ReadOnlyHint:    boolPtr(true),
				DestructiveHint: boolPtr(true),
			},
			wantReadOnly: false,
		},
		{
			name: "readOnlyHint=false, destructiveHint=false",
			annotations: &MCPToolAnnotations{
				ReadOnlyHint:    boolPtr(false),
				DestructiveHint: boolPtr(false),
			},
			wantReadOnly: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mcpTool := MCPTool{
				Name:        "tool_x",
				Description: "desc",
				Annotations: tt.annotations,
			}
			res := registryTool(client, "tool_x", mcpTool, 30*time.Second)
			if res.Source != types.ToolMCP {
				t.Fatalf("expected Source == ToolMCP, got %v", res.Source)
			}
			if res.ReadOnlyHint != tt.wantReadOnly {
				t.Errorf("expected ReadOnlyHint=%v, got %v", tt.wantReadOnly, res.ReadOnlyHint)
			}
		})
	}
}

// TestMCPTool_ToolEqualAnnotations 验证 toolEqual 识别 Annotations 的变更。
func TestMCPTool_ToolEqualAnnotations(t *testing.T) {
	toolA := MCPTool{
		Name:        "tool_a",
		Description: "desc",
		Annotations: &MCPToolAnnotations{
			ReadOnlyHint: boolPtr(true),
		},
	}

	toolB := MCPTool{
		Name:        "tool_a",
		Description: "desc",
		Annotations: &MCPToolAnnotations{
			ReadOnlyHint: boolPtr(true),
		},
	}

	if !toolEqual(toolA, toolB) {
		t.Error("expected toolA and toolB to be equal")
	}

	// 改变 ReadOnlyHint
	toolB.Annotations.ReadOnlyHint = boolPtr(false)
	if toolEqual(toolA, toolB) {
		t.Error("expected toolA and toolB not to be equal after changing ReadOnlyHint")
	}

	// 改变 DestructiveHint
	toolB.Annotations.ReadOnlyHint = boolPtr(true)
	toolB.Annotations.DestructiveHint = boolPtr(true)
	if toolEqual(toolA, toolB) {
		t.Error("expected toolA and toolB not to be equal after changing DestructiveHint")
	}

	// 一个有 annotations 一个为 nil
	toolB.Annotations = nil
	if toolEqual(toolA, toolB) {
		t.Error("expected toolA and toolB not to be equal when one has nil annotations")
	}
}
