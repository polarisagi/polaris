package builtin

import (
	"context"
	"fmt"

	"github.com/polarisagi/polaris/internal/tool"

	"github.com/polarisagi/polaris/internal/memory/retrieval"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// 记忆工具集 — consumer-side 接口定义（防 pkg/action ↔ pkg/cognition 包循环）
// 实现由 pkg/cognition/memory.SemanticMem 提供，在 cmd/polaris/main.go 注入。
// ============================================================================

// SemanticMemWriter 语义记忆写入接口（consumer-side，L1 层内互引禁止）。
// 与 pkg/cognition/memory.SemanticMemWriter 保持方法签名一致，Go 结构子类型自动满足。
type SemanticMemWriter interface {
	UpsertFact(ctx context.Context, entity types.Entity, taint types.TaintLevel) error
	Archive(ctx context.Context, id string, reason string) error
	GetEntity(ctx context.Context, entityType, name string) (*types.Entity, error)
}

// RegisterMemoryTools 注册 4 个记忆工具到 InProcessSandbox 和 tool.InMemoryToolRegistry。
// 注意：工具元数据内联构造，不依赖 tool.GetBuiltinToolMeta（避免 builtin/<name>/ 目录缺失导致
// 静默跳过——这是 Gemini 首版实现的功能 bug，此处修复）。
func RegisterMemoryTools(
	sbx *sandbox.InProcessSandbox,
	toolReg *tool.InMemoryToolRegistry,
	exclusiveWriter *retrieval.ExclusiveWriter,
	semanticWriter SemanticMemWriter,
	retriever protocol.HybridRetriever,
	reflection protocol.ReflectionMemory,
	coreMemory protocol.CoreMemory,
) error {
	type entry struct {
		tool types.Tool
		fn   sandbox.InProcessFn
	}

	entries := []entry{
		{tool: memoryWriteTool(), fn: MakeMemoryWriteFn(exclusiveWriter)},
		{tool: memorySearchTool(), fn: MakeMemorySearchFn(retriever)},
		{tool: memoryAppendTool(), fn: MakeMemoryAppendFn(semanticWriter)},
		{tool: memoryExpireTool(), fn: MakeMemoryExpireFn(semanticWriter)},
		{tool: memoryReflectTool(), fn: MakeMemoryReflectFn(reflection)},
		{tool: coreMemoryEditTool(), fn: MakeCoreMemoryEditFn(coreMemory)},
		{tool: memoryPageOutTool(), fn: MakeMemoryPageOutFn(coreMemory, semanticWriter)},
		{tool: memoryPageInTool(), fn: MakeMemoryPageInFn(coreMemory, semanticWriter)},
	}

	for _, e := range entries {
		sbx.Register(e.tool.Name, e.fn)
		if err := toolReg.Register(e.tool); err != nil {
			return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("memory_tools: register %s", e.tool.Name), err)
		}
	}
	return nil
}

// ─── 工具元数据（内联，不依赖 builtin/ embed FS）─────────────────────────────

func memoryWriteTool() types.Tool {
	return types.Tool{
		Name: "memory_write",
		Description: "Write a factual statement to long-term semantic memory. " +
			"Use for user preferences, project facts, key constraints, and decisions " +
			"that should persist across conversations.",
		Version:     "1.0.0",
		Source:      types.ToolBuiltin,
		TrustTier:   types.TrustSystem,
		Capability:  types.CapWriteLocal,
		RiskLevel:   types.RiskLow,
		SandboxTier: types.SandboxInProcess,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"description": "Entity name or fact key (unique within entity_type)",
				},
				"entity_type": map[string]any{
					"type":        "string",
					"description": "Type: Person | Project | Tool | Concept | Preference | Fact | Constraint",
					"default":     "Fact",
				},
				"description": map[string]any{
					"type":        "string",
					"description": "The factual content to store",
				},
				"valid_until": map[string]any{
					"type":        "string",
					"description": "Optional: duration string until fact expires, e.g. '24h', '7d'. Omit for permanent.",
				},
			},
			"required": []string{"name", "description"},
		},
	}
}

func memorySearchTool() types.Tool {
	return types.Tool{
		Name: "memory_search",
		Description: "Search long-term memory using hybrid retrieval (BM25 + vector + graph). " +
			"Returns the most relevant facts, preferences, and past context.",
		Version:     "1.0.0",
		Source:      types.ToolBuiltin,
		TrustTier:   types.TrustSystem,
		Capability:  types.CapReadOnly,
		RiskLevel:   types.RiskLow,
		SandboxTier: types.SandboxInProcess,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "Natural language search query",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Max results to return (default 5, max 20)",
					"default":     5,
				},
				"layer": map[string]any{
					"type":        "string",
					"description": "Memory layer to search: 'memory' (episodic + facts, default) | 'semantic' (stored facts and documents only)",
					"default":     "memory",
				},
				"as_of": map[string]any{
					"type":        "integer",
					"description": "Optional: Unix timestamp (ms) to search the memory graph as it existed at a specific time in the past.",
				},
			},
			"required": []string{"query"},
		},
	}
}

