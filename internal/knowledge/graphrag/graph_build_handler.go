package graphrag

import (
	"context"
	"encoding/json"

	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// GraphBuildRunner 定义异步执行知识图谱构建的接口
type GraphBuildRunner interface {
	Run(ctx context.Context, docID string) error
}

// GraphBuildOutboxHandler 监听 rag_doc_ingested 事件，异步触发知识图谱构建。
// 注册方式：outboxWorker.RegisterHandler(EventTypeRAGDocIngested, handler.Handle)
type GraphBuildOutboxHandler struct {
	pipeline GraphBuildRunner
}

// EventTypeRAGDocIngested Outbox 事件类型（与 PipelineImpl 写入事件名对齐）。
const EventTypeRAGDocIngested = "rag_doc_ingested"

func NewGraphBuildOutboxHandler(pipeline GraphBuildRunner) *GraphBuildOutboxHandler {
	return &GraphBuildOutboxHandler{pipeline: pipeline}
}

// Handle 实现 store.OutboxHandler 接口。
// payload: JSON {"doc_id": "<string>"}
func (h *GraphBuildOutboxHandler) Handle(ctx context.Context, record *store.OutboxRecord) error {
	var payload struct {
		DocID string `json:"doc_id"`
	}
	if err := json.Unmarshal(record.Payload, &payload); err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "GraphBuildOutboxHandler: invalid payload", err)
	}
	if payload.DocID == "" {
		return nil
	}
	return h.pipeline.Run(ctx, payload.DocID)
}
