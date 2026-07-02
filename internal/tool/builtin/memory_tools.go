package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/polarisagi/polaris/internal/tool"

	"github.com/polarisagi/polaris/internal/observability/metrics"
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
// 注意：工具元数据内联构造，不依赖 tool.LoadBuiltinToolMeta（避免 builtin/<name>/ 目录缺失导致
// 静默跳过——这是 Gemini 首版实现的功能 bug，此处修复）。
func RegisterMemoryTools(
	sbx *sandbox.InProcessSandbox,
	toolReg *tool.InMemoryToolRegistry,
	semanticWriter SemanticMemWriter,
	retriever protocol.HybridRetriever,
	reflection protocol.ReflectionMemory,
) error {
	type entry struct {
		tool types.Tool
		fn   sandbox.InProcessFn
	}

	entries := []entry{
		{tool: memoryWriteTool(), fn: makeMemoryWriteFn(semanticWriter)},
		{tool: memorySearchTool(), fn: makeMemorySearchFn(retriever)},
		{tool: memoryAppendTool(), fn: makeMemoryAppendFn(semanticWriter)},
		{tool: memoryExpireTool(), fn: makeMemoryExpireFn(semanticWriter)},
		{tool: memoryReflectTool(), fn: makeMemoryReflectFn(reflection)},
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

// ─── 工具执行函数 ──────────────────────────────────────────────────────────────

type memoryWriteArgs struct {
	Name        string `json:"name"`
	EntityType  string `json:"entity_type"`
	Description string `json:"description"`
	ValidUntil  string `json:"valid_until,omitempty"` // duration string: "24h", "7d"
}

func makeMemoryWriteFn(writer SemanticMemWriter) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args memoryWriteArgs
		if err := json.Unmarshal(input, &args); err != nil {
			metrics.RecordMemoryToolCall(ctx, "memory_write", false)
			return nil, apperr.Wrap(apperr.CodeInternal, "memory_write: invalid args", err)
		}
		if args.EntityType == "" {
			args.EntityType = "Fact"
		}

		props := map[string]any{
			"description": args.Description,
			"source_type": "user_stated",
			"written_at":  time.Now().Format(time.RFC3339),
		}

		// 解析可选有效期
		if args.ValidUntil != "" {
			if d, err := time.ParseDuration(args.ValidUntil); err == nil && d > 0 {
				props["valid_until"] = time.Now().Add(d).UnixMilli()
			}
		}

		ent := types.Entity{
			ID:          "ent_" + args.Name,
			Name:        args.Name,
			Type:        args.EntityType,
			TaintLevel:  types.TaintMedium,
			SyncVersion: time.Now().UnixNano(),
			Confidence:  1.0,
			Properties:  props,
		}

		if err := writer.UpsertFact(ctx, ent, types.TaintNone); err != nil {
			metrics.RecordMemoryToolCall(ctx, "memory_write", false)
			return nil, apperr.Wrap(apperr.CodeInternal, "memory_write: upsert failed", err)
		}
		metrics.RecordMemoryToolCall(ctx, "memory_write", true)
		b, _ := json.Marshal(map[string]string{
			"status":      "success",
			"entity_type": args.EntityType,
			"name":        args.Name,
		})
		return b, nil
	}
}

type memorySearchArgs struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty"`
	Layer string `json:"layer,omitempty"`
}

func makeMemorySearchFn(retriever protocol.HybridRetriever) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args memorySearchArgs
		if err := json.Unmarshal(input, &args); err != nil {
			metrics.RecordMemoryToolCall(ctx, "memory_search", false)
			return nil, apperr.Wrap(apperr.CodeInternal, "memory_search: invalid args", err)
		}
		if retriever == nil {
			metrics.RecordMemoryToolCall(ctx, "memory_search", false)
			b, _ := json.Marshal(map[string]string{"error": "memory unavailable"})
			return b, nil
		}
		if args.Limit <= 0 || args.Limit > 20 {
			args.Limit = 5
		}

		cfg := types.RetrievalConfig{
			FinalTopK:    args.Limit,
			RerankTopM:   args.Limit * 3,
			BM25Weight:   0.3,
			VectorWeight: 0.5,
			GraphWeight:  0.2,
		}

		scope := types.SearchScope{Type: "memory"}
		if args.Layer != "" {
			scope.Type = args.Layer
		}
		results, err := retriever.Search(ctx, args.Query, scope, cfg)
		if err != nil {
			metrics.RecordMemoryToolCall(ctx, "memory_search", false)
			return nil, apperr.Wrap(apperr.CodeInternal, "memory_search: search failed", err)
		}

		b, err := json.Marshal(results)
		if err != nil {
			metrics.RecordMemoryToolCall(ctx, "memory_search", false)
			return nil, apperr.Wrap(apperr.CodeInternal, "memory_search: encode response", err)
		}
		metrics.RecordMemoryToolCall(ctx, "memory_search", true)
		return b, nil
	}
}

