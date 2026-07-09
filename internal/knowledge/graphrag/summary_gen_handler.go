package graphrag

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

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
		// 查询失败通常是瞬时 DB 问题（连接抖动等），可重试——返回 err 让 Outbox 重投递，
		// 不再吞掉（此前 nilerr 会让该 handler 的重试语义形同虚设）。
		return apperr.Wrap(apperr.CodeInternal, "summary_gen: query parent chunks", err)
	}

	var contents []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err == nil {
			contents = append(contents, c)
		}
	}
	rowsErr := rows.Err()
	// [R1.16] 显式提前关闭 reader 连接：LLM 调用（provider.Infer，量级数秒~15s）
	// 不得在持有 reader 池连接期间进行，否则会占住 4 连接 reader 池之一直到推理结束。
	rows.Close()
	if rowsErr != nil {
		return apperr.Wrap(apperr.CodeInternal, "summary_gen: iterate parent chunks", rowsErr)
	}
	if len(contents) == 0 {
		// 无可摘要片段：非错误状态（文档尚未产生 parent chunk），不重试。
		return nil
	}

	// A-12：System Prompt 与用户数据严格分离，避免拼接注入风险。
	// System 消息固定角色定义；User 消息携带待摘要的原始文档内容（可能含外部数据）。
	inferMsgs := []types.Message{
		{
			Role:    "system",
			Content: "你是文档摘要助手。请根据用户提供的文档片段，生成一个简洁的文档级摘要，不超过200个token，只输出摘要内容。",
		},
		{
			Role:    "user",
			Content: strings.Join(contents, "\n\n"),
		},
	}
	// P-1：每次 LLM 调用自持超时（90s），不信任 Outbox 调度上下文一定带 deadline（A-05）。
	inferCtx, inferCancel := context.WithTimeout(ctx, 90*time.Second)
	defer inferCancel()
	//nolint:bare-infer // 历史代码暂留，后续重构替换
	resp, err := h.provider.Infer(inferCtx, inferMsgs)
	if err != nil {
		// LLM 调用失败多为瞬时（限流/超时/厂商故障），可重试。
		return apperr.Wrap(apperr.CodeInternal, "summary_gen: llm infer", err)
	}
	if resp == nil || resp.Content == "" {
		// 空响应视为"本轮无有效摘要"，非错误，不重试（避免空响应无限重试打满 outbox）。
		return nil
	}

	summaryID := fmt.Sprintf("doc_summary_%s", docID)
	if _, err := h.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO rag_chunks
			(id, doc_id, content, taint_level, taint_source, chunk_type, chunk_index)
		 VALUES (?,?,?,?,?,?,?)`,
		summaryID, docID, resp.Content, 0, "outbox_summary", "doc_summary", -1); err != nil {
		// 持久化失败可重试（INSERT OR REPLACE 幂等，重试安全）；仍记录日志便于排障。
		slog.WarnContext(ctx, "summary_gen: db update failed", "error", err)
		return apperr.Wrap(apperr.CodeInternal, "summary_gen: persist summary", err)
	}
	return nil
}
