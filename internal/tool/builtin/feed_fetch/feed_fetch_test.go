package feed_fetch

import (
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	repoimpl "github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/internal/tool/builtin/web_fetch"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type dummyDialer struct {
	net.Dialer
}

func newTestWebCacheRepo(t *testing.T) repo.WebCacheRepository {
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
	return repoimpl.NewSQLiteWebCacheRepository(db)
}

func readFeedFixture(t *testing.T, filename string) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "testdata", "feeds", filename)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read feed fixture %s: %v", filename, err)
	}
	return data
}

func TestFeedFetch_RSS(t *testing.T) {
	fixtureData := readFeedFixture(t, "rss.xml")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Write(fixtureData)
	}))
	defer ts.Close()

	fn := MakeFeedFetchFn(web_fetch.Deps{Dialer: &dummyDialer{}})
	input, _ := json.Marshal(map[string]any{"url": ts.URL})

	out, err := fn(context.Background(), input)
	if err != nil {
		t.Fatalf("feed_fetch failed: %v", err)
	}

	var res struct {
		FeedTitle string     `json:"feed_title"`
		Items     []FeedItem `json:"items"`
		Count     int        `json:"count"`
		Unchanged bool       `json:"unchanged"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if res.FeedTitle != "Go News" {
		t.Errorf("expected feed_title 'Go News', got %q", res.FeedTitle)
	}
	if res.Count != 2 || len(res.Items) != 2 {
		t.Fatalf("expected 2 items, got count=%d items=%d", res.Count, len(res.Items))
	}

	item0 := res.Items[0]
	if item0.Title != "Go 1.23 Released" {
		t.Errorf("expected item title 'Go 1.23 Released', got %q", item0.Title)
	}
	if item0.URL != "https://go.dev/blog/go1.23" {
		t.Errorf("expected relative link resolved to 'https://go.dev/blog/go1.23', got %q", item0.URL)
	}
	if !strings.Contains(item0.Excerpt, "Go 1.23") || !strings.Contains(item0.Excerpt, "& more details.") {
		t.Errorf("expected HTML stripped and entities decoded in excerpt, got %q", item0.Excerpt)
	}
}

func TestFeedFetch_Atom(t *testing.T) {
	fixtureData := readFeedFixture(t, "atom.xml")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/atom+xml")
		w.Write(fixtureData)
	}))
	defer ts.Close()

	fn := MakeFeedFetchFn(web_fetch.Deps{Dialer: &dummyDialer{}})
	input, _ := json.Marshal(map[string]any{"url": ts.URL})

	out, err := fn(context.Background(), input)
	if err != nil {
		t.Fatalf("feed_fetch failed: %v", err)
	}

	var res struct {
		FeedTitle string     `json:"feed_title"`
		Items     []FeedItem `json:"items"`
		Count     int        `json:"count"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if res.FeedTitle != "Example Feed" || res.Count != 1 {
		t.Fatalf("unexpected feed response: %+v", res)
	}
	item := res.Items[0]
	if item.Author != "John Doe" {
		t.Errorf("expected author 'John Doe', got %q", item.Author)
	}
	if item.URL != "https://example.com/posts/atom-1" {
		t.Errorf("expected resolved URL 'https://example.com/posts/atom-1', got %q", item.URL)
	}
	if item.Excerpt != "Summary of entry 1 with tags" {
		t.Errorf("expected excerpt without html tags, got %q", item.Excerpt)
	}
}

