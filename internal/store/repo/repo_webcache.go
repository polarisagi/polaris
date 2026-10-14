package repo

import (
	"context"
	"database/sql"
	"errors"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// SQLiteWebCacheRepository web_fetch_cache（049）的 SQLite 实现。
type SQLiteWebCacheRepository struct {
	db protocol.SQLQuerier
}

var _ repo.WebCacheRepository = (*SQLiteWebCacheRepository)(nil)

// NewSQLiteWebCacheRepository 创建 SQLiteWebCacheRepository 实例。
func NewSQLiteWebCacheRepository(db protocol.SQLQuerier) *SQLiteWebCacheRepository {
	return &SQLiteWebCacheRepository{db: db}
}

// GetWebCache 按 URL 哈希查询缓存；未命中返回 nil, nil。
func (r *SQLiteWebCacheRepository) GetWebCache(ctx context.Context, urlHash string) (*repo.WebCacheRow, error) {
	row := r.db.QueryRowContext(ctx, `SELECT url_hash, url, etag, last_modified, content_hash, title, markdown, fetched_at
		FROM web_fetch_cache WHERE url_hash = ?`, urlHash)
	var w repo.WebCacheRow
	if err := row.Scan(&w.URLHash, &w.URL, &w.ETag, &w.LastModified, &w.ContentHash, &w.Title, &w.Markdown, &w.FetchedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteWebCacheRepository.GetWebCache", err)
	}
	return &w, nil
}

// UpsertWebCache 插入或覆盖写入缓存条目。
func (r *SQLiteWebCacheRepository) UpsertWebCache(ctx context.Context, row repo.WebCacheRow) error {
	query := `INSERT INTO web_fetch_cache (url_hash, url, etag, last_modified, content_hash, title, markdown, fetched_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, COALESCE(NULLIF(?, ''), strftime('%Y-%m-%dT%H:%M:%SZ','now')))
		ON CONFLICT(url_hash) DO UPDATE SET
			url=excluded.url,
			etag=excluded.etag,
			last_modified=excluded.last_modified,
			content_hash=excluded.content_hash,
			title=excluded.title,
			markdown=excluded.markdown,
			fetched_at=excluded.fetched_at`
	_, err := r.db.ExecContext(ctx, query,
		row.URLHash, row.URL, row.ETag, row.LastModified, row.ContentHash, row.Title, row.Markdown, row.FetchedAt)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteWebCacheRepository.UpsertWebCache", err)
	}
	return nil
}

// TrimWebCache 按 fetched_at 倒序保留最新的 maxRows 行，清理更旧的缓存行。
func (r *SQLiteWebCacheRepository) TrimWebCache(ctx context.Context, maxRows int) error {
	if maxRows <= 0 {
		return nil
	}
	query := `DELETE FROM web_fetch_cache WHERE url_hash IN (
		SELECT url_hash FROM web_fetch_cache ORDER BY fetched_at DESC LIMIT -1 OFFSET ?
	)`
	_, err := r.db.ExecContext(ctx, query, maxRows)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteWebCacheRepository.TrimWebCache", err)
	}
	return nil
}
