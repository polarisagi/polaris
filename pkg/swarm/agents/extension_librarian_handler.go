package agents

import (
	"context"
	"encoding/json"
	"log/slog"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/pkg/substrate"
)

// ExtensionLibrarianHandler 负责在收到 extension_librarian 任务时处理插件或扩展的后置工作。
// 触发自 extension_manager.go 等内部逻辑通过 OutboxWorker 推送的任务。
type ExtensionLibrarianHandler struct {
	llmInfer LLMInferFunc
	surreal  SurrealWriterInterface
}

// NewExtensionLibrarianHandler 创建扩展图书馆员处理器
func NewExtensionLibrarianHandler(llmInfer LLMInferFunc, surreal SurrealWriterInterface) *ExtensionLibrarianHandler {
	return &ExtensionLibrarianHandler{
		llmInfer: llmInfer,
		surreal:  surreal,
	}
}

// Handle 实现 substrate.OutboxHandler 接口
func (h *ExtensionLibrarianHandler) Handle(ctx context.Context, record *substrate.OutboxRecord) error {
	var req struct {
		ExtensionID string `json:"extension_id"`
	}
	if err := json.Unmarshal(record.Payload, &req); err != nil {
		return perrors.Wrap(perrors.CodeInvalidInput, "extension_librarian: parse payload failed", err)
	}

	slog.Info("extension_librarian: processing extension index", "extension_id", req.ExtensionID)
	// 实际业务逻辑：更新系统知识图谱，索引新插件的 API 和能力，使其对 Planner 等智能体可见

	if h.llmInfer != nil && h.surreal != nil {
		prompt := "Describe the capability of the extension " + req.ExtensionID + " for indexing"
		desc, err := h.llmInfer(ctx, prompt)
		if err == nil && desc != "" {
			_ = h.surreal.FTSIndex("extension:"+req.ExtensionID, desc)
			_ = h.surreal.VecUpsert("extension:"+req.ExtensionID, []float32{0.1, 0.2}) // dummy embedding
			_ = h.surreal.GraphRelate("system", "has_extension", "extension:"+req.ExtensionID, 1.0)
		}
	}

	return nil
}
