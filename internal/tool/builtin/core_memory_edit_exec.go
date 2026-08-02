package builtin

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type coreMemoryEditArgs struct {
	Operation   string `json:"operation"`
	BlockKey    string `json:"block_key"`
	Content     string `json:"content,omitempty"`
	OldStr      string `json:"old_str,omitempty"`
	NewStr      string `json:"new_str,omitempty"`
	ReplaceAll  bool   `json:"replace_all,omitempty"`
	Description string `json:"description,omitempty"`
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

// coreMemoryBlockSummary 是 "list" 操作的响应形状——刻意不含 content（ADR-0082），
// 避免一次 list 把整个核心记忆区重复灌入上下文放大 token 消耗。
type coreMemoryBlockSummary struct {
	BlockKey    string `json:"block_key"`
	Description string `json:"description"`
	SizeBytes   int    `json:"size_bytes"`
	MaxBytes    int    `json:"max_bytes"`
	ReadOnly    bool   `json:"read_only"`
	UpdatedAt   string `json:"updated_at"`
}

//nolint:gocyclo // operation 分发本质是扁平 switch，拆分反而降低可读性
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

		c := extractCoreMemoryContext(ctx)

		switch args.Operation {
		case "list":
			return executeList(ctx, coreMemory, c)
		case "get":
			return executeGet(ctx, coreMemory, args, c)
		case "delete":
			return executeDelete(ctx, coreMemory, args, c)
		case "replace":
			return executeReplace(ctx, coreMemory, args, c)
		case "describe":
			return executeDescribe(ctx, coreMemory, args, c)
		case "set", "append":
			if args.BlockKey == "" {
				return nil, apperr.New(apperr.CodeInvalidInput, "core_memory_edit: block_key is required")
			}
			return executeSetOrAppend(ctx, coreMemory, args, c)
		default:
			metrics.RecordMemoryToolCall(ctx, "core_memory_edit", false)
			return nil, apperr.New(apperr.CodeInvalidInput, "core_memory_edit: invalid operation")
		}
	}
}

func executeList(ctx context.Context, coreMemory protocol.CoreMemory, c coreMemoryContext) ([]byte, error) {
	blocks, err := coreMemory.List(ctx, c.AgentID, c.SessionID)
	if err != nil {
		metrics.RecordMemoryToolCall(ctx, "core_memory_edit", false)
		return nil, apperr.Wrap(apperr.CodeInternal, "core_memory_edit: list failed", err)
	}
	summaries := make([]coreMemoryBlockSummary, 0, len(blocks))
	for _, b := range blocks {
		summaries = append(summaries, coreMemoryBlockSummary{
			BlockKey:    b.BlockKey,
			Description: b.Description,
			SizeBytes:   len(b.Content),
			MaxBytes:    b.MaxBytes,
			ReadOnly:    b.ReadOnly,
			UpdatedAt:   b.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
		})
	}
	out, err := json.Marshal(map[string]any{"status": "success", "blocks": summaries})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "core_memory_edit: list marshal failed", err)
	}
	metrics.RecordMemoryToolCall(ctx, "core_memory_edit", true)
	return out, nil
}

func executeGet(ctx context.Context, coreMemory protocol.CoreMemory, args coreMemoryEditArgs, c coreMemoryContext) ([]byte, error) {
	if args.BlockKey == "" {
		return nil, apperr.New(apperr.CodeInvalidInput, "core_memory_edit: block_key is required")
	}
	block, err := coreMemory.Get(ctx, c.AgentID, c.SessionID, args.BlockKey)
	if err != nil {
		metrics.RecordMemoryToolCall(ctx, "core_memory_edit", false)
		return nil, apperr.Wrap(apperr.CodeInternal, "core_memory_edit: get failed", err)
	}
	if block == nil {
		metrics.RecordMemoryToolCall(ctx, "core_memory_edit", false)
		return nil, apperr.New(apperr.CodeNotFound, "core_memory_edit: block not found")
	}
	out, err := json.Marshal(map[string]any{
		"status": "success", "block_key": block.BlockKey, "content": block.Content,
		"description": block.Description, "read_only": block.ReadOnly,
		"size_bytes": len(block.Content), "max_bytes": block.MaxBytes,
	})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "core_memory_edit: get marshal failed", err)
	}
	metrics.RecordMemoryToolCall(ctx, "core_memory_edit", true)
	return out, nil
}

func executeDelete(ctx context.Context, coreMemory protocol.CoreMemory, args coreMemoryEditArgs, c coreMemoryContext) ([]byte, error) {
	if args.BlockKey == "" {
		return nil, apperr.New(apperr.CodeInvalidInput, "core_memory_edit: block_key is required")
	}
	existing, err := coreMemory.Get(ctx, c.AgentID, c.SessionID, args.BlockKey)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "core_memory_edit: get failed", err)
	}
	if existing != nil && existing.ReadOnly {
		metrics.RecordMemoryToolCall(ctx, "core_memory_edit", false)
		return nil, apperr.New(apperr.CodeForbidden, "core_memory_edit: block is read-only")
	}
	if err := coreMemory.Delete(ctx, c.AgentID, c.SessionID, args.BlockKey); err != nil {
		metrics.RecordMemoryToolCall(ctx, "core_memory_edit", false)
		return nil, apperr.Wrap(apperr.CodeInternal, "core_memory_edit: delete failed", err)
	}
	metrics.RecordMemoryToolCall(ctx, "core_memory_edit", true)
	return []byte(`{"status":"success","operation":"delete"}`), nil
}

