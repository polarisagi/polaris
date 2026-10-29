package types

import (
	"encoding/json"
	"testing"
)

func TestBuildToolUIRef(t *testing.T) {
	ok := &ToolResult{Success: true, MCPRaw: json.RawMessage(`{"content":[]}`)}
	if ref := BuildToolUIRef("v1", "ui://s/x", "srv", "tool", []byte(`{}`), ok); ref == nil || ref.ViewID != "v1" || ref.ServerID != "srv" || string(ref.ToolResult) != `{"content":[]}` {
		t.Fatalf("unexpected ref %+v", ref)
	}
	if ref := BuildToolUIRef("", "ui://s/x", "srv", "tool", nil, ok); ref == nil || ref.ViewID == "" {
		t.Fatalf("empty viewID must be generated, got %+v", ref)
	}
	for name, res := range map[string]*ToolResult{
		"nil":    nil,
		"failed": {Success: false, MCPRaw: json.RawMessage(`{}`)},
		"no raw": {Success: true},
	} {
		if BuildToolUIRef("v", "ui://s/x", "srv", "tool", nil, res) != nil {
			t.Fatalf("%s result must not yield a ref", name)
		}
	}
	if BuildToolUIRef("v", "", "srv", "tool", nil, ok) != nil {
		t.Fatal("no resource uri must not yield a ref")
	}
}
