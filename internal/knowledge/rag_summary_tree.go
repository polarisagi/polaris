package knowledge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/llm/safecall"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// leafChunk 为段落级原始内容的最小查询结果单元。
type leafChunk struct{ id, content string }

// buildSummaryTree 为 outboxWriter 未注入场景生成 L1/L2/L3 三级摘要树
// （段落级 → 章节级 → 文档级），从 rag_impl.go 抽出（R7 拆分，行为与拆分前
// 逐行等价，仅用于满足文件行数上限；R1.16 修复见 fetchLeafChunks 内
// rows.Close() 注释）。查询与生成逻辑进一步拆为 fetchLeafChunks /
// generateSummaryLevels 两个子函数，收敛圈复杂度（R7 gocyclo 上限）。
func (p *DefaultIngestionPipeline) buildSummaryTree(ctx context.Context, docNode *DocNode, db protocol.SQLQuerier) {
	if p.provider == nil {
		return
	}

	leaves, ok := p.fetchLeafChunks(ctx, db, docNode.ID)
	if !ok {
		return
	}

	// 查询源 chunks 最高污点级别（taint 只升不降）
	var srcTaint int
	_ = db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(taint_level), 0) FROM rag_chunks WHERE doc_id = ? AND deleted_at IS NULL`,
		docNode.ID).Scan(&srcTaint)

	p.generateSummaryLevels(ctx, db, docNode, leaves, srcTaint)
}

// fetchLeafChunks 查询文档的所有 leaf chunks（段落级原始内容）。
// ok=false 表示查询失败或无数据，调用方应放弃后续摘要生成。
func (p *DefaultIngestionPipeline) fetchLeafChunks(ctx context.Context, db protocol.SQLQuerier, docID string) ([]leafChunk, bool) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, content FROM rag_chunks WHERE doc_id = ? AND chunk_type = 'leaf' AND deleted_at IS NULL ORDER BY chunk_index ASC`,
		docID)
	if err != nil {
		return nil, false
	}

	var leaves []leafChunk
	for rows.Next() {
		var lc leafChunk
		if err := rows.Scan(&lc.id, &lc.content); err == nil {
			leaves = append(leaves, lc)
		}
	}
	rowsErr := rows.Err()
	// [R1.16] 显式提前关闭 reader 连接，而非 defer 到函数末尾：调用方后续的
	// L1/L2/L3 三级摘要循环会对每个 leaf/分组/全文各发起一次 provider.Infer
	// （15s 超时 × N 次），大文档摄入可能占住 4 连接 reader 池之一长达数分钟。
	rows.Close()
	if rowsErr != nil {
		slog.WarnContext(ctx, "rag_impl: iterate leaf chunks failed", "error", rowsErr)
		return nil, false
	}
	if len(leaves) == 0 {
		return nil, false
	}
	return leaves, true
}

// summarizeText 对 prompt 发起一次 LLM 推理并返回裁剪后的文本，失败时返回空串
// （调用方按空串跳过写入，不视为致命错误）。
func (p *DefaultIngestionPipeline) summarizeText(ctx context.Context, prompt string, maxTokens int) string {
	sCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := safecall.Infer(sCtx, p.provider, []types.Message{{Role: "user", Content: prompt}},
		types.WithMaxTokens(maxTokens))
	if err != nil || resp == nil {
		return ""
	}
	return strings.TrimSpace(resp.Content)
}

// insertSummaryChunk 将一条摘要写回 rag_chunks；content 为空时跳过（无摘要可写）。
func (p *DefaultIngestionPipeline) insertSummaryChunk(ctx context.Context, db protocol.SQLQuerier, docID string, srcTaint int, now, id, content, chunkType string, idx int) {
	if content == "" {
		return
	}
	if _, err := db.ExecContext(ctx,
		`INSERT OR REPLACE INTO rag_chunks (id, doc_id, content, taint_level, taint_source, chunk_type, chunk_index, created_at)
         VALUES (?,?,?,?,?,?,?,?)`,
		id, docID, content, srcTaint, "auto_summary", chunkType, idx, now); err != nil {
		slog.WarnContext(ctx, "rag_impl: db write failed", "error", err)
	}
}

// generateSummaryLevels 依次生成 L1（段落）/L2（章节）/L3（文档）三级摘要并写回。
func (p *DefaultIngestionPipeline) generateSummaryLevels(ctx context.Context, db protocol.SQLQuerier, docNode *DocNode, leaves []leafChunk, srcTaint int) {
	now := time.Now().UTC().Format(time.RFC3339)

	// L1 段落级摘要（每个 leaf chunk → ≤30 tokens 关键句）
	for i, leaf := range leaves {
		prompt := fmt.Sprintf("用一句话（≤30个token）总结以下段落的核心信息：\n%s", leaf.content)
		summary := p.summarizeText(ctx, prompt, 60)
		p.insertSummaryChunk(ctx, db, docNode.ID, srcTaint, now, fmt.Sprintf("para_summary_%s_%d", docNode.ID, i), summary, "para_summary", i)
	}

	// L2 章节级摘要（每 5 个 leaf 为一组 → ~100 tokens）
	groupSize := 5
	for gi := 0; gi < len(leaves); gi += groupSize {
		end := min(gi+groupSize, len(leaves))
		group := leaves[gi:end]
		combined := make([]string, len(group))
		for k, lc := range group {
			combined[k] = lc.content
		}
		prompt := fmt.Sprintf("将以下段落内容总结为约100个token的章节摘要：\n%s",
			strings.Join(combined, "\n\n"))
		summary := p.summarizeText(ctx, prompt, 150)
		p.insertSummaryChunk(ctx, db, docNode.ID, srcTaint, now, fmt.Sprintf("chap_summary_%s_%d", docNode.ID, gi/groupSize), summary, "chap_summary", gi/groupSize)
	}

	// L3 文档级摘要（全文 → ≤200 tokens）
	allContent := make([]string, len(leaves))
	for i, lc := range leaves {
		allContent[i] = lc.content
	}
	joined := strings.Join(allContent, "\n\n")
	if len(joined) > 8000 { // 避免超长 prompt
		joined = joined[:8000] + "…"
	}
	prompt := fmt.Sprintf("请生成一个不超过200个token的文档级摘要：\n%s", joined)
	docSummary := p.summarizeText(ctx, prompt, 300)
	p.insertSummaryChunk(ctx, db, docNode.ID, srcTaint, now, fmt.Sprintf("doc_summary_%s", docNode.ID), docSummary, "doc_summary", -1)
}
