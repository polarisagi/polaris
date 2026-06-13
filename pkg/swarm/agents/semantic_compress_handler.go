package agents

import (
	"context"
	"encoding/json"
	"log/slog"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/substrate"
)

// SemanticCompressHandler 负责在收到 semantic_compress 任务时，执行知识图谱或语义记忆的进一步压缩。
// 触发自 compressor.go 等内部逻辑通过 OutboxWorker 推送的任务。
type SemanticCompressHandler struct {
	semantic  protocol.SemanticMemory
	llmInfer  LLMInferFunc
	vfsLoader func(vfsID string) ([]byte, error)
	vfsWriter func(vfsID string, data []byte) error
}

// NewSemanticCompressHandler 创建语义压缩处理器
func NewSemanticCompressHandler(semantic protocol.SemanticMemory, llmInfer LLMInferFunc,
	vfsLoader func(vfsID string) ([]byte, error),
	vfsWriter func(vfsID string, data []byte) error) *SemanticCompressHandler {
	return &SemanticCompressHandler{
		semantic:  semantic,
		llmInfer:  llmInfer,
		vfsLoader: vfsLoader,
		vfsWriter: vfsWriter,
	}
}

// Handle 实现 substrate.OutboxHandler 接口
func (h *SemanticCompressHandler) Handle(ctx context.Context, record *substrate.OutboxRecord) error {
	if h.semantic == nil {
		slog.Warn("semantic_compress: semantic memory is nil, skip")
		return nil
	}

	var req struct {
		VfsID string `json:"vfs_id"`
	}
	if err := json.Unmarshal(record.Payload, &req); err != nil {
		return perrors.Wrap(perrors.CodeInvalidInput, "semantic_compress: parse payload failed", err)
	}

	slog.Info("semantic_compress: processing", "vfs_id", req.VfsID)

	if h.vfsLoader != nil && h.llmInfer != nil && h.vfsWriter != nil {
		data, err := h.vfsLoader(req.VfsID)
		if err != nil {
			return perrors.Wrap(perrors.CodeInternal, "failed to load vfs data", err)
		}

		prompt := "Condense and summarize the following payload:\n" + string(data)
		summary, err := h.llmInfer(ctx, prompt)
		if err != nil {
			return perrors.Wrap(perrors.CodeInternal, "llm compress failed", err)
		}

		err = h.vfsWriter(req.VfsID, []byte(summary))
		if err != nil {
			return perrors.Wrap(perrors.CodeInternal, "failed to write vfs data", err)
		}
	}

	return nil
}
