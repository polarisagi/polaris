package repo

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/internal/protocol/schema"
)

func newWebCacheTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })

	ddl, err := schema.FS.ReadFile("049_web_fetch_cache.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestWebCacheRepository_GetNotFound(t *testing.T) {
	ctx := context.Background()
	r := NewSQLiteWebCacheRepository(newWebCacheTestDB(t))

	row, err := r.GetWebCache(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if row != nil {
		t.Fatalf("expected nil row, got: %+v", row)
	}
}

func TestWebCacheRepository_UpsertAndOverwrite(t *testing.T) {
	ctx := context.Background()
	r := NewSQLiteWebCacheRepository(newWebCacheTestDB(t))

	row1 := repo.WebCacheRow{
		URLHash:      "hash1",
		URL:          "https://example.com/test",
		ETag:         "\"etag1\"",
		LastModified: "Wed, 21 Oct 2026 07:28:00 GMT",
		ContentHash:  "chash1",
		Title:        "Title 1",
		Markdown:     "# Hello World",
		FetchedAt:    "2026-10-14T10:00:00Z",
	}

	if err := r.UpsertWebCache(ctx, row1); err != nil {
		t.Fatalf("UpsertWebCache failed: %v", err)
	}

	got, err := r.GetWebCache(ctx, "hash1")
	if err != nil {
		t.Fatalf("GetWebCache failed: %v", err)
	}
	if got == nil || got.URL != row1.URL || got.Markdown != row1.Markdown || got.ETag != row1.ETag {
		t.Fatalf("unexpected cached row: %+v", got)
	}

	// 覆盖写入相同 url_hash
	row2 := repo.WebCacheRow{
		URLHash:      "hash1",
		URL:          "https://example.com/test",
		ETag:         "\"etag2\"",
		LastModified: "Wed, 21 Oct 2026 08:00:00 GMT",
		ContentHash:  "chash2",
		Title:        "Title 2",
		Markdown:     "# Updated Content",
		FetchedAt:    "2026-10-14T11:00:00Z",
	}

	if err := r.UpsertWebCache(ctx, row2); err != nil {
		t.Fatalf("UpsertWebCache overwrite failed: %v", err)
	}

	got2, err := r.GetWebCache(ctx, "hash1")
	if err != nil {
		t.Fatalf("GetWebCache failed: %v", err)
	}
	if got2 == nil || got2.Title != "Title 2" || got2.Markdown != "# Updated Content" || got2.ETag != "\"etag2\"" {
		t.Fatalf("unexpected overwritten row: %+v", got2)
	}
}

func TestWebCacheRepository_TrimWebCache(t *testing.T) {
	ctx := context.Background()
	r := NewSQLiteWebCacheRepository(newWebCacheTestDB(t))

	// 插入 5 条不同 fetched_at 的记录
	rows := []repo.WebCacheRow{
		{URLHash: "h1", URL: "https://example.com/1", FetchedAt: "2026-10-14T01:00:00Z"},
		{URLHash: "h2", URL: "https://example.com/2", FetchedAt: "2026-10-14T02:00:00Z"},
		{URLHash: "h3", URL: "https://example.com/3", FetchedAt: "2026-10-14T03:00:00Z"},
		{URLHash: "h4", URL: "https://example.com/4", FetchedAt: "2026-10-14T04:00:00Z"},
		{URLHash: "h5", URL: "https://example.com/5", FetchedAt: "2026-10-14T05:00:00Z"},
	}

	for _, row := range rows {
		if err := r.UpsertWebCache(ctx, row); err != nil {
			t.Fatalf("UpsertWebCache failed: %v", err)
		}
	}

	// 保留最新 3 行（h3, h4, h5 应保留，h1, h2 应被删）
	if err := r.TrimWebCache(ctx, 3); err != nil {
		t.Fatalf("TrimWebCache failed: %v", err)
	}

	for _, h := range []string{"h1", "h2"} {
		row, err := r.GetWebCache(ctx, h)
		if err != nil {
			t.Fatalf("GetWebCache %s failed: %v", h, err)
		}
		if row != nil {
			t.Fatalf("expected %s to be trimmed, but found: %+v", h, row)
		}
	}

	for _, h := range []string{"h3", "h4", "h5"} {
		row, err := r.GetWebCache(ctx, h)
		if err != nil {
			t.Fatalf("GetWebCache %s failed: %v", h, err)
		}
		if row == nil {
			t.Fatalf("expected %s to be kept, but was nil", h)
		}
	}
}
