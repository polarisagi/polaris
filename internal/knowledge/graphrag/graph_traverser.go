package graphrag

import (
	"context"
	"database/sql"
	"log/slog"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

const (
	bfsDepthLimit   = 2   // M10 §2.6: BFS 深度上限（向后兑容默认値）
	bfsEdgesPerNode = 20  // 每节点最多出边
	bfsTotalNodes   = 200 // BFS 总节点数上限（Tier 0 内存约束）
)

// TraverseDirection 控制 BFS 游走的边方向。
// [Task 16] Path-Constrained Graph Search 新增参数。
type TraverseDirection string

const (
	DirectionOutgoing TraverseDirection = "outgoing" // 只走出边（source → target）
	DirectionIncoming TraverseDirection = "incoming" // 只走入边（target → source）
	DirectionBoth     TraverseDirection = "both"     // 両方向（默认行为）
)

// TraverseOptions 控制 BFS 路径约束。所有字段均为可选，零値表示使用默认行为（向后兑容）。
// [Task 16] Path-Constrained Graph Search：限定"沿着什么类型的关系走、走几跳、往哪个方向走"。
type TraverseOptions struct {
	RelationTypes []string          // 关系类型白名单（为空时不过滤）
	Direction     TraverseDirection // 为空则使用 DirectionOutgoing
	MaxDepth      int               // 为 0 则使用常量 bfsDepthLimit
}

func (o TraverseOptions) maxDepth() int {
	if o.MaxDepth > 0 {
		return o.MaxDepth
	}
	return bfsDepthLimit
}

func (o TraverseOptions) direction() TraverseDirection {
	if o.Direction == "" {
		return DirectionOutgoing
	}
	return o.Direction
}

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
	return gt.TraverseChunksWithOptions(ctx, queryText, topK, TraverseOptions{})
}

// TraverseChunksWithOptions 支持路径约束的图走读。
// [Task 16] 新 API：支持 RelationTypes 白名单 + Direction 方向约束 + MaxDepth 可配置深度。
// 默认行为与原 TraverseChunks 完全建边（向后兑容）。
//
//nolint:gocyclo
func (gt *GraphTraverser) TraverseChunksWithOptions(ctx context.Context, queryText string, topK int, opts TraverseOptions) ([]Chunk, error) {
	// Step 1: 以 queryText 文本匹配实体（FTS5 名称检索，限 top-5 种子）
	seeds, err := gt.findSeedEntities(ctx, queryText, 5)
	if err != nil || len(seeds) == 0 {
		return nil, apperr.Wrap(apperr.CodeInternal, "GraphTraverser.TraverseChunks", err) // 无种子实体，降级（调用方回退到 FTS5+Vector）
	}

	// Step 2: BFS 扩散（depth=2，≤200 节点，每节点 ≤20 出边）
	visited := make(map[int64]int) // entityID → BFS 深度
	queue := make([]bfsNode, 0, bfsTotalNodes)
	for _, id := range seeds {
		visited[id] = 0
		queue = append(queue, bfsNode{entityID: id, depth: 0})
	}

	maxDepth := opts.maxDepth()
	for i := 0; i < len(queue) && len(visited) < bfsTotalNodes; i++ {
		node := queue[i]
		if node.depth >= maxDepth {
			continue
		}
		neighbors, err := gt.fetchNeighbors(ctx, node.entityID, bfsEdgesPerNode, opts)
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

// fetchNeighbors 查询 entityID 的邻居节点。
// [Task 16] 支持关系类型白名单过滤 + 方向约束（outgoing/incoming/both）。
// 不传带约束时行为与原实现完全建边（向后兑容）。
func (gt *GraphTraverser) fetchNeighbors(ctx context.Context, entityID int64, limit int, opts TraverseOptions) ([]int64, error) {
	dir := opts.direction()

	// 建立关系类型 WHERE 子句（白名单过滤）
	// 出于安全考虑，白名单内容由调用方控制，不操拤毛 SQL
	var typeFilter string
	var typeArgs []any
	if len(opts.RelationTypes) > 0 {
		placeholders := make([]string, len(opts.RelationTypes))
		for i, rt := range opts.RelationTypes {
			placeholders[i] = "?"
			typeArgs = append(typeArgs, rt)
		}
		typeFilter = " AND r.relation_type IN (" + joinStr(placeholders, ",") + ")"
	}

	var q string
	var args []any

	switch dir {
	case DirectionIncoming:
		// incoming: target=entityID, 返回 source_id
		q = `SELECT r.source_id FROM semantic_relations r
			JOIN semantic_entities e ON r.source_id = e.id AND e.status = 'active'
			WHERE r.target_id = ?` + typeFilter + `
			ORDER BY r.weight DESC LIMIT ?`
		args = append([]any{entityID}, typeArgs...)
		args = append(args, limit)
	case DirectionBoth:
		// both: UNION 出边和入边
		q = `SELECT r.target_id FROM semantic_relations r
			JOIN semantic_entities e ON r.target_id = e.id AND e.status = 'active'
			WHERE r.source_id = ?` + typeFilter + `
			UNION
			SELECT r.source_id FROM semantic_relations r
			JOIN semantic_entities e ON r.source_id = e.id AND e.status = 'active'
			WHERE r.target_id = ?` + typeFilter + `
			ORDER BY 1 LIMIT ?`
		args = append([]any{entityID}, typeArgs...)
		args = append(args, entityID)
		args = append(args, typeArgs...)
		args = append(args, limit)
	default: // DirectionOutgoing
		q = `SELECT r.target_id FROM semantic_relations r
			JOIN semantic_entities e ON r.target_id = e.id AND e.status = 'active'
			WHERE r.source_id = ?` + typeFilter + `
			ORDER BY r.weight DESC LIMIT ?`
		args = append([]any{entityID}, typeArgs...)
		args = append(args, limit)
	}

	rows, err := gt.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "GraphTraverser.fetchNeighbors", err)
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

// joinStr 连接字符串切片。
func joinStr(ss []string, sep string) string {
	result := ""
	for i, s := range ss {
		if i > 0 {
			result += sep
		}
		result += s
	}
	return result
}

// fetchEntityName 获取实体名称。
func (gt *GraphTraverser) fetchEntityName(ctx context.Context, entityID int64) (string, error) {
	var name string
	err := gt.db.QueryRowContext(ctx,
		`SELECT name FROM semantic_entities WHERE id = ?`, entityID).Scan(&name)
	if err != nil {
		return name, apperr.Wrap(apperr.CodeInternal, "GraphTraverser.fetchEntityName", err)
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
		return nil, apperr.Wrap(apperr.CodeInternal, "GraphTraverser.chunksForEntity", err)
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
