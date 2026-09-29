package repo

import (
	"context"
	"fmt"

	"github.com/polarisagi/polaris/internal/protocol"
	protorepo "github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// SQLiteLLMUsageQuery 对 llm_calls 做只读聚合（ADR-0105 决策八）。应传入读连接（store.ReadDB()）。
type SQLiteLLMUsageQuery struct {
	db protocol.SQLQuerier
}

func NewSQLiteLLMUsageQuery(db protocol.SQLQuerier) *SQLiteLLMUsageQuery {
	return &SQLiteLLMUsageQuery{db: db}
}

// usageGroupExpr 把分组维度映射到常量 SQL 表达式。
// 为什么用白名单映射而非拼接：group_by 来自 HTTP 参数，表达式无法参数化绑定，
// 只有"枚举 → 编译期常量"这一条路才不会引入 SQL 注入面。
func usageGroupExpr(g protorepo.LLMUsageGroupBy) (expr, order string, ok bool) {
	switch g {
	case protorepo.LLMUsageByPurpose:
		return "purpose", "SUM(cost_usd) DESC, k ASC", true
	case protorepo.LLMUsageByModel:
		return "model_id", "SUM(cost_usd) DESC, k ASC", true
	case protorepo.LLMUsageByProvider:
		return "provider", "SUM(cost_usd) DESC, k ASC", true
	case protorepo.LLMUsageByDay:
		// created_at 是 Unix 毫秒；按 UTC 自然日分桶，与错峰窗口同为 UTC 便于对账。
		return "strftime('%Y-%m-%d', created_at / 1000, 'unixepoch')", "k ASC", true
	}
	return "", "", false
}

// AggregateLLMUsage 见 protorepo.LLMUsageQueryRepository。
// 时间过滤是 created_at 的范围条件，走 idx_llm_calls_created_at（039_llm_calls.sql），
// 分组在范围命中的行上做临时 B-tree，不需要额外索引。
func (r *SQLiteLLMUsageQuery) AggregateLLMUsage(ctx context.Context, q protorepo.LLMUsageQuery) ([]protorepo.LLMUsageGroup, error) {
	expr, order, ok := usageGroupExpr(q.GroupBy)
	if !ok {
		return nil, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("llm usage: unsupported group_by %q", q.GroupBy))
	}
	// expr/order 仅来自 usageGroupExpr 的常量白名单；时间边界走参数绑定。
	query := `SELECT ` + expr + ` AS k,
		COUNT(*),
		COALESCE(SUM(CASE WHEN status = 'cache_hit' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(input_tokens), 0),
		COALESCE(SUM(cache_hit_tokens), 0),
		COALESCE(SUM(output_tokens), 0),
		COALESCE(SUM(reasoning_tokens), 0),
		COALESCE(SUM(cost_usd), 0)
		FROM llm_calls WHERE created_at >= ? AND created_at < ?
		GROUP BY k ORDER BY ` + order
	rows, err := r.db.QueryContext(ctx, query, q.SinceMs, q.UntilMs)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeStorageUnavailable, "SQLiteLLMUsageQuery.AggregateLLMUsage", err)
	}
	defer rows.Close()
	var out []protorepo.LLMUsageGroup
	for rows.Next() {
		var g protorepo.LLMUsageGroup
		if err := rows.Scan(&g.Key, &g.Requests, &g.ResponseCacheHits, &g.InputTokens, &g.CacheHitTokens,
			&g.OutputTokens, &g.ReasoningTokens, &g.CostUSD); err != nil {
			return nil, apperr.Wrap(apperr.CodeStorageUnavailable, "SQLiteLLMUsageQuery.AggregateLLMUsage: scan", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeStorageUnavailable, "SQLiteLLMUsageQuery.AggregateLLMUsage: rows", err)
	}
	return out, nil
}

// llmCallsPruneBatch 单批删除行数。SQLite 单写者：一条大 DELETE 会长时间独占写连接，
// 阻塞 llm_calls 记账与其他写入，所以分批、批间让出。
const llmCallsPruneBatch = 5000

// PruneLLMCalls 删除 created_at < beforeMs 的 llm_calls 行，返回删除总数（ADR-0105 决策八保留期）。
// 应传入写连接的仓库实例（NewSQLiteLLMCallRepository(store.DB())）。ctx 取消时返回已删数与取消错误。
func (r *SQLiteLLMCallRepository) PruneLLMCalls(ctx context.Context, beforeMs int64) (int64, error) {
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, apperr.Wrap(apperr.CodeCancelled, "SQLiteLLMCallRepository.PruneLLMCalls", err)
		}
		res, err := r.db.ExecContext(ctx,
			`DELETE FROM llm_calls WHERE id IN (SELECT id FROM llm_calls WHERE created_at < ? LIMIT ?)`,
			beforeMs, llmCallsPruneBatch)
		if err != nil {
			return total, apperr.Wrap(apperr.CodeStorageUnavailable, "SQLiteLLMCallRepository.PruneLLMCalls", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, apperr.Wrap(apperr.CodeStorageUnavailable, "SQLiteLLMCallRepository.PruneLLMCalls: rows affected", err)
		}
		total += n
		if n < llmCallsPruneBatch {
			return total, nil
		}
	}
}
