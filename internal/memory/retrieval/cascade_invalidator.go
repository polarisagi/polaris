package retrieval

import (
	"context"
	"fmt"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// CascadeInvalidator 当实体被 superseded/expired 时，级联标记相关实体为 'pending_review'。
// 实现：BFS 图遍历（semantic_relations），最大深度 2 跳，防止全图扩散。
// 触发：ConsolidationPipeline.upsertSemantic 完成 belief revision 后调用。
type CascadeInvalidator struct {
	db protocol.SQLQuerier
}

// NewCascadeInvalidator 创建级联失效器。
func NewCascadeInvalidator(db protocol.SQLQuerier) *CascadeInvalidator {
	return &CascadeInvalidator{db: db}
}

// maxCascadeHops 最大扩散跳数，防止全图扫描。
const maxCascadeHops = 2

// Invalidate 对已被 superseded 的实体 entityID 执行级联失效，最多扩散 maxCascadeHops 跳。
// 返回实际标记为 pending_review 的实体 ID 列表。
func (ci *CascadeInvalidator) Invalidate(ctx context.Context, entityID int64) ([]int64, error) {
	visited := map[int64]bool{entityID: true}
	frontier := []int64{entityID}
	var pendingReview []int64

	for hop := 0; hop < maxCascadeHops && len(frontier) > 0; hop++ {
		neighbors, err := ci.queryNeighbors(ctx, frontier)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "cascade_invalidator: query neighbors", err)
		}

		var nextFrontier []int64
		for _, n := range neighbors {
			if !visited[n] {
				visited[n] = true
				nextFrontier = append(nextFrontier, n)
				pendingReview = append(pendingReview, n)
			}
		}
		frontier = nextFrontier
	}

	if len(pendingReview) == 0 {
		return nil, nil
	}

	// 批量标记为 pending_review（新增状态，需在 DDL 的 status 枚举注释中声明）
	now := time.Now().UnixMilli()
	if err := ci.markPendingReview(ctx, pendingReview, now); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "cascade_invalidator: mark pending_review", err)
	}

	// 写入 change_log 审计记录
	if err := ci.logInvalidation(ctx, entityID, pendingReview, now); err != nil {
		// 审计失败不阻断主流程
		_ = err
	}

	return pendingReview, nil
}

// queryNeighbors 查询实体的一阶相邻实体（双向边）。
func (ci *CascadeInvalidator) queryNeighbors(ctx context.Context, ids []int64) ([]int64, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	// 构造 IN 子句（小批量，最多 maxCascadeHops 跳×关系数，不会过大）
	placeholders := make([]any, 0, len(ids)*2)
	inClause := ""
	for i, id := range ids {
		if i > 0 {
			inClause += ","
		}
		inClause += "?"
		placeholders = append(placeholders, id)
	}

	query := fmt.Sprintf(
		`SELECT DISTINCT target_id FROM semantic_relations WHERE source_id IN (%s) AND target_id != 0
         UNION
         SELECT DISTINCT source_id FROM semantic_relations WHERE target_id IN (%s) AND source_id != 0`,
		inClause, inClause,
	)
	allArgs := append(placeholders, placeholders...)

	rows, err := ci.db.QueryContext(ctx, query, allArgs...)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "cascade_invalidator: query context", err)
	}
	defer rows.Close()

	var neighbors []int64
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "cascade_invalidator: scan row", err)
		}
		neighbors = append(neighbors, n)
	}
	return neighbors, rows.Err()
}

// markPendingReview 批量更新 status='pending_review'（仅对 status='active' 的实体）。
func (ci *CascadeInvalidator) markPendingReview(ctx context.Context, ids []int64, nowMs int64) error {
	for _, id := range ids {
		_, err := ci.db.ExecContext(ctx,
			`UPDATE semantic_entities SET status='pending_review', updated_at=? WHERE id=? AND status='active'`,
			nowMs, id,
		)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "cascade_invalidator: mark pending_review", err)
		}
	}
	return nil
}

// logInvalidation 写审计日志到 episodic_events_change_log。
func (ci *CascadeInvalidator) logInvalidation(
	ctx context.Context,
	sourceID int64,
	affected []int64,
	nowMs int64,
) error {
	_, err := ci.db.ExecContext(ctx,
		`INSERT INTO episodic_events_change_log(session_id, changed_at, change_type, affected_count)
         VALUES (?, ?, 'cascade_invalidation', ?)`,
		fmt.Sprintf("entity_%d", sourceID),
		nowMs,
		len(affected),
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "cascade_invalidator: log invalidation", err)
	}
	return nil
}
