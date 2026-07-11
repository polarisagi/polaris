package retrieval

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// CascadeInvalidator 当实体被 superseded/expired 时，级联标记相关实体为 'pending_review'。
// 实现：SQLite 递归 CTE 沿 semantic_relations 图遍历，最大深度 maxCascadeHops 跳，
// 防止全图扩散（GD-14-001 复核增强：原实现是 Go 侧多轮 BFS 循环，每跳一次 DB 往返；
// 现改为单条 WITH RECURSIVE 查询——递归项的 hop 列既是结果字段也是终止条件，
// 无论图中是否存在环，递归严格在 maxCascadeHops 轮内终止，语义与原 BFS 等价，
// 仅执行路径从 N 次往返收敛为 1 次）。
// relation_type 覆盖所有类型，含 'derived_from'（GD-14-001 新增，entity_extraction.tmpl
// 现在允许 LLM 显式标注"派生推论"关系，与既有 depends_on/configures/conflicts_with/
// relates_to 并列，语义更精确地覆盖"基础信念变更时其派生推论应连带失效"这一原始诉求）。
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

// cascadeCTEQuery 从种子实体 entityID 出发，沿 semantic_relations 双向边递归遍历，
// 最多 maxCascadeHops 跳。
//
// path 列记录本条路径已访问过的全部节点（'|id1|id2|...|' 分隔），递归项用
// instr(path, '|nbr|')=0 排除"沿刚走过的边折返回父节点/祖先节点"——SQLite 的
// WITH RECURSIVE 每轮只能看到上一轮新增的行（frontier-only 语义），没有 path
// 列的话，双向边会在下一跳把邻居重新展开回种子/祖先自身（TestCascadeInvalidator_
// CycleDoesNotHang / _TwoHopChain 曾复现此问题）。
// hop 列同时充当结果字段与递归终止条件：WHERE cascade.hop < ? 保证无论图是否
// 有环，递归轮数都严格有界，不会无限展开。
const cascadeCTEQuery = `
WITH RECURSIVE cascade(id, hop, path) AS (
    SELECT CAST(? AS INTEGER) AS id, 0 AS hop, '|' || CAST(? AS TEXT) || '|' AS path
    UNION ALL
    SELECT n.nbr, cascade.hop + 1, cascade.path || n.nbr || '|'
    FROM cascade
    JOIN (
        SELECT source_id AS node, target_id AS nbr FROM semantic_relations
        UNION ALL
        SELECT target_id AS node, source_id AS nbr FROM semantic_relations
    ) n ON n.node = cascade.id
    WHERE cascade.hop < ?
      AND instr(cascade.path, '|' || n.nbr || '|') = 0
)
SELECT DISTINCT id FROM cascade WHERE hop > 0 AND id != 0`

// Invalidate 对已被 superseded 的实体 entityID 执行级联失效，最多扩散 maxCascadeHops 跳。
// 返回实际标记为 pending_review 的实体 ID 列表。
func (ci *CascadeInvalidator) Invalidate(ctx context.Context, entityID int64) ([]int64, error) {
	pendingReview, err := ci.queryCascade(ctx, entityID)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "cascade_invalidator: query cascade", err)
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
		slog.Warn("cascade_invalidator: audit write failed", "err", err)
	}

	return pendingReview, nil
}

// queryCascade 执行 cascadeCTEQuery，返回种子实体 maxCascadeHops 跳以内的所有相邻实体
// （不含种子自身，即 hop=0 的行被 WHERE hop > 0 排除）。
func (ci *CascadeInvalidator) queryCascade(ctx context.Context, entityID int64) ([]int64, error) {
	rows, err := ci.db.QueryContext(ctx, cascadeCTEQuery, entityID, entityID, maxCascadeHops)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "cascade_invalidator: query context", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "cascade_invalidator: scan row", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "cascade_invalidator: rows iteration", err)
	}
	return ids, nil
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
