package repo

import (
	"context"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// SQLiteLLMCallRepository 写入 llm_calls 表（internal/protocol/schema/039_llm_calls.sql），
// 结构满足 internal/llm.UsageRecorder。
type SQLiteLLMCallRepository struct {
	db protocol.SQLQuerier
}

func NewSQLiteLLMCallRepository(db protocol.SQLQuerier) *SQLiteLLMCallRepository {
	return &SQLiteLLMCallRepository{db: db}
}

// RecordLLMUsage 插入一行调用记账。
func (r *SQLiteLLMCallRepository) RecordLLMUsage(ctx context.Context, rec protocol.LLMUsageRecord) error {
	streaming := 0
	if rec.Streaming {
		streaming = 1
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO llm_calls (
		id, created_at, session_id, purpose, provider, model_id, model_pool, thinking_mode, streaming,
		status, input_tokens, cache_hit_tokens, output_tokens, reasoning_tokens, latency_ms, cost_usd, error
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ID, rec.CreatedAtMs, rec.SessionID, rec.Purpose, rec.Provider, rec.ModelID, rec.ModelPool,
		rec.ThinkingMode, streaming, rec.Status, rec.InputTokens, rec.CacheHitTokens, rec.OutputTokens,
		rec.ReasoningTokens, rec.LatencyMs, rec.CostUSD, rec.Error)
	if err != nil {
		return apperr.Wrap(apperr.CodeStorageUnavailable, "SQLiteLLMCallRepository.RecordLLMUsage", err)
	}
	return nil
}

// SQLiteLLMSpendLedger 是 agent.LLMSpendLedger 的实现：对 llm_calls 做 SUM 只读聚合
// （ADR-0104 决策四）。应传入读连接（store.ReadDB()），避免占用单写连接。
type SQLiteLLMSpendLedger struct {
	db protocol.SQLQuerier
}

func NewSQLiteLLMSpendLedger(db protocol.SQLQuerier) *SQLiteLLMSpendLedger {
	return &SQLiteLLMSpendLedger{db: db}
}

// SessionTokens 该会话累计 input+output token（input 已含缓存命中，output 已含推理）。
func (l *SQLiteLLMSpendLedger) SessionTokens(ctx context.Context, sessionID string) (int64, error) {
	var n int64
	err := l.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(input_tokens + output_tokens), 0) FROM llm_calls WHERE session_id = ?`,
		sessionID).Scan(&n)
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeStorageUnavailable, "SQLiteLLMSpendLedger.SessionTokens", err)
	}
	return n, nil
}

// SpendUSDSince 自 sinceMs（Unix 毫秒，含）起全部会话的累计 cost_usd。
func (l *SQLiteLLMSpendLedger) SpendUSDSince(ctx context.Context, sinceMs int64) (float64, error) {
	var v float64
	err := l.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(cost_usd), 0) FROM llm_calls WHERE created_at >= ?`, sinceMs).Scan(&v)
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeStorageUnavailable, "SQLiteLLMSpendLedger.SpendUSDSince", err)
	}
	return v, nil
}
