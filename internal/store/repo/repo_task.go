package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// SQLiteTaskReadRepository 实现 protocol.TaskReadRepository。
// 只读读取 tasks 表和 events 表（写路径由 Blackboard CAS 持有 *sql.DB）。
// @arch: docs/upgrade/repo-interface-migration.md §3.6
type SQLiteTaskReadRepository struct {
	db *sql.DB
}

var _ protocol.TaskReadRepository = (*SQLiteTaskReadRepository)(nil)

// NewSQLiteTaskReadRepository 创建 SQLiteTaskReadRepository。
func NewSQLiteTaskReadRepository(db *sql.DB) *SQLiteTaskReadRepository {
	return &SQLiteTaskReadRepository{db: db}
}

// GetTaskProviderSuspendCount 返回指定任务的 provider_suspended_count。
func (r *SQLiteTaskReadRepository) GetTaskProviderSuspendCount(ctx context.Context, taskID string) (int, error) {
	var count int
	err := r.db.QueryRowContext(ctx,
		`SELECT provider_suspended_count FROM tasks WHERE task_id = ?`, taskID,
	).Scan(&count)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("SQLiteTaskReadRepository.GetTaskProviderSuspendCount: %w", err)
	}
	return count, nil
}

// GetTaskIntentTaint 返回指定任务的 intent_taint。
func (r *SQLiteTaskReadRepository) GetTaskIntentTaint(ctx context.Context, taskID string) (int, error) {
	var taint int
	err := r.db.QueryRowContext(ctx,
		`SELECT intent_taint FROM tasks WHERE task_id = ?`, taskID,
	).Scan(&taint)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("SQLiteTaskReadRepository.GetTaskIntentTaint: %w", err)
	}
	return taint, nil
}

// AggregateTokenCosts 从 events 表聚合指定时间范围内的 token 费用统计。
// startMicro / endMicro 为 Unix 微秒整数，与 events.created_at 列的存储格式一致
// （参见 cost_report.go aggregateCosts 中的 start.UnixMicro() 用法）。
// 事件 payload 格式：{"provider":"...","input_tokens":N,"output_tokens":N,"cache_read_tokens":N,"cost_usd":F}
func (r *SQLiteTaskReadRepository) AggregateTokenCosts(ctx context.Context, startISO, endISO string) ([]types.TokenCostAgg, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT payload FROM events
		 WHERE topic LIKE 'llm.%'
		   AND created_at >= ? AND created_at < ?`,
		startISO, endISO)
	if err != nil {
		return nil, fmt.Errorf("SQLiteTaskReadRepository.AggregateTokenCosts: %w", err)
	}
	defer rows.Close()

	type inferPayload struct {
		Provider        string  `json:"provider"`
		InputTokens     int64   `json:"input_tokens"`
		OutputTokens    int64   `json:"output_tokens"`
		CacheReadTokens int64   `json:"cache_read_tokens"`
		CostUSD         float64 `json:"cost_usd"`
	}

	agg := map[string]*types.TokenCostAgg{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			continue
		}
		var p inferPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			continue
		}
		pool := p.Provider
		if pool == "" {
			pool = "default"
		}
		a, ok := agg[pool]
		if !ok {
			a = &types.TokenCostAgg{Pool: pool}
			agg[pool] = a
		}
		a.TotalInput += p.InputTokens
		a.TotalOutput += p.OutputTokens
		a.TotalCacheRd += p.CacheReadTokens
		a.TotalCostUSD += p.CostUSD
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("SQLiteTaskReadRepository.AggregateTokenCosts rows: %w", err)
	}

	result := make([]types.TokenCostAgg, 0, len(agg))
	for _, a := range agg {
		result = append(result, *a)
	}
	return result, nil
}
