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
