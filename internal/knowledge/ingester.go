package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/internal/knowledge/graphrag"
	"github.com/polarisagi/polaris/internal/llm/safecall"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/internal/store/search"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

func simpleHash(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

// PipelineImpl 实现 IngestionPipeline (M10 Knowledge RAG 摄取管道)。
// 生产实现: 将文本分块落盘到 SQLite rag_chunks 表（FTS5 支持），
// 同时将 DocTree 元数据持久化到 rag_docs 表（JSON 序列化）。
type PipelineImpl struct {
	db                 protocol.SQLQuerier
	provider           protocol.Provider
	outboxWriter       protocol.OutboxWriter
	searchEngine       *search.HybridSearchEngine
	boundarySerializer *taint.TaintBoundarySerializer // 可选；nil 时不计算/校验 taint_hmac（inv_M11_02）
}

var _ IngestionPipeline = (*PipelineImpl)(nil)

// NewPipeline 创建 PipelineImpl，并确保 rag_docs 表存在。
func NewPipeline(db protocol.SQLQuerier, provider protocol.Provider, outboxWriter protocol.OutboxWriter, searchEngine *search.HybridSearchEngine, boundarySerializer *taint.TaintBoundarySerializer) *PipelineImpl {
	// CREATE TABLE IF NOT EXISTS，幂等；上线前阶段可随主 schema 建表
	_, _ = db.ExecContext(context.Background(), `
		CREATE TABLE IF NOT EXISTS rag_docs (
			uri          TEXT PRIMARY KEY,
			title        TEXT NOT NULL DEFAULT '',
			source_type  TEXT NOT NULL DEFAULT '',
			content_hash TEXT NOT NULL DEFAULT '',
			tree_json    TEXT NOT NULL DEFAULT '{}',
			created_at   TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at   TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)
	`)
	return &PipelineImpl{db: db, provider: provider, outboxWriter: outboxWriter, searchEngine: searchEngine, boundarySerializer: boundarySerializer}
}

// Ingest 将文档转换为 Chunk 并持久化。
func (p *PipelineImpl) Ingest(ctx context.Context, doc *Document, initialTaint int) (*DocTree, error) { //nolint:gocyclo
	if doc == nil {
		return nil, apperr.New(apperr.CodeInvalidInput, "knowledge: doc is nil")
	}

	// 增量检测：hash 相同则跳过重摄取，返回缓存 DocTree
	var existingHash string
	_ = p.db.QueryRowContext(ctx,
		`SELECT content_hash FROM rag_docs WHERE uri = ? AND deleted_at IS NULL`, doc.Ref.URI,
	).Scan(&existingHash)
	if existingHash != "" && existingHash == doc.Ref.ContentHash {
		var treeJSON string
		if err := p.db.QueryRowContext(ctx,
			`SELECT tree_json FROM rag_docs WHERE uri = ? AND deleted_at IS NULL`, doc.Ref.URI,
		).Scan(&treeJSON); err == nil && treeJSON != "" {
			var cached DocTree
			if json.Unmarshal([]byte(treeJSON), &cached) == nil {
				return &cached, nil
			}
		}
	}

	content := string(doc.Raw)

	var chunker ChunkStrategy
	switch doc.Ref.SourceType {
	case "md", "markdown":
		chunker = &MarkdownChunker{}
	case "go", "py", "python", "js", "ts", "javascript", "typescript", "java", "cpp", "c", "rs", "rust":
		chunker = &CodeChunker{}
	default:
		chunker = &PlainTextChunker{}
	}
	parts := chunker.Chunk(content, doc.Ref.SourceType)

	var nodes []*DocNode //nolint:prealloc

	for i, part := range parts {
		if part == "" {
			continue
		}
		contentHash := fmt.Sprintf("%x", simpleHash(part))
		chunkID := fmt.Sprintf("%s_chunk_%d", doc.Ref.ContentHash, i)

		nodes = append(nodes, &DocNode{
			ID:      chunkID,
			Title:   fmt.Sprintf("Chunk %d", i),
			Level:   1,
			Content: part,
		})

		// 持久化 Chunk 到 SQLite rag_chunks 表（含 inv_M10_03 lineage 字段 +
		// inv_M11_02 跨边界 HMAC 签名）
		hmacHex := sealChunkTaint(p.boundarySerializer, chunkID, part, initialTaint, "")
		_, err := p.db.ExecContext(ctx, `
			INSERT INTO rag_chunks (id, doc_id, content, taint_level, taint_source, taint_hmac,
				source_uri, doc_version, chunk_seq, content_hash, embed_model_version)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '')
			ON CONFLICT(id) DO UPDATE SET
				content=excluded.content,
				taint_level=excluded.taint_level,
				taint_source=excluded.taint_source,
				taint_hmac=excluded.taint_hmac,
				source_uri=excluded.source_uri,
				doc_version=excluded.doc_version,
				chunk_seq=excluded.chunk_seq,
				content_hash=excluded.content_hash,
				created_at=CURRENT_TIMESTAMP
		`, chunkID, doc.Ref.URI, part, initialTaint, "", hmacHex,
			doc.Ref.URI, doc.Ref.ContentHash, i, contentHash)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "ingester: insert chunk failed", err)
		}
		if p.searchEngine != nil {
			_ = p.searchEngine.AddDocument(ctx, chunkID, part)
		}
	}

	tree := &DocTree{
		Document: &DocNode{
			ID:       doc.Ref.URI,
			Title:    doc.Ref.Title,
			Level:    0,
			Children: nodes,
		},
		SourceURL: doc.Ref.URI,
	}

	// 持久化 DocTree 到 rag_docs 表（JSON 序列化）
	treeJSON, _ := json.Marshal(tree)
	if _, docErr := p.db.ExecContext(ctx, `
		INSERT INTO rag_docs (uri, title, source_type, content_hash, tree_json, updated_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(uri) DO UPDATE SET
			title        = excluded.title,
			source_type  = excluded.source_type,
			content_hash = excluded.content_hash,
			tree_json    = excluded.tree_json,
			updated_at   = CURRENT_TIMESTAMP
	`, doc.Ref.URI, doc.Ref.Title, doc.Ref.SourceType, doc.Ref.ContentHash, string(treeJSON)); docErr != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "ingester: upsert rag_docs failed", docErr)
	}

	// LLM 摘要生成（Tier 1+，provider 可用时）
	// 为每个 ParentChunk 生成 summary 类型块，供 StructuredNavigator 的 FTS5 导航使用。
	if p.provider != nil && len(nodes) > 0 {
		for _, node := range nodes {
			if node.Level != 1 || node.Content == "" {
				continue
			}
			summaryID := node.ID + "_summary"
			resp, err := safecall.Infer(ctx, p.provider, []types.Message{
				{Role: "system", Content: "用一句话（50字以内）总结以下文本片段的核心内容，输出纯文本，不加任何格式："},
				{Role: "user", Content: node.Content},
			}, types.WithModel("standard"))
			if err != nil || resp == nil || resp.Content == "" {
				continue
			}
			summaryHash := fmt.Sprintf("%x", simpleHash(resp.Content))
			summaryHMAC := sealChunkTaint(p.boundarySerializer, summaryID, resp.Content, initialTaint, "llm_summary")
			if _, err := p.db.ExecContext(ctx, `
				INSERT INTO rag_chunks (id, doc_id, content, taint_level, taint_source, taint_hmac,
					source_uri, doc_version, chunk_seq, content_hash, embed_model_version, chunk_type)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', 'summary')
				ON CONFLICT(id) DO UPDATE SET
					content=excluded.content,
					content_hash=excluded.content_hash,
					taint_hmac=excluded.taint_hmac,
					created_at=CURRENT_TIMESTAMP
			`, summaryID, doc.Ref.URI, resp.Content, initialTaint, "llm_summary", summaryHMAC,
				doc.Ref.URI, doc.Ref.ContentHash, -1, summaryHash); err != nil {
				slog.WarnContext(ctx, "knowledge_ingester: db write failed", "error", err)
			}
		}
	}

	// 异步触发 GraphBuild（Outbox 解耦）。
	// 幂等键必须真正唯一（2026-07-22 一致性审查修复）：outbox.idempotency_key
	// 是 UNIQUE 约束列，空字符串会导致同一部署生命周期内只有*全局第一份*
	// 被摄入的文档能成功写入这条 outbox 记录——此后每一份新文档的 GraphBuild
	// 触发都会因约束冲突被静默丢弃（`_ =` 吞掉了 Write 的错误返回值），知识图谱
	// 构建实际上从第二份文档起就从未真正触发过。用文档 URI + 纳秒时间戳：
	// 前者保留可读性/可追溯性，后者保证每次真实摄入都不会与历史记录冲突。
	if p.outboxWriter != nil {
		idemKey := fmt.Sprintf("ragdoc:%s:%d", doc.Ref.URI, time.Now().UnixNano())
		ev, _ := protocol.NewOutboxEvent(graphrag.EventTypeRAGDocIngested, "", map[string]string{"doc_id": doc.Ref.URI}, idemKey)
		_ = p.outboxWriter.Write(ctx, ev)
	}

	return tree, nil
}

