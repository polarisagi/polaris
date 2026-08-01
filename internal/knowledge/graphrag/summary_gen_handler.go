package graphrag

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/prompt/templates"

	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/internal/llm/safecall"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/pkg/apperr"
)

const EventTypeRAGDocSummaryNeeded = "rag_doc_summary_needed"

// ChunkTaintSealer 是 graphrag 对跨模块持久化边界 HMAC 签名能力（inv_M11_02）
// 的消费端接口（S-05，HE-3：接口在调用方定义）。graphrag 不得反向依赖
// internal/knowledge 根包（会与 knowledge→graphrag 的既有依赖方向成环），
// 故由 knowledge 包实现本接口（knowledge.ChunkTaintSealerAdapter）并注入。
type ChunkTaintSealer interface {
	// SealChunkTaint 为一条 rag_chunks 记录计算 HMAC-SHA256 边界签名。
	SealChunkTaint(id, content string, level int, source string) string
}

// SummaryGenOutboxHandler 监听 rag_doc_summary_needed 事件，异步触发 LLM 摘要生成。
type SummaryGenOutboxHandler struct {
	db       protocol.SQLQuerier
	provider protocol.Provider
	// sealer 为 nil 时 taint_hmac 写空串（与 knowledge.sealChunkTaint 的既有
	// nil-safe 降级语义对称）；生产路径（boot_knowledge.go）恒定注入非 nil 实例。
	sealer ChunkTaintSealer
}

func NewSummaryGenOutboxHandler(db protocol.SQLQuerier, provider protocol.Provider, sealer ChunkTaintSealer) *SummaryGenOutboxHandler {
	return &SummaryGenOutboxHandler{db: db, provider: provider, sealer: sealer}
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

	// S-05：摘要必须继承源文档的最高 taint_level（only-up 语义），不得写死为 0。
	// 读取失败 fail-closed 取 TaintHigh，禁止取 0（与 ingester.go 的
	// sealChunkTaint 调用点保持同一 canonical 写法）。COALESCE 兜底 TaintMedium
	// 仅覆盖"查得到行但全为 NULL"这一理论边界（正常情况下 len(contents)>0
	// 已保证至少一行 parent chunk 存在）。
	var srcTaint int
	if err := h.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(taint_level), ?) FROM rag_chunks WHERE doc_id = ? AND chunk_type != 'doc_summary'`,
		int(types.TaintMedium), docID).Scan(&srcTaint); err != nil {
		slog.WarnContext(ctx, "summary_gen: read source taint failed, fail-closed to TaintHigh", "doc_id", docID, "err", err)
		srcTaint = int(types.TaintHigh)
	}

	// A-12：System Prompt 与用户数据严格分离，避免拼接注入风险。
	// System 消息固定角色定义；User 消息携带待摘要的原始文档内容（可能含外部数据）。
	sysPrompt, err := templates.Render("graphrag_doc_summary.tmpl", nil)
	if err != nil {
		slog.WarnContext(ctx, "summary_gen: render system prompt failed, fallback to default", "error", err)
		sysPrompt = "你是文档摘要助手。请根据用户提供的文档片段，生成一个简洁的文档级摘要，不超过200个token，只输出摘要内容。"
	}

	inferMsgs := []types.Message{
		{
			Role:    "system",
			Content: sysPrompt,
		},
		{
			Role:    "user",
			Content: strings.Join(contents, "\n\n"),
		},
	}
	// P-1：每次 LLM 调用自持超时（90s），不信任 Outbox 调度上下文一定带 deadline（A-05）。
	inferCtx, inferCancel := context.WithTimeout(ctx, 90*time.Second)
	defer inferCancel()
	resp, err := safecall.Infer(inferCtx, h.provider, inferMsgs)
	if err != nil || resp == nil {
		// LLM 调用失败多为瞬时（限流/超时/厂商故障），可重试。
		return apperr.Wrap(apperr.CodeInternal, "summary_gen: llm infer", err)
	}
	if resp.Content == "" {
		// 空响应视为"本轮无有效摘要"，非错误，不重试（避免空响应无限重试打满 outbox）。
		return nil
	}

	summaryID := fmt.Sprintf("doc_summary_%s", docID)
	// S-05：taint_level 继承 srcTaint（而非硬编码 0），并补上 taint_hmac 签名
	// （inv_M11_02 持久化边界密码学验证），与 ingester.go 的 canonical 写法对齐。
	var hmacHex string
	if h.sealer != nil {
		hmacHex = h.sealer.SealChunkTaint(summaryID, resp.Content, srcTaint, "outbox_summary")
	}
	if _, err := h.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO rag_chunks
			(id, doc_id, content, taint_level, taint_source, taint_hmac, chunk_type, chunk_index)
		 VALUES (?,?,?,?,?,?,?,?)`,
		summaryID, docID, resp.Content, srcTaint, "outbox_summary", hmacHex, "doc_summary", -1); err != nil {
		// 持久化失败可重试（INSERT OR REPLACE 幂等，重试安全）；仍记录日志便于排障。
		slog.WarnContext(ctx, "summary_gen: db update failed", "error", err)
		return apperr.Wrap(apperr.CodeInternal, "summary_gen: persist summary", err)
	}
	return nil
}
