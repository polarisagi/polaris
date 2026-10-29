package repo

import (
	"context"
	"database/sql"
	"errors"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// SQLiteLLMResponseCacheRepository 实现 repo.LLMResponseCacheRepository
// （internal/protocol/schema/047_llm_response_cache.sql）。
//
// 读写分连接：Get 走读连接（read），避免命中路径在交互热路径上排队等单写连接；
// 写入/淘汰/清理走写连接（write）。二者可为同一个 SQLQuerier（测试、单连接内存库）。
type SQLiteLLMResponseCacheRepository struct {
	write protocol.SQLQuerier
	read  protocol.SQLQuerier
}

var _ repo.LLMResponseCacheRepository = (*SQLiteLLMResponseCacheRepository)(nil)

func NewSQLiteLLMResponseCacheRepository(write, read protocol.SQLQuerier) *SQLiteLLMResponseCacheRepository {
	if read == nil {
		read = write
	}
	return &SQLiteLLMResponseCacheRepository{write: write, read: read}
}

func (r *SQLiteLLMResponseCacheRepository) Get(ctx context.Context, key string, nowMs int64) (*repo.LLMCacheEntry, error) {
	e := repo.LLMCacheEntry{Key: key}
	err := r.read.QueryRowContext(ctx,
		`SELECT purpose, provider, model, response, created_at, expires_at, hit_count
		 FROM llm_response_cache WHERE key = ? AND expires_at > ?`, key, nowMs).
		Scan(&e.Purpose, &e.Provider, &e.Model, &e.Response, &e.CreatedAtMs, &e.ExpiresAtMs, &e.HitCount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // 未命中/已过期是正常结果，不是错误
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeStorageUnavailable, "SQLiteLLMResponseCacheRepository.Get", err)
	}
	return &e, nil
}

func (r *SQLiteLLMResponseCacheRepository) Put(ctx context.Context, e repo.LLMCacheEntry) error {
	_, err := r.write.ExecContext(ctx,
		`INSERT OR REPLACE INTO llm_response_cache
		 (key, purpose, provider, model, response, created_at, expires_at, hit_count)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 0)`,
		e.Key, e.Purpose, e.Provider, e.Model, e.Response, e.CreatedAtMs, e.ExpiresAtMs)
	if err != nil {
		return apperr.Wrap(apperr.CodeStorageUnavailable, "SQLiteLLMResponseCacheRepository.Put", err)
	}
	return nil
}

func (r *SQLiteLLMResponseCacheRepository) IncrHit(ctx context.Context, key string) error {
	_, err := r.write.ExecContext(ctx,
		`UPDATE llm_response_cache SET hit_count = hit_count + 1 WHERE key = ?`, key)
	if err != nil {
		return apperr.Wrap(apperr.CodeStorageUnavailable, "SQLiteLLMResponseCacheRepository.IncrHit", err)
	}
	return nil
}

// EvictOverflow 保留 created_at 最新的 maxEntries 行，其余删除。
// 同 created_at 时按 key 决胜，保证结果确定；OFFSET 语义下 LIMIT -1 即「不限条数」。
func (r *SQLiteLLMResponseCacheRepository) EvictOverflow(ctx context.Context, maxEntries int) (int64, error) {
	if maxEntries <= 0 {
		return 0, nil
	}
	res, err := r.write.ExecContext(ctx,
		`DELETE FROM llm_response_cache WHERE key IN (
			SELECT key FROM llm_response_cache ORDER BY created_at DESC, key DESC LIMIT -1 OFFSET ?)`, maxEntries)
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeStorageUnavailable, "SQLiteLLMResponseCacheRepository.EvictOverflow", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (r *SQLiteLLMResponseCacheRepository) DeleteExpired(ctx context.Context, nowMs int64) (int64, error) {
	res, err := r.write.ExecContext(ctx, `DELETE FROM llm_response_cache WHERE expires_at <= ?`, nowMs)
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeStorageUnavailable, "SQLiteLLMResponseCacheRepository.DeleteExpired", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
