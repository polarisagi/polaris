package graphrag

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/pkg/apperr"
)

const EventTypeRAGDocSummaryNeeded = "rag_doc_summary_needed"

// SummaryGenOutboxHandler 监听 rag_doc_summary_needed 事件，异步触发 LLM 摘要生成。
type SummaryGenOutboxHandler struct {
	db       protocol.SQLQuerier
	provider protocol.Provider
}

func NewSummaryGenOutboxHandler(db protocol.SQLQuerier, provider protocol.Provider) *SummaryGenOutboxHandler {
	return &SummaryGenOutboxHandler{db: db, provider: provider}
}

func (h *SummaryGenOutboxHandler) Handle(ctx context.Context, record *store.OutboxRecord) error {
	var payload struct {
		DocID string `json:"doc_id"`
	}
	if err := json.Unmarshal(record.Payload, &payload); err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "SummaryGenOutboxHandler: invalid payload", err)
	}
	if payload.DocID == "" || h.provider == nil {
		return nil
	}
	return h.generateSummary(ctx, payload.DocID)
}

func (h *SummaryGenOutboxHandler) generateSummary(ctx context.Context, docID string) error {
	rows, err := h.db.QueryContext(ctx,
		"SELECT content FROM rag_chunks WHERE doc_id = ? AND chunk_type = 'parent' AND deleted_at IS NULL ORDER BY chunk_index ASC LIMIT 3",
		docID)
	if err != nil {
		return nil //nolint:nilerr
	}
	defer rows.Close()

	var contents []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err == nil {
			contents = append(contents, c)
		}
	}
	if len(contents) == 0 {
		return nil
	}

	prompt := fmt.Sprintf("请根据以下文档片段，生成一个不超过200 tokens的文档级摘要：\n%s",
		strings.Join(contents, "\n\n"))
	resp, err := h.provider.Infer(ctx, []types.Message{{Role: "user", Content: prompt}})
	if err != nil || resp == nil || resp.Content == "" {
		return nil //nolint:nilerr
	}

	summaryID := fmt.Sprintf("doc_summary_%s", docID)
	if _, err := h.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO rag_chunks
			(id, doc_id, content, taint_level, taint_source, chunk_type, chunk_index)
		 VALUES (?,?,?,?,?,?,?)`,
		summaryID, docID, resp.Content, 0, "outbox_summary", "doc_summary", -1); err != nil {
		slog.WarnContext(ctx, "summary_gen: db update failed", "error", err)
	}
	return nil
}
