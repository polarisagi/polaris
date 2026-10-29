package repo

import (
	"context"
	"database/sql"
	"errors"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type SQLiteMockResponseCache struct {
	db *sql.DB
}

// NewSQLiteMockResponseCache 创建基于 SQLite 的 mock 响应缓存仓库
func NewSQLiteMockResponseCache(db *sql.DB) repo.MockResponseCache {
	return &SQLiteMockResponseCache{db: db}
}

func (r *SQLiteMockResponseCache) GetMockResponse(ctx context.Context, operationHash string) (*protocol.MockResponse, error) {
	query := `
		SELECT operation_hash, plan_session_id, method, url_pattern, status_code, response_body, hit_count, created_at, expires_at
		FROM mock_response_cache
		WHERE operation_hash = ?
	`
	row := r.db.QueryRowContext(ctx, query, operationHash)
	return r.scanMockResponse(row)
}

func (r *SQLiteMockResponseCache) SaveMockResponse(ctx context.Context, resp *protocol.MockResponse) error {
	query := `
		INSERT INTO mock_response_cache (
			operation_hash, plan_session_id, method, url_pattern, status_code, response_body, hit_count, created_at, expires_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(operation_hash) DO UPDATE SET
			plan_session_id = excluded.plan_session_id,
			method = excluded.method,
			url_pattern = excluded.url_pattern,
			status_code = excluded.status_code,
			response_body = excluded.response_body,
			hit_count = excluded.hit_count,
			expires_at = excluded.expires_at
	`
	var expiresAt sql.NullInt64
	if resp.ExpiresAt != nil {
		expiresAt.Int64 = *resp.ExpiresAt
		expiresAt.Valid = true
	}
	_, err := r.db.ExecContext(ctx, query,
		resp.OperationHash, resp.PlanSessionID, resp.Method, resp.URLPattern, resp.StatusCode,
		resp.ResponseBody, resp.HitCount, resp.CreatedAt, expiresAt,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "repo_mock: save mock response", err)
	}
	return nil
}

func (r *SQLiteMockResponseCache) scanMockResponse(row *sql.Row) (*protocol.MockResponse, error) {
	var m protocol.MockResponse
	var expiresAt sql.NullInt64
	err := row.Scan(
		&m.OperationHash, &m.PlanSessionID, &m.Method, &m.URLPattern,
		&m.StatusCode, &m.ResponseBody, &m.HitCount, &m.CreatedAt, &expiresAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, apperr.New(apperr.CodeNotFound, "repo_mock: mock response not found")
		}
		return nil, apperr.Wrap(apperr.CodeInternal, "repo_mock: scan mock response", err)
	}
	if expiresAt.Valid {
		v := expiresAt.Int64
		m.ExpiresAt = &v
	}
	return &m, nil
}