type memoryAppendArgs struct {
	EntityType string `json:"entity_type"`
	Name       string `json:"name"`
	PropKey    string `json:"prop_key"`
	PropValue  string `json:"prop_value"`
}

func makeMemoryAppendFn(writer SemanticMemWriter) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args memoryAppendArgs
		if err := json.Unmarshal(input, &args); err != nil {
			metrics.RecordMemoryToolCall(ctx, "memory_append", false)
			return nil, apperr.Wrap(apperr.CodeInternal, "memory_append: invalid args", err)
		}
		if args.EntityType == "" {
			args.EntityType = "Fact"
		}

		ent, err := writer.GetEntity(ctx, args.EntityType, args.Name)
		if err != nil || ent == nil {
			// 实体不存在时创建新实体
			ent = &types.Entity{
				ID:          "ent_" + args.Name,
				Name:        args.Name,
				Type:        args.EntityType,
				TaintLevel:  types.TaintMedium,
				Confidence:  1.0,
				SyncVersion: time.Now().UnixNano(),
				Properties:  make(map[string]any),
			}
		}

		if ent.Properties == nil {
			ent.Properties = make(map[string]any)
		}
		ent.Properties[args.PropKey] = args.PropValue
		ent.Properties["source_type"] = "user_stated"
		ent.SyncVersion = time.Now().UnixNano()

		if err := writer.UpsertFact(ctx, *ent, types.TaintNone); err != nil {
			metrics.RecordMemoryToolCall(ctx, "memory_append", false)
			return nil, apperr.Wrap(apperr.CodeInternal, "memory_append: upsert failed", err)
		}
		metrics.RecordMemoryToolCall(ctx, "memory_append", true)
		return []byte(`{"status":"success"}`), nil
	}
}

type memoryExpireArgs struct {
	EntityType string `json:"entity_type"`
	Name       string `json:"name"`
	Reason     string `json:"reason"`
}

func makeMemoryExpireFn(writer SemanticMemWriter) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args memoryExpireArgs
		if err := json.Unmarshal(input, &args); err != nil {
			metrics.RecordMemoryToolCall(ctx, "memory_expire", false)
			return nil, apperr.Wrap(apperr.CodeInternal, "memory_expire: invalid args", err)
		}
		if args.EntityType == "" {
			args.EntityType = "Fact"
		}

		// GetEntity: CodeNotFound（实体不存在）和 DB 错误均软失败 → 返回 not_found JSON。
		// 丢弃 err 避免 nilerr：不存在是预期场景，其他错误同样不阻断工具响应。
		ent, _ := writer.GetEntity(ctx, args.EntityType, args.Name)
		if ent == nil {
			metrics.RecordMemoryToolCall(ctx, "memory_expire", false)
			b, _ := json.Marshal(map[string]string{
				"status": "not_found",
				"name":   args.Name,
			})
			return b, nil
		}

		reason := args.Reason
		if reason == "" {
			reason = "agent_expired"
		}

		if err := writer.Archive(ctx, ent.ID, reason); err != nil {
			metrics.RecordMemoryToolCall(ctx, "memory_expire", false)
			return nil, apperr.Wrap(apperr.CodeInternal, "memory_expire: archive failed", err)
		}
		metrics.RecordMemoryToolCall(ctx, "memory_expire", true)
		b, _ := json.Marshal(map[string]string{
			"status": "success",
			"name":   args.Name,
			"reason": reason,
		})
		return b, nil
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

type memoryReflectArgs struct {
	Topic    string `json:"topic"`
	Insight  string `json:"insight"`
	Decision string `json:"decision"`
}

func makeMemoryReflectFn(reflection protocol.ReflectionMemory) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args memoryReflectArgs
		if err := json.Unmarshal(input, &args); err != nil {
			metrics.RecordMemoryToolCall(ctx, "memory_reflect", false)
			return nil, apperr.Wrap(apperr.CodeInternal, "memory_reflect: invalid args", err)
		}

		if reflection != nil {
			entry := types.ReflectionEntry{
				ID:        fmt.Sprintf("ref_%d", time.Now().UnixNano()),
				Strategy:  args.Topic + ": " + args.Insight,
				Decision:  args.Decision,
				CreatedAt: time.Now(),
			}
			err := reflection.AppendReflection(ctx, entry)
			if err != nil {
				metrics.RecordMemoryToolCall(ctx, "memory_reflect", false)
				return nil, apperr.Wrap(apperr.CodeInternal, "memory_reflect: append failed", err)
			}
		} else {
			metrics.RecordMemoryToolCall(ctx, "memory_reflect", false)
			return nil, apperr.New(apperr.CodeInternal, "memory_reflect: reflection memory unavailable")
		}

		metrics.RecordMemoryToolCall(ctx, "memory_reflect", true)
		return []byte(`{"status":"success"}`), nil
	}
}
