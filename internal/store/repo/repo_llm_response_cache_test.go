package repo

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	_ "modernc.org/sqlite"

	protorepo "github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/internal/protocol/schema"
)

func newResponseCacheTestRepo(t *testing.T) *SQLiteLLMResponseCacheRepository {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	ddl, err := schema.FS.ReadFile("047_llm_response_cache.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatal(err)
	}
	return NewSQLiteLLMResponseCacheRepository(db, db)
}

func entry(key string, createdMs, expiresMs int64) protorepo.LLMCacheEntry {
	return protorepo.LLMCacheEntry{Key: key, Purpose: "graphrag_extract", Provider: "p", Model: "m",
		Response: `{"Content":"x"}`, CreatedAtMs: createdMs, ExpiresAtMs: expiresMs}
}

func TestLLMResponseCacheRepository_GetPutExpiry(t *testing.T) {
	r := newResponseCacheTestRepo(t)
	ctx := context.Background()

	if got, err := r.Get(ctx, "k1", 100); err != nil || got != nil {
		t.Fatalf("空表应未命中: %v %v", got, err)
	}
	if err := r.Put(ctx, entry("k1", 100, 200)); err != nil {
		t.Fatal(err)
	}
	got, err := r.Get(ctx, "k1", 150)
	if err != nil || got == nil || got.Response != `{"Content":"x"}` || got.Provider != "p" {
		t.Fatalf("期望命中: %+v %v", got, err)
	}
	// expires_at 是开区间上界：now == expires_at 即视为过期。
	if got, _ := r.Get(ctx, "k1", 200); got != nil {
		t.Fatalf("now==expires_at 应视为过期: %+v", got)
	}
	if err := r.IncrHit(ctx, "k1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Get(ctx, "k1", 150); got == nil || got.HitCount != 1 {
		t.Fatalf("hit_count 应为 1: %+v", got)
	}
	// 覆盖写：命中计数归零。
	if err := r.Put(ctx, entry("k1", 120, 300)); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Get(ctx, "k1", 150); got == nil || got.HitCount != 0 || got.ExpiresAtMs != 300 {
		t.Fatalf("覆盖写后应重置: %+v", got)
	}
}

func TestLLMResponseCacheRepository_DeleteExpired(t *testing.T) {
	r := newResponseCacheTestRepo(t)
	ctx := context.Background()
	_ = r.Put(ctx, entry("old", 1, 50))
	_ = r.Put(ctx, entry("live", 1, 500))
	n, err := r.DeleteExpired(ctx, 100)
	if err != nil || n != 1 {
		t.Fatalf("应删除 1 行过期: n=%d err=%v", n, err)
	}
	if got, _ := r.Get(ctx, "live", 100); got == nil {
		t.Fatal("未过期行不应被删除")
	}
}

func TestLLMResponseCacheRepository_EvictOverflowKeepsNewest(t *testing.T) {
	r := newResponseCacheTestRepo(t)
	ctx := context.Background()
	for i := 1; i <= 10; i++ {
		if err := r.Put(ctx, entry(fmt.Sprintf("k%02d", i), int64(i), 1000)); err != nil {
			t.Fatal(err)
		}
	}
	n, err := r.EvictOverflow(ctx, 4)
	if err != nil || n != 6 {
		t.Fatalf("应淘汰 6 行: n=%d err=%v", n, err)
	}
	for i := 1; i <= 6; i++ {
		if got, _ := r.Get(ctx, fmt.Sprintf("k%02d", i), 0); got != nil {
			t.Fatalf("k%02d 是最旧的，应被淘汰", i)
		}
	}
	for i := 7; i <= 10; i++ {
		if got, _ := r.Get(ctx, fmt.Sprintf("k%02d", i), 0); got == nil {
			t.Fatalf("k%02d 是最新的，应保留", i)
		}
	}
	if n, _ := r.EvictOverflow(ctx, 0); n != 0 {
		t.Fatal("maxEntries<=0 必须是空操作，不得清空整表")
	}
}
