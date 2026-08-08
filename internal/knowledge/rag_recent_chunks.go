package knowledge

import (
	"context"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// GetRecentChunks 返回最近写入的 limit 条 rag_chunks 内容（SyntheticEvalGen
// Pipeline 数据源，cmd/polaris/boot_agent.go "synthetic-eval-gen" 每小时轮询）。
//
// 2026-08-08 补齐：此前返回写死占位串（注释"不需要真查 DB"）。同款缺陷曾于
// 2026-07-14 在 PipelineImpl 上修过一次，但生产走的是本类型（boot_agent.go:353
// 调 kb.Ingester.GetRecentChunks），那次修复打在了并行的孪生实现上，从未生效。
// 后果：GenerateCases/SyntheticCaseToEvalCase/PutCase 三段下游管线都是真实现，
// 唯独数据源恒定——每小时消耗一次真实 LLM 配额，产出的合成用例与知识库实际
// 内容无关。孪生实现已随本次提交删除，防止修复再次落到死分支上。
func (p *DefaultIngestionPipeline) GetRecentChunks(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 10
	}
	db, err := p.router.GetPrimary()
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "ingestion: GetRecentChunks get primary db failed", err)
	}
	rows, err := db.QueryContext(ctx,
		`SELECT content FROM rag_chunks WHERE deleted_at IS NULL AND content != '' ORDER BY created_at DESC LIMIT ?`,
		limit)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "ingestion: GetRecentChunks query failed", err)
	}
	defer rows.Close()

	chunks := make([]string, 0, limit)
	for rows.Next() {
		var content string
		if scanErr := rows.Scan(&content); scanErr != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "ingestion: GetRecentChunks scan failed", scanErr)
		}
		chunks = append(chunks, content)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "ingestion: GetRecentChunks rows iteration failed", rowsErr)
	}
	return chunks, nil
}
