package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/polarisagi/polaris/internal/memory/retrieval"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// 记忆工具集 — 执行函数（R7 拆分自 memory_tools.go）。
// 工具元数据（InputSchema 等）见 memory_tools.go。
// ============================================================================

// ─── 工具执行函数 ──────────────────────────────────────────────────────────────

type memoryWriteArgs struct {
	Name        string `json:"name"`
	EntityType  string `json:"entity_type"`
	Description string `json:"description"`
	ValidUntil  string `json:"valid_until,omitempty"` // duration string: "24h", "7d"
}

func MakeMemoryWriteFn(writer *retrieval.ExclusiveWriter) sandbox.InProcessFn {
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

		if err := writer.UpsertFactExclusive(ctx, &ent, types.TaintNone); err != nil {
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
	AsOf  int64  `json:"as_of,omitempty"`
}

func MakeMemorySearchFn(retriever protocol.HybridRetriever) sandbox.InProcessFn {
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
			AsOf:         args.AsOf,
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

func MakeMemoryAppendFn(writer SemanticMemWriter) sandbox.InProcessFn {
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

func MakeMemoryExpireFn(writer SemanticMemWriter) sandbox.InProcessFn {
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

type memoryReflectArgs struct {
	Topic    string `json:"topic"`
	Insight  string `json:"insight"`
	Decision string `json:"decision"`
}

func MakeMemoryReflectFn(reflection protocol.ReflectionMemory) sandbox.InProcessFn {
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