func memoryAppendTool() types.Tool {
	return types.Tool{
		Name: "memory_append",
		Description: "Append additional information to an existing memory entity " +
			"(same name + entity_type). Useful for accumulating related facts without " +
			"overwriting the original.",
		Version:     "1.0.0",
		Source:      types.ToolBuiltin,
		TrustTier:   types.TrustSystem,
		Capability:  types.CapWriteLocal,
		RiskLevel:   types.RiskLow,
		SandboxTier: types.SandboxInProcess,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"entity_type": map[string]any{
					"type":    "string",
					"default": "Fact",
				},
				"name": map[string]any{
					"type":        "string",
					"description": "Name of the existing entity to append to",
				},
				"prop_key": map[string]any{
					"type":        "string",
					"description": "Property key to set or update",
				},
				"prop_value": map[string]any{
					"type":        "string",
					"description": "Property value",
				},
			},
			"required": []string{"name", "prop_key", "prop_value"},
		},
	}
}

func memoryExpireTool() types.Tool {
	return types.Tool{
		Name: "memory_expire",
		Description: "Mark a memory entry as expired/invalid. Use when you learn that a " +
			"stored fact is no longer true (e.g. a user's project has changed, a preference " +
			"was updated, or a constraint no longer applies).",
		Version:     "1.0.0",
		Source:      types.ToolBuiltin,
		TrustTier:   types.TrustSystem,
		Capability:  types.CapWriteLocal,
		RiskLevel:   types.RiskLow,
		SandboxTier: types.SandboxInProcess,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"entity_type": map[string]any{
					"type":    "string",
					"default": "Fact",
				},
				"name": map[string]any{
					"type":        "string",
					"description": "Name of the entity to expire",
				},
				"reason": map[string]any{
					"type":        "string",
					"description": "Why this fact is no longer valid (audit trail)",
				},
			},
			"required": []string{"name"},
		},
	}
}

// memoryPageOutTool / memoryPageInTool（GD-14-002 最小实现）：
// 上下文主动置换。Core Memory 是每轮 Prompt 都会注入的稀缺资源（M5Memory
// CoreMemoryTotalMaxKB 硬顶），长程任务容易被非核心内容（如已处理完的某个
// 子任务详情）占满。这两个工具允许 Agent 自主判断"当前哪些内容不再需要
// 每轮可见，但未来可能还要用到"，主动把它从 Core Memory 移出到长期语义
// 记忆（page_out，仍可通过 memory_search / memory_page_in 找回），需要时
// 再取回注入（page_in）。是否调用、何时调用完全由 LLM 自主决定——Go 侧不
// 设强制阈值触发（那是既有的 ForgettingManager/consolidation 全局回收机制，
// 与本工具是两回事，见任务书 8 §8.5 步骤 3 的设计原则）。
func memoryPageOutTool() types.Tool {
	return types.Tool{
		Name: "memory_page_out",
		Description: "Page out a core memory block you no longer need visible in every prompt turn " +
			"(e.g. details of a sub-task you've already finished), while keeping it durably retrievable " +
			"later via memory_search or memory_page_in. Use this when you notice context pressure " +
			"(long-running task, core memory getting crowded) and want to free up space without losing " +
			"the information permanently.",
		Version:     "1.0.0",
		Source:      types.ToolBuiltin,
		TrustTier:   types.TrustSystem,
		Capability:  types.CapWriteLocal,
		RiskLevel:   types.RiskLow,
		SandboxTier: types.SandboxInProcess,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"block_key": map[string]any{
					"type":        "string",
					"description": "The core memory block key to page out (same key used with core_memory_edit).",
				},
				"reason": map[string]any{
					"type":        "string",
					"description": "Optional: why this content is being paged out (audit trail).",
				},
			},
			"required": []string{"block_key"},
		},
	}
}

func memoryPageInTool() types.Tool {
	return types.Tool{
		Name: "memory_page_in",
		Description: "Page a previously paged-out core memory block back into every-prompt visibility. " +
			"Use this when you determine that a previously deprioritized piece of context is relevant again.",
		Version:     "1.0.0",
		Source:      types.ToolBuiltin,
		TrustTier:   types.TrustSystem,
		Capability:  types.CapWriteLocal,
		RiskLevel:   types.RiskLow,
		SandboxTier: types.SandboxInProcess,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"block_key": map[string]any{
					"type":        "string",
					"description": "The core memory block key to page back in (must have been paged out previously).",
				},
			},
			"required": []string{"block_key"},
		},
	}
}

func memoryReflectTool() types.Tool {
	return types.Tool{
		Name: "memory_reflect",
		Description: "Trigger an explicit reflection on past events to generate high-level insights or rules. " +
			"Useful when the agent notices a recurring pattern or makes a significant mistake.",
		Version:     "1.0.0",
		Source:      types.ToolBuiltin,
		TrustTier:   types.TrustSystem,
		Capability:  types.CapWriteLocal,
		RiskLevel:   types.RiskLow,
		SandboxTier: types.SandboxInProcess,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"topic": map[string]any{
					"type":        "string",
					"description": "The topic or goal being reflected upon",
				},
				"insight": map[string]any{
					"type":        "string",
					"description": "The generalized learning or strategy derived",
				},
				"decision": map[string]any{
					"type":        "string",
					"description": "Actionable decision for future similar tasks",
				},
			},
			"required": []string{"topic", "insight", "decision"},
		},
	}
}
