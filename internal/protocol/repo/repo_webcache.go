package repo

import "context"

// WebCacheRow 表示 web_fetch_cache 表（049）的一行数据。
type WebCacheRow struct {
	URLHash      string
	URL          string
	ETag         string
	LastModified string
	ContentHash  string
	Title        string
	Markdown     string
	FetchedAt    string
}

// WebCacheRepository 条件请求缓存仓储接口（ADR-0114 决策三）。
type WebCacheRepository interface {
	// GetWebCache 按 URL 哈希查询缓存记录；不存在返回 nil, nil。
	GetWebCache(ctx context.Context, urlHash string) (*WebCacheRow, error)
	// UpsertWebCache 插入或更新缓存记录。
	UpsertWebCache(ctx context.Context, row WebCacheRow) error
	// TrimWebCache 保留最新 maxRows 行记录，清理其余最旧记录。
	TrimWebCache(ctx context.Context, maxRows int) error
}
