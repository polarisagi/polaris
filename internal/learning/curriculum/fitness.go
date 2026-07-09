package curriculum

import (
	"context"
	"database/sql"
	"log/slog"

	"github.com/polarisagi/polaris/internal/protocol"
)

// SQLFitnessEvaluator 基于历史执行事件计算技能适应度，避免调用 LLM。
// 适应度 = 成功率 × (1 - 平均预测误差)；样本量不足时返回 -1（跳过评估，交由 LLM judge）。
type SQLFitnessEvaluator struct {
	querier protocol.SQLQuerier
}

// NewSQLFitnessEvaluator 构造评估器；querier 为只读 DB 查询接口。
func NewSQLFitnessEvaluator(q protocol.SQLQuerier) *SQLFitnessEvaluator {
	return &SQLFitnessEvaluator{querier: q}
}

const minSamples = 5 // 低于此样本量时跳过 SQL 评估

// EvaluateFitness 查询 events 表，返回 [0,1] 适应度或 -1（样本不足）。
// fitness < 0.5 → 淘汰（不需要调用 LLM）。
// fitness >= 0.5 或 -1 → 交由后续 LLM-as-Judge 决策。
func (e *SQLFitnessEvaluator) EvaluateFitness(ctx context.Context, skillID string) float64 {
	const q = `
		SELECT
			COUNT(*) FILTER (WHERE json_extract(payload,'$.status') = 'done')  AS success_cnt,
			COUNT(*)                                                             AS total_cnt,
			COALESCE(AVG(CAST(json_extract(payload,'$.prediction_error') AS REAL)), 0.5) AS avg_err
		FROM events
		WHERE type = 'tool_execute'
		  AND json_extract(payload,'$.skill_id') = ?
		  AND created_at > strftime('%Y-%m-%dT%H:%M:%SZ','now','-7 days')
	`
	var successCnt, totalCnt int
	var avgErr float64
	row := e.querier.QueryRowContext(ctx, q, skillID)
	if err := row.Scan(&successCnt, &totalCnt, &avgErr); err != nil {
		if err != sql.ErrNoRows {
			slog.Warn("curriculum: sql fitness query failed", "skill_id", skillID, "err", err)
		}
		return -1
	}
	if totalCnt < minSamples {
		return -1 // 样本不足，跳过 SQL 评估
	}
	successRate := float64(successCnt) / float64(totalCnt)
	fitness := successRate * (1 - avgErr)
	return fitness
}
