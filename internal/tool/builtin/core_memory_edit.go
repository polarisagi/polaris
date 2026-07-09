package builtin

import (
	"github.com/polarisagi/polaris/pkg/types"
)

func coreMemoryEditTool() types.Tool {
	return types.Tool{
		Name: "core_memory_edit",
		Description: "Edit the agent's core working memory. Core memory is a set of text blocks injected into every prompt. " +
			"Use it to maintain persistent state for long-running tasks, persona constraints, and user preferences. " +
			"Operations available: set, append, delete.",
		Version:     "1.0.0",
		Source:      types.ToolBuiltin,
		TrustTier:   types.TrustSystem,
		Capability:  types.CapWriteLocal,
		RiskLevel:   types.RiskLow,
		SandboxTier: types.SandboxInProcess,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"operation": map[string]any{
					"type":        "string",
					"enum":        []string{"set", "append", "delete"},
					"description": "The operation to perform on the core memory block.",
				},
				"block_key": map[string]any{
					"type":        "string",
					"description": "The unique key of the memory block (e.g. 'persona', 'task_state', 'user_prefs').",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "The content to set or append. Ignored for 'delete' operation.",
				},
			},
			"required": []string{"operation", "block_key"},
		},
	}
}