func TestFeedFetch_JSONFeed(t *testing.T) {
	fixtureData := readFeedFixture(t, "feed.json")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/feed+json")
		w.Write(fixtureData)
	}))
	defer ts.Close()

	fn := MakeFeedFetchFn(web_fetch.Deps{Dialer: &dummyDialer{}})
	input, _ := json.Marshal(map[string]any{"url": ts.URL})

	out, err := fn(context.Background(), input)
	if err != nil {
		t.Fatalf("feed_fetch failed: %v", err)
	}

	var res struct {
		FeedTitle string     `json:"feed_title"`
		Items     []FeedItem `json:"items"`
		Count     int        `json:"count"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if res.FeedTitle != "JSON Feed Example" || res.Count != 1 {
		t.Fatalf("unexpected feed response: %+v", res)
	}
	item := res.Items[0]
	if item.Title != "First Item" || item.URL != "https://example.org/initial-post" {
		t.Errorf("unexpected item: %+v", item)
	}
}

func TestFeedFetch_SinceFilter(t *testing.T) {
	fixtureData := readFeedFixture(t, "rss.xml")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Write(fixtureData)
	}))
	defer ts.Close()

	fn := MakeFeedFetchFn(web_fetch.Deps{Dialer: &dummyDialer{}})

	// Filter since May 2024: only the Aug 2024 item should be kept
	input, _ := json.Marshal(map[string]any{
		"url":   ts.URL,
		"since": "2024-05-01T00:00:00Z",
	})

	out, err := fn(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}

	var res struct {
		Items []FeedItem `json:"items"`
		Count int        `json:"count"`
	}
	json.Unmarshal(out, &res)

	if res.Count != 1 || len(res.Items) != 1 {
		t.Fatalf("expected 1 item after since filter, got count=%d", res.Count)
	}
	if res.Items[0].Title != "Go 1.23 Released" {
		t.Errorf("expected 'Go 1.23 Released', got %q", res.Items[0].Title)
	}
}

func TestFeedFetch_Conditional304(t *testing.T) {
	cache := newTestWebCacheRepo(t)
	dialer := &dummyDialer{}
	fn := MakeFeedFetchFn(web_fetch.Deps{
		Dialer: dialer,
		Cache:  cache,
	})
	ctx := context.Background()

	fixtureData := readFeedFixture(t, "rss.xml")
	var reqCount int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := atomic.AddInt32(&reqCount, 1)
		if c == 1 {
			w.Header().Set("ETag", `"feed-etag-999"`)
			w.Header().Set("Content-Type", "application/rss+xml")
			w.Write(fixtureData)
			return
		}
		if r.Header.Get("If-None-Match") == `"feed-etag-999"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer ts.Close()

	input, _ := json.Marshal(map[string]any{"url": ts.URL})

	// 1st call: 200 OK
	out1, err := fn(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	var res1 map[string]any
	json.Unmarshal(out1, &res1)
	if res1["unchanged"] != false || res1["count"] != float64(2) {
		t.Errorf("unexpected 1st call result: %+v", res1)
	}

	// 2nd call: 304 Not Modified
	out2, err := fn(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	var res2 map[string]any
	json.Unmarshal(out2, &res2)
	if res2["unchanged"] != true || res2["count"] != float64(0) {
		t.Errorf("unexpected 2nd call result (expected 304 unchanged=true): %+v", res2)
	}
}

func TestFeedFetch_MaxItemsAndExcerptTruncation(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		longDesc := strings.Repeat("Very long description text. ", 100) // ~2800 chars
		rss := `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Test</title>
    <item><title>Item 1</title><link>https://example.com/1</link><description>` + longDesc + `</description></item>
    <item><title>Item 2</title><link>https://example.com/2</link><description>Short</description></item>
  </channel>
</rss>`
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Write([]byte(rss))
	}))
	defer ts.Close()

	fn := MakeFeedFetchFn(web_fetch.Deps{Dialer: &dummyDialer{}})
	input, _ := json.Marshal(map[string]any{
		"url":       ts.URL,
		"max_items": 1,
	})

	out, err := fn(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}

	var res struct {
		Items []FeedItem `json:"items"`
		Count int        `json:"count"`
	}
	json.Unmarshal(out, &res)

	if res.Count != 1 {
		t.Fatalf("expected count 1, got %d", res.Count)
	}
	if len([]rune(res.Items[0].Excerpt)) > 500 {
		t.Errorf("expected excerpt <= 500 runes, got %d", len([]rune(res.Items[0].Excerpt)))
	}
}

func TestFeedFetch_URLValidation(t *testing.T) {
	fn := MakeFeedFetchFn(web_fetch.Deps{Dialer: &dummyDialer{}})
	ctx := context.Background()

	// file:/// -> CodeInvalidInput
	input1, _ := json.Marshal(map[string]any{"url": "file:///etc/passwd"})
	_, err1 := fn(ctx, input1)
	if err1 == nil || !apperr.IsCode(err1, apperr.CodeInvalidInput) {
		t.Errorf("expected CodeInvalidInput for file scheme, got: %v", err1)
	}

	// empty url -> CodeInvalidInput
	input2, _ := json.Marshal(map[string]any{"url": ""})
	_, err2 := fn(ctx, input2)
	if err2 == nil || !apperr.IsCode(err2, apperr.CodeInvalidInput) {
		t.Errorf("expected CodeInvalidInput for empty url, got: %v", err2)
	}
}
