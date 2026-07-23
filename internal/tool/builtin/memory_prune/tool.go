package memory_prune

import (
	"context"
	_ "embed"
	"encoding/json"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/tool"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type PruneArgs struct {
	EntityType string `json:"entity_type"`
	EntityName string `json:"entity_name"`
	Reason     string `json:"reason"`
}

type MemoryPruneTool struct {
	cfg types.Tool
	mem protocol.SemanticMemory
}

func NewMemoryPruneTool(mem protocol.SemanticMemory) *MemoryPruneTool {
	return &MemoryPruneTool{mem: mem}
}

func (t *MemoryPruneTool) Spec() types.Tool {
	if t.cfg.Name == "" {
		t.cfg, _ = tool.GetBuiltinToolMeta("memory_prune")
	}
	return t.cfg
}

func (t *MemoryPruneTool) Execute(ctx context.Context, args []byte) (*types.ToolResult, error) {
	var p PruneArgs
	if err := json.Unmarshal(args, &p); err != nil {
		return &types.ToolResult{Error: "invalid arguments"}, nil
	}

	if t.mem == nil {
		return nil, apperr.New(apperr.CodeInternal, "semantic memory not injected")
	}

	err := t.mem.MarkEntityExpired(ctx, p.EntityType, p.EntityName, p.Reason)
	if err != nil {
		return &types.ToolResult{Error: err.Error()}, nil
	}

	return &types.ToolResult{
		Output: []byte("Successfully pruned entity: " + p.EntityName),
	}, nil
}
