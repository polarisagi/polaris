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
	semantic protocol.SemanticMemory
}

// NewSemanticCompressHandler 创建语义压缩处理器
func NewSemanticCompressHandler(semantic protocol.SemanticMemory) *SemanticCompressHandler {
	return &SemanticCompressHandler{semantic: semantic}
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
	// 实际业务逻辑：调用 SemanticMemory 进行进一步的压缩或降维合并
	// h.semantic.Compress(ctx, req.VfsID)
	return nil
}
