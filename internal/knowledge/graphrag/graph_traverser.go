package graphrag

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

const (
	bfsDepthLimit   = 2   // M10 §2.6: BFS 深度上限
	bfsEdgesPerNode = 20  // 每节点最多出边
	bfsTotalNodes   = 200 // BFS 总节点数上限（Tier 0 内存约束）
)

// GraphTraverser 实现 M10 §2.6 LocalSearch：BFS 图遍历，收集关联 chunk。
// 使用 SQLite semantic_entities + semantic_relations（Tier 0 邻接表）。
type GraphTraverser struct {
	db protocol.SQLQuerier
}

// NewGraphTraverser 创建图遍历器。
func NewGraphTraverser(db protocol.SQLQuerier) *GraphTraverser {
	return &GraphTraverser{db: db}
}

// TraverseChunks 以 queryText 匹配种子实体，BFS 扩散（depth=2），
// 收集关联 rag_chunks，返回评分 Chunk 列表（按 BFS 深度衰减评分）。
//
//nolint:gocyclo
func (gt *GraphTraverser) TraverseChunks(ctx context.Context, queryText string, topK int) ([]Chunk, error) {
	// Step 1: 以 queryText 文本匹配实体（FTS5 名称检索，限 top-5 种子）
	seeds, err := gt.findSeedEntities(ctx, queryText, 5)
	if err != nil || len(seeds) == 0 {
		return nil, fmt.Errorf("GraphTraverser.TraverseChunks: %w", err) // 无种子实体，降级（调用方回退到 FTS5+Vector）
	}

	// Step 2: BFS 扩散（depth=2，≤200 节点，每节点 ≤20 出边）
	visited := make(map[int64]int) // entityID → BFS 深度
	queue := make([]bfsNode, 0, bfsTotalNodes)
	for _, id := range seeds {
		visited[id] = 0
		queue = append(queue, bfsNode{entityID: id, depth: 0})
	}

	for i := 0; i < len(queue) && len(visited) < bfsTotalNodes; i++ {
		node := queue[i]
		if node.depth >= bfsDepthLimit {
			continue
		}
		neighbors, err := gt.fetchNeighbors(ctx, node.entityID, bfsEdgesPerNode)
		if err != nil {
			slog.Warn("graph_traverser: fetchNeighbors failed", "entity_id", node.entityID, "err", err)
			continue
		}
		for _, n := range neighbors {
			if _, seen := visited[n]; !seen && len(visited) < bfsTotalNodes {
				visited[n] = node.depth + 1
				queue = append(queue, bfsNode{entityID: n, depth: node.depth + 1})
			}
		}
	}

	// Step 3: 从 BFS 节点的 entity_type 匹配 rag_chunks（通过 entity name BM25 召回）
	// 关联策略：将实体名称作为检索词，在 rag_chunks_fts 中召回关联文档 chunk。
	// 评分：depth=0 → 1.0，depth=1 → 0.7，depth=2 → 0.4（距离衰减）
	depthScore := map[int]float64{0: 1.0, 1: 0.7, 2: 0.4}
	chunkScores := make(map[string]float64) // chunkID → score
	chunkMap := make(map[string]Chunk)

	for entityID, depth := range visited {
		entityName, err := gt.fetchEntityName(ctx, entityID)
		if err != nil || entityName == "" {
			continue
		}
		chunks, err := gt.chunksForEntity(ctx, entityName, 5)
		if err != nil {
			continue
		}
		score := depthScore[depth]
		for _, c := range chunks {
			if existing, ok := chunkScores[c.ID]; !ok || score > existing {
				chunkScores[c.ID] = score
				chunkMap[c.ID] = c
			}
		}
	}

	// Step 4: 按分数排序，截取 topK
	type scored struct {
		chunk Chunk
		score float64
	}
	results := make([]scored, 0, len(chunkMap))
	for id, c := range chunkMap {
		results = append(results, scored{chunk: c, score: chunkScores[id]})
	}
	// 简单排序（数量 ≤200，无需堆）
	for i := 1; i < len(results); i++ {
		for j := i; j > 0 && results[j].score > results[j-1].score; j-- {
			results[j], results[j-1] = results[j-1], results[j]
		}
	}
	if topK > 0 && len(results) > topK {
		results = results[:topK]
	}

	out := make([]Chunk, len(results))
	for i, r := range results {
		out[i] = r.chunk
	}
	return out, nil
}

type bfsNode struct {
	entityID int64
	depth    int
}

// findSeedEntities 用 FTS5 或名称 LIKE 匹配活跃实体，返回最多 limit 个 ID。
func (gt *GraphTraverser) findSeedEntities(ctx context.Context, queryText string, limit int) ([]int64, error) {
	rows, err := gt.db.QueryContext(ctx, `
		SELECT id FROM semantic_entities
		WHERE status = 'active' AND (
			name LIKE '%' || ? || '%'
		)
		LIMIT ?`, queryText, limit)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "graph_traverser: seed entity query failed", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

// fetchNeighbors 查询 entityID 的出边邻居（source → target），返回 target entity ID 列表。
func (gt *GraphTraverser) fetchNeighbors(ctx context.Context, entityID int64, limit int) ([]int64, error) {
	rows, err := gt.db.QueryContext(ctx, `
		SELECT r.target_id
		FROM semantic_relations r
		JOIN semantic_entities e ON r.target_id = e.id AND e.status = 'active'
		WHERE r.source_id = ?
		ORDER BY r.weight DESC
		LIMIT ?`, entityID, limit)
	if err != nil {
		return nil, fmt.Errorf("GraphTraverser.fetchNeighbors: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

// fetchEntityName 获取实体名称。
func (gt *GraphTraverser) fetchEntityName(ctx context.Context, entityID int64) (string, error) {
	var name string
	err := gt.db.QueryRowContext(ctx,
		`SELECT name FROM semantic_entities WHERE id = ?`, entityID).Scan(&name)
	if err != nil {
		return name, fmt.Errorf("GraphTraverser.fetchEntityName: %w", err)
	}
	return name, nil
}

// chunksForEntity 按实体名称在 rag_chunks_fts 中检索关联 chunk。
func (gt *GraphTraverser) chunksForEntity(ctx context.Context, entityName string, limit int) ([]Chunk, error) {
	rows, err := gt.db.QueryContext(ctx, `
		SELECT rc.id, rc.doc_id, rc.content, rc.taint_level, rc.taint_source
		FROM rag_chunks rc
		WHERE rc.rowid IN (
			SELECT rowid FROM rag_chunks_fts
			WHERE rag_chunks_fts MATCH ?
			ORDER BY rank LIMIT ?
		) AND rc.deleted_at IS NULL`, entityName, limit)
	if err != nil {
		return nil, fmt.Errorf("GraphTraverser.chunksForEntity: %w", err)
	}
	defer rows.Close()
	var chunks []Chunk
	for rows.Next() {
		var c Chunk
		var ts sql.NullString
		if err := rows.Scan(&c.ID, &c.DocID, &c.Content, &c.TaintLevel, &ts); err == nil {
			if ts.Valid {
				c.TaintSource = ts.String
			}
			chunks = append(chunks, c)
		}
	}
	return chunks, rows.Err()
}