// Delete 删除指定文档的所有片段。
func (p *PipelineImpl) Delete(ctx context.Context, uri string) error {
	_, err := p.db.ExecContext(ctx, "UPDATE rag_chunks SET deleted_at = CURRENT_TIMESTAMP WHERE doc_id = ?", uri)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "PipelineImpl.Delete", err)
	}
	return nil
}

// GetRecentChunks 返回最近写入的 limit 条 rag_chunks 内容（SyntheticEvalGen
// Pipeline 数据源，cmd/polaris/boot_agent.go "synthetic-eval-gen" 每小时轮询）。
//
// 2026-07-14 补齐：此前硬编码返回同一条写死字符串（注释"不需要真查 DB"），
// 导致该小时级任务每次调用 LLM 合成用例时输入永远是同一句占位文本——
// GenerateCases/SyntheticCaseToEvalCase/PutCase 下游三段管线本身都是真实实现
// （见 internal/learning/synthetic/synthetic_eval_gen.go 三阶段 RAGAS 蒸馏 +
// internal/eval/synthetic_adapter.go 字段映射），唯独数据源是假的——每小时
// 消耗一次真实 LLM 调用配额，产出的合成用例却与知识库实际内容无关，是
// "下游全真实、上游全虚假"的隐蔽缺口。
func (p *PipelineImpl) GetRecentChunks(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := p.db.QueryContext(ctx,
		`SELECT content FROM rag_chunks WHERE deleted_at IS NULL AND content != '' ORDER BY created_at DESC LIMIT ?`,
		limit)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "ingester: GetRecentChunks query failed", err)
	}
	defer rows.Close()

	chunks := make([]string, 0, limit)
	for rows.Next() {
		var content string
		if scanErr := rows.Scan(&content); scanErr != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "ingester: GetRecentChunks scan failed", scanErr)
		}
		chunks = append(chunks, content)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "ingester: GetRecentChunks rows iteration failed", rowsErr)
	}
	return chunks, nil
}
