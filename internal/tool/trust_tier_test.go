package tool

import (
	"testing"
)

func TestToolSearchTrustTier(t *testing.T) {
	tool, err := LoadBuiltinToolMeta("tool_search")
	if err != nil {
		t.Fatal(err)
	}
	if tool.TrustTier != 4 {
		t.Fatalf("expected TrustTier 4, got %d", tool.TrustTier)
	}
	t.Logf("Tool TrustTier: %d", tool.TrustTier)
}
