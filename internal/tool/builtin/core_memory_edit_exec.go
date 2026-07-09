package builtin

import (
	"context"
	"encoding/json"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type coreMemoryEditArgs struct {
	Operation string `json:"operation"`
	BlockKey  string `json:"block_key"`
	Content   string `json:"content,omitempty"`
}

type coreMemoryContext struct {
	AgentID    string
	SessionID  string
	TaintLevel types.TaintLevel
}

// extractCoreMemoryContext 从类型化 ctx key（protocol.Ctx*Key，禁魔法字符串）提取
// 调用身份与污点。SessionID 复用 CtxTaskIDKey——生产路径 dag/executor.Execute 以
// SessionID 作为 taskID 注入（见 agent/context/memory_context.go 同源注释）。
// 键缺失时回退 default/TaintNone（仅测试或非 DAG 直调场景）。
func extractCoreMemoryContext(ctx context.Context) coreMemoryContext {
	c := coreMemoryContext{
		AgentID:    "default",
		SessionID:  "default",
		TaintLevel: types.TaintNone,
	}
	if v := ctx.Value(protocol.CtxAgentIDKey{}); v != nil {
		if s, ok := v.(string); ok && s != "" {
			c.AgentID = s
		}
	}
	if v := ctx.Value(protocol.CtxTaskIDKey{}); v != nil {
		if s, ok := v.(string); ok && s != "" {
			c.SessionID = s
		}
	}
	if v := ctx.Value(protocol.CtxTaintLevelKey{}); v != nil {
		if t, ok := v.(types.TaintLevel); ok {
			c.TaintLevel = t
		}
	}
	return c
}

//nolint:gocyclo // simplified but still slightly complex
func MakeCoreMemoryEditFn(coreMemory protocol.CoreMemory) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		if coreMemory == nil {
			return nil, apperr.New(apperr.CodeInternal, "core_memory_edit: core memory unavailable")
		}

		var args coreMemoryEditArgs
		if err := json.Unmarshal(input, &args); err != nil {
			metrics.RecordMemoryToolCall(ctx, "core_memory_edit", false)
			return nil, apperr.Wrap(apperr.CodeInternal, "core_memory_edit: invalid args", err)
		}

		if args.BlockKey == "" {
			return nil, apperr.New(apperr.CodeInternal, "core_memory_edit: block_key is required")
		}

		c := extractCoreMemoryContext(ctx)

		if args.Operation == "delete" {
			if err := coreMemory.Delete(ctx, c.AgentID, c.SessionID, args.BlockKey); err != nil {
				metrics.RecordMemoryToolCall(ctx, "core_memory_edit", false)
				return nil, apperr.Wrap(apperr.CodeInternal, "core_memory_edit: delete failed", err)
			}
			metrics.RecordMemoryToolCall(ctx, "core_memory_edit", true)
			return []byte(`{"status":"success","operation":"delete"}`), nil
		}

		return executeSetOrAppend(ctx, coreMemory, args, c)
	}
}

func executeSetOrAppend(ctx context.Context, coreMemory protocol.CoreMemory, args coreMemoryEditArgs, c coreMemoryContext) ([]byte, error) {
	existing, err := coreMemory.Get(ctx, c.AgentID, c.SessionID, args.BlockKey)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "core_memory_edit: get failed", err)
	}

	newContent := args.Content
	taintLevel := c.TaintLevel

	if args.Operation == "append" && existing != nil {
		newContent = existing.Content + "\n" + args.Content
		if existing.TaintLevel > taintLevel {
			taintLevel = existing.TaintLevel
		}
	} else if args.Operation != "set" && args.Operation != "append" {
		return nil, apperr.New(apperr.CodeInternal, "core_memory_edit: invalid operation")
	}

	thresholds := config.DefaultThresholds()
	maxBlockSize := thresholds.M5Memory.CoreMemoryBlockMaxKB * 1024
	if len(newContent) > maxBlockSize {
		metrics.RecordMemoryToolCall(ctx, "core_memory_edit", false)
		return nil, apperr.New(apperr.CodeInternal, "core_memory_edit: block size exceeds limit")
	}

	if err := checkTotalSize(ctx, coreMemory, args.BlockKey, newContent, c, thresholds.M5Memory.CoreMemoryTotalMaxKB*1024); err != nil {
		metrics.RecordMemoryToolCall(ctx, "core_memory_edit", false)
		return nil, err
	}

	if err := coreMemory.Set(ctx, c.AgentID, c.SessionID, args.BlockKey, newContent, taintLevel); err != nil {
		metrics.RecordMemoryToolCall(ctx, "core_memory_edit", false)
		return nil, apperr.Wrap(apperr.CodeInternal, "core_memory_edit: set failed", err)
	}

	metrics.RecordMemoryToolCall(ctx, "core_memory_edit", true)
	return []byte(`{"status":"success"}`), nil
}

func checkTotalSize(ctx context.Context, coreMemory protocol.CoreMemory, skipKey string, newContent string, c coreMemoryContext, maxSize int) error {
	allBlocks, err := coreMemory.List(ctx, c.AgentID, c.SessionID)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "core_memory_edit: list failed", err)
	}

	totalSize := len(newContent)
	for _, b := range allBlocks {
		if b.BlockKey != skipKey {
			totalSize += len(b.Content)
		}
	}

	if totalSize > maxSize {
		return apperr.New(apperr.CodeInternal, "core_memory_edit: total core memory size exceeds limit")
	}
	return nil
}