func executeReplace(ctx context.Context, coreMemory protocol.CoreMemory, args coreMemoryEditArgs, c coreMemoryContext) ([]byte, error) {
	if args.BlockKey == "" || args.OldStr == "" {
		return nil, apperr.New(apperr.CodeInvalidInput, "core_memory_edit: block_key and old_str are required")
	}
	occurrences, err := coreMemory.Replace(ctx, c.AgentID, c.SessionID, args.BlockKey, args.OldStr, args.NewStr, args.ReplaceAll, c.TaintLevel)
	if err != nil {
		metrics.RecordMemoryToolCall(ctx, "core_memory_edit", false)
		return nil, err //nolint:wrapcheck // Replace 已按 CodeNotFound/CodeForbidden/CodeInvalidInput 分类，重新包装会丢失分类信息
	}
	metrics.RecordMemoryToolCall(ctx, "core_memory_edit", true)
	out, _ := json.Marshal(map[string]any{"status": "success", "operation": "replace", "occurrences": occurrences})
	return out, nil
}

func executeDescribe(ctx context.Context, coreMemory protocol.CoreMemory, args coreMemoryEditArgs, c coreMemoryContext) ([]byte, error) {
	if args.BlockKey == "" {
		return nil, apperr.New(apperr.CodeInvalidInput, "core_memory_edit: block_key is required")
	}
	if err := coreMemory.Describe(ctx, c.AgentID, c.SessionID, args.BlockKey, args.Description); err != nil {
		metrics.RecordMemoryToolCall(ctx, "core_memory_edit", false)
		return nil, err //nolint:wrapcheck // Describe 已按 CodeNotFound 分类，重新包装会丢失分类信息
	}
	metrics.RecordMemoryToolCall(ctx, "core_memory_edit", true)
	return []byte(`{"status":"success","operation":"describe"}`), nil
}

// checkBlockCountLimit 新建块前的块数上限校验（ADR-0082），避免无界增长。
// existing 非 nil（更新已有块）时不受此限。
func checkBlockCountLimit(ctx context.Context, coreMemory protocol.CoreMemory, c coreMemoryContext, maxBlocks int) error {
	allBlocks, err := coreMemory.List(ctx, c.AgentID, c.SessionID)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "core_memory_edit: list failed", err)
	}
	if len(allBlocks) >= maxBlocks {
		return apperr.New(apperr.CodeResourceExhausted, fmt.Sprintf(
			"core_memory_edit: block count limit reached (%d/%d), delete an unused block first",
			len(allBlocks), maxBlocks))
	}
	return nil
}

// buildSetOrAppendContent 计算 set/append 的最终内容、污点（只升不降）与生效的
// 单块字节上限（已存在块沿用创建时固化的 MaxBytes，新块用当前全局默认值）。
func buildSetOrAppendContent(args coreMemoryEditArgs, c coreMemoryContext, existing *types.CoreMemoryBlock, defaultMaxBytes int) (content string, taintLevel types.TaintLevel, maxBytes int, err error) {
	content = args.Content
	taintLevel = c.TaintLevel
	maxBytes = defaultMaxBytes

	switch {
	case args.Operation == "append" && existing != nil:
		content = existing.Content + "\n" + args.Content
		if existing.TaintLevel > taintLevel {
			taintLevel = existing.TaintLevel
		}
	case args.Operation == "set" || args.Operation == "append":
		// 新建块的 set/append，沿用上面已初始化的 content/taintLevel。
	default:
		return "", types.TaintNone, 0, apperr.New(apperr.CodeInvalidInput, "core_memory_edit: invalid operation")
	}
	if existing != nil && existing.MaxBytes > 0 {
		maxBytes = existing.MaxBytes // 已存在块沿用其创建时固化的上限（ADR-0082）
	}
	return content, taintLevel, maxBytes, nil
}

func executeSetOrAppend(ctx context.Context, coreMemory protocol.CoreMemory, args coreMemoryEditArgs, c coreMemoryContext) ([]byte, error) {
	existing, err := coreMemory.Get(ctx, c.AgentID, c.SessionID, args.BlockKey)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "core_memory_edit: get failed", err)
	}
	if existing != nil && existing.ReadOnly {
		metrics.RecordMemoryToolCall(ctx, "core_memory_edit", false)
		return nil, apperr.New(apperr.CodeForbidden, "core_memory_edit: block is read-only")
	}

	thresholds := config.DefaultThresholds()

	if existing == nil {
		if err := checkBlockCountLimit(ctx, coreMemory, c, thresholds.M5Memory.CoreMemoryMaxBlocks); err != nil {
			metrics.RecordMemoryToolCall(ctx, "core_memory_edit", false)
			return nil, err
		}
	}

	newContent, taintLevel, maxBlockBytes, err := buildSetOrAppendContent(
		args, c, existing, thresholds.M5Memory.CoreMemoryBlockMaxKB*1024)
	if err != nil {
		return nil, err
	}

	if len(newContent) > maxBlockBytes {
		metrics.RecordMemoryToolCall(ctx, "core_memory_edit", false)
		return nil, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf(
			"core_memory_edit: block size %d bytes exceeds limit %d bytes, summarize or split the content",
			len(newContent), maxBlockBytes))
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
		return apperr.New(apperr.CodeResourceExhausted, fmt.Sprintf(
			"core_memory_edit: total core memory size %d bytes exceeds limit %d bytes", totalSize, maxSize))
	}
	return nil
}
