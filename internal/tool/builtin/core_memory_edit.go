package builtin

import (
	"github.com/polarisagi/polaris/pkg/types"
)

func coreMemoryEditTool() types.Tool {
	return types.Tool{
		Name: "core_memory_edit",
		Description: "Edit core working memory: named text blocks injected into every prompt. " +
			"Operations: list (keys, sizes, limits; call first if unsure), get, set (overwrite/create), append, " +
			"replace (exact old_str→new_str; must match once unless replace_all; prefer over set for large blocks), " +
			"delete, describe (record a block's purpose). Read-only blocks can't change; writes over a block's byte " +
			"limit are rejected — summarize or split instead of retrying." +
			" Changes are saved immediately; in the current session the result shown here is what you see, and the always-visible memory block refreshes at the next context refresh (new session).",
		Version:     "2.0.0",
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
					"enum":        []string{"list", "get", "set", "append", "replace", "delete", "describe"},
					"description": "The operation to perform on the core memory block.",
				},
				"block_key": map[string]any{
					"type": "string",
					"description": "The unique key of the memory block (e.g. 'persona', 'task_state', 'user_prefs'). " +
						"Not required for 'list'.",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "The content to set or append. Required for 'set'/'append', ignored otherwise.",
				},
				"old_str": map[string]any{
					"type":        "string",
					"description": "Required for 'replace'. The exact substring to find inside the block.",
				},
				"new_str": map[string]any{
					"type":        "string",
					"description": "Required for 'replace'. The text to substitute in place of old_str.",
				},
				"replace_all": map[string]any{
					"type":        "boolean",
					"description": "For 'replace': if true, replace every occurrence of old_str instead of requiring exactly one match. Default false.",
				},
				"description": map[string]any{
					"type":        "string",
					"description": "Required for 'describe'. A one-sentence note on what this block is for.",
				},
			},
			"required": []string{"operation"},
		},
	}
}
