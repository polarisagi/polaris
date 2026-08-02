package builtin

import (
	"github.com/polarisagi/polaris/pkg/types"
)

func coreMemoryEditTool() types.Tool {
	return types.Tool{
		Name: "core_memory_edit",
		Description: "Edit the agent's core working memory — a set of named text blocks injected into every " +
			"prompt. Treat it as a small, always-visible filesystem you fully control.\n\n" +
			"Operations:\n" +
			"  list     — enumerate all blocks with key, description, size, and byte limit. " +
			"Call this first if you are unsure which block to edit.\n" +
			"  get      — read one block's full content.\n" +
			"  set      — overwrite a block entirely (creates it if absent).\n" +
			"  append   — add text to the end of a block.\n" +
			"  replace  — replace an exact substring inside a block. Prefer this over `set` for large blocks: " +
			"it is cheaper and cannot accidentally drop unrelated text. `old_str` must appear exactly once " +
			"unless `replace_all` is true.\n" +
			"  delete   — remove a block.\n" +
			"  describe — record what a block is for, so future turns pick the right target.\n\n" +
			"Blocks marked read-only cannot be modified. Writes exceeding a block's byte limit are rejected — " +
			"summarize or split the content instead of retrying the same write.",
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
