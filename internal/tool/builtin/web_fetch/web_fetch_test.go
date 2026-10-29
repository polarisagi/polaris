package web_fetch

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	repoimpl "github.com/polarisagi/polaris/internal/store/repo"
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

type fixtureCase struct {
	File           string   `yaml:"file"`
	BaseURL        string   `yaml:"base_url"`
	MustContain    []string `yaml:"must_contain"`
	MustNotContain []string `yaml:"must_not_contain"`
}

type fixtureConfig struct {
	Cases []fixtureCase `yaml:"cases"`
}

func TestWebFetch_Fixtures(t *testing.T) {
	testdataDir := filepath.Join("..", "..", "..", "..", "testdata", "webfetch")
	casesFile := filepath.Join(testdataDir, "cases.yaml")
	data, err := os.ReadFile(casesFile)
	if err != nil {
		t.Fatalf("failed to read %s: %v", casesFile, err)
	}

	var cfg fixtureConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("failed to unmarshal cases.yaml: %v", err)
	}

	dialer := &dummyDialer{}

	for _, c := range cfg.Cases {
		t.Run(c.File, func(t *testing.T) {
			filePath := filepath.Join(testdataDir, c.File)
			htmlBytes, readErr := os.ReadFile(filePath)
			if readErr != nil {
				t.Fatalf("failed to read fixture %s: %v", c.File, readErr)
			}

			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.Write(htmlBytes)
			}))
			defer ts.Close()

			fn := MakeWebFetchFn(Deps{
				Dialer: dialer,
			})

			input, _ := json.Marshal(map[string]any{
				"url":  ts.URL,
				"mode": "markdown",
			})

			out, err := fn(context.Background(), input)
			if err != nil {
				t.Fatalf("web_fetch failed: %v", err)
			}

			var res map[string]any
			if err := json.Unmarshal(out, &res); err != nil {
				t.Fatalf("unmarshal output failed: %v", err)
			}

			content, _ := res["content"].(string)

			for _, mc := range c.MustContain {
				if !strings.Contains(content, mc) {
					t.Errorf("fixture %s: expected content to contain %q\ncontent: %s", c.File, mc, content)
				}
			}

			for _, mnc := range c.MustNotContain {
				if strings.Contains(content, mnc) {
					t.Errorf("fixture %s: expected content NOT to contain %q", c.File, mnc)
				}
			}

			t.Logf("fixture %s: raw %d chars -> markdown %d chars", c.File, len(htmlBytes), len(content))
		})
	}
}

func TestWebFetch_URLValidationAndSSRF(t *testing.T) {
	dialer := &dummyDialer{}
	fn := MakeWebFetchFn(Deps{Dialer: dialer})
	ctx := context.Background()

	// 1. /v10.html should not be blocked by url validation
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html><body><h1>V10 Title</h1><p>Release v10 is out.</p></body></html>"))
	}))
	defer ts.Close()

	input, _ := json.Marshal(map[string]any{
		"url": ts.URL + "/v10.html",
	})
	out, err := fn(ctx, input)
	if err != nil {
		t.Fatalf("expected /v10.html to succeed, got: %v", err)
	}
	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if res["status"] != float64(200) {
		t.Errorf("expected status 200, got: %v", res["status"])
	}

	// 2. file:///etc/passwd -> CodeInvalidInput
	badInput, _ := json.Marshal(map[string]any{
		"url": "file:///etc/passwd",
	})
	_, err = fn(ctx, badInput)
	if err == nil || !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Errorf("expected CodeInvalidInput for file scheme, got: %v", err)
	}

	// 3. ftp scheme -> CodeInvalidInput
	badInput2, _ := json.Marshal(map[string]any{
		"url": "ftp://example.com/file.txt",
	})
	_, err = fn(ctx, badInput2)
	if err == nil || !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Errorf("expected CodeInvalidInput for ftp scheme, got: %v", err)
	}

	// 4. empty url -> CodeInvalidInput
	badInput3, _ := json.Marshal(map[string]any{
		"url": "",
	})
	_, err = fn(ctx, badInput3)
	if err == nil || !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Errorf("expected CodeInvalidInput for empty url, got: %v", err)
	}
}

func TestWebFetch_ConditionalCacheAndETag(t *testing.T) {
	cache := newTestWebCacheRepo(t)
	dialer := &dummyDialer{}
	fn := MakeWebFetchFn(Deps{
		Dialer: dialer,
		Cache:  cache,
	})
	ctx := context.Background()

	var reqCount int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&reqCount, 1)
		if count == 1 {
			w.Header().Set("ETag", `"etag-123"`)
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html><body><article><h1>ETag Test</h1><p>Cached article content.</p></article></body></html>"))
			return
		}
		if r.Header.Get("If-None-Match") == `"etag-123"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer ts.Close()

	// First request: 200 OK, sets ETag, unchanged=false
	input, _ := json.Marshal(map[string]any{"url": ts.URL})
	out1, err := fn(ctx, input)
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}
	var res1 map[string]any
	if err := json.Unmarshal(out1, &res1); err != nil {
		t.Fatal(err)
	}
	if res1["status"] != float64(200) || res1["unchanged"] != false {
		t.Errorf("expected status 200 unchanged=false, got %+v", res1)
	}

	// Second request: 304 Not Modified, returns cached markdown, unchanged=true
	out2, err := fn(ctx, input)
	if err != nil {
		t.Fatalf("second request failed: %v", err)
	}
	var res2 map[string]any
	if err := json.Unmarshal(out2, &res2); err != nil {
		t.Fatal(err)
	}
	if res2["status"] != float64(304) || res2["unchanged"] != true {
		t.Errorf("expected status 304 unchanged=true, got %+v", res2)
	}
	content2, _ := res2["content"].(string)
	if !strings.Contains(content2, "Cached article content") {
		t.Errorf("expected cached content to be preserved, got: %s", content2)
	}
}

func TestWebFetch_UnchangedContentHash(t *testing.T) {
	cache := newTestWebCacheRepo(t)
	dialer := &dummyDialer{}
	fn := MakeWebFetchFn(Deps{
		Dialer: dialer,
		Cache:  cache,
	})
	ctx := context.Background()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html><body><h1>Title</h1><p>Same static content every time.</p></body></html>"))
	}))
	defer ts.Close()

	input, _ := json.Marshal(map[string]any{"url": ts.URL})
	out1, err := fn(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	var res1 map[string]any
	json.Unmarshal(out1, &res1)
	if res1["unchanged"] != false {
		t.Errorf("expected first call unchanged=false, got %v", res1["unchanged"])
	}

	out2, err := fn(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	var res2 map[string]any
	json.Unmarshal(out2, &res2)
	if res2["unchanged"] != true {
		t.Errorf("expected second call unchanged=true via content_hash, got %v", res2["unchanged"])
	}
}

func TestWebFetch_Pagination(t *testing.T) {
	dialer := &dummyDialer{}
	fn := MakeWebFetchFn(Deps{Dialer: dialer})
	ctx := context.Background()

	longText := strings.Repeat("ABCDEFGHIJ", 100) // 1000 runes
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(longText))
	}))
	defer ts.Close()

	// First page: max_chars=500, offset=0
	input1, _ := json.Marshal(map[string]any{
		"url":       ts.URL,
		"mode":      "text",
		"max_chars": 500,
		"offset":    0,
	})
	out1, err := fn(ctx, input1)
	if err != nil {
		t.Fatal(err)
	}
	var res1 map[string]any
	json.Unmarshal(out1, &res1)
	if res1["next_offset"] != float64(500) || res1["truncated"] != true {
		t.Errorf("expected next_offset=500 truncated=true, got: %+v", res1)
	}
	content1, _ := res1["content"].(string)
	if len([]rune(content1)) != 500 {
		t.Errorf("expected 500 runes, got %d", len([]rune(content1)))
	}

	// Second page: max_chars=500, offset=500
	input2, _ := json.Marshal(map[string]any{
		"url":       ts.URL,
		"mode":      "text",
		"max_chars": 500,
		"offset":    500,
	})
	out2, err := fn(ctx, input2)
	if err != nil {
		t.Fatal(err)
	}
	var res2 map[string]any
	json.Unmarshal(out2, &res2)
	if res2["next_offset"] != float64(0) || res2["truncated"] != false {
		t.Errorf("expected next_offset=0 truncated=false, got: %+v", res2)
	}
	content2, _ := res2["content"].(string)
	if len([]rune(content2)) != 500 {
		t.Errorf("expected 500 runes, got %d", len([]rune(content2)))
	}
	if content1+content2 != longText {
		t.Errorf("combined pages do not equal full text")
	}
}

func TestWebFetch_ModeLinks(t *testing.T) {
	dialer := &dummyDialer{}
	fn := MakeWebFetchFn(Deps{Dialer: dialer})
	ctx := context.Background()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`
			<html><body>
				<a href="/about">About Us</a>
				<a href="https://example.com/external">External</a>
				<a href="/about">About Us Duplicate</a>
				<a href="javascript:alert(1)">Ignore JS</a>
				<a href="#section">Ignore Fragment</a>
			</body></html>
		`))
	}))
	defer ts.Close()

	input, _ := json.Marshal(map[string]any{
		"url":  ts.URL,
		"mode": "links",
	})
	out, err := fn(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	var res map[string]any
	json.Unmarshal(out, &res)
	content, _ := res["content"].(string)

	expectedAbout := fmt.Sprintf("- [About Us](%s/about)", ts.URL)
	if !strings.Contains(content, expectedAbout) {
		t.Errorf("expected content to contain %q, got: %s", expectedAbout, content)
	}
	if !strings.Contains(content, "- [External](https://example.com/external)") {
		t.Errorf("expected external link, got: %s", content)
	}
	if strings.Contains(content, "javascript:") {
		t.Errorf("expected no javascript link, got: %s", content)
	}
	if strings.Count(content, ts.URL+"/about") != 1 {
		t.Errorf("expected exactly 1 link for /about, got content: %s", content)
	}
}

func TestWebFetch_NonHTML(t *testing.T) {
	dialer := &dummyDialer{}
	fn := MakeWebFetchFn(Deps{Dialer: dialer})
	ctx := context.Background()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A})
	}))
	defer ts.Close()

	input, _ := json.Marshal(map[string]any{"url": ts.URL})
	out, err := fn(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	var res map[string]any
	json.Unmarshal(out, &res)

	if res["content_type"] != "image/png" {
		t.Errorf("expected content_type image/png, got %v", res["content_type"])
	}
	if res["content"] != "" {
		t.Errorf("expected empty content for binary image, got %v", res["content"])
	}
}

func TestWebFetch_FeedHint(t *testing.T) {
	dialer := &dummyDialer{}
	fn := MakeWebFetchFn(Deps{Dialer: dialer})
	ctx := context.Background()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Write([]byte(`<rss version="2.0"><channel><title>Feed</title></channel></rss>`))
	}))
	defer ts.Close()

	input, _ := json.Marshal(map[string]any{"url": ts.URL})
	out, err := fn(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	var res map[string]any
	json.Unmarshal(out, &res)
	content, _ := res["content"].(string)

	if !strings.Contains(content, "这是 feed，请用 feed_fetch") {
		t.Errorf("expected feed hint, got: %s", content)
	}
}

func TestWebFetch_RetryAfter(t *testing.T) {
	dialer := &dummyDialer{}
	fn := MakeWebFetchFn(Deps{Dialer: dialer})
	ctx := context.Background()

	var attempts int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a := atomic.AddInt32(&attempts, 1)
		if a == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("Retry success!"))
	}))
	defer ts.Close()

	input, _ := json.Marshal(map[string]any{"url": ts.URL, "mode": "text"})
	out, err := fn(ctx, input)
	if err != nil {
		t.Fatalf("expected retry to succeed, got: %v", err)
	}
	var res map[string]any
	json.Unmarshal(out, &res)
	if res["status"] != float64(200) || res["content"] != "Retry success!" {
		t.Errorf("unexpected result after retry: %+v", res)
	}

	// Retry-After > 60s should return CodeNetworkUnavailable
	tsOver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer tsOver.Close()

	inputOver, _ := json.Marshal(map[string]any{"url": tsOver.URL})
	_, err = fn(ctx, inputOver)
	if err == nil || !apperr.IsCode(err, apperr.CodeNetworkUnavailable) {
		t.Errorf("expected CodeNetworkUnavailable for Retry-After > 60, got: %v", err)
	}
}

func TestWebFetch_HostLimiter(t *testing.T) {
	dialer := &dummyDialer{}
	limiter := NewHostLimiter(func() time.Duration {
		return 60 * time.Millisecond
	})
	fn := MakeWebFetchFn(Deps{
		Dialer:  dialer,
		Limiter: limiter,
	})
	ctx := context.Background()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("ok"))
	}))
	defer ts.Close()

	input, _ := json.Marshal(map[string]any{"url": ts.URL, "mode": "text"})

	start := time.Now()
	if _, err := fn(ctx, input); err != nil {
		t.Fatal(err)
	}
	if _, err := fn(ctx, input); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)

	if elapsed < 50*time.Millisecond {
		t.Errorf("expected at least ~60ms between requests, got: %v", elapsed)
	}
}

func TestWebFetch_Selector(t *testing.T) {
	dialer := &dummyDialer{}
	fn := MakeWebFetchFn(Deps{Dialer: dialer})
	ctx := context.Background()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`
			<html><body>
				<div class="sidebar"><p>Sidebar advertisement</p></div>
				<div id="content"><article><h1>Article</h1><p>Main content here</p></article></div>
			</body></html>
		`))
	}))
	defer ts.Close()

	// 1. Valid selector #content
	input, _ := json.Marshal(map[string]any{
		"url":      ts.URL,
		"mode":     "markdown",
		"selector": "#content",
	})
	out, err := fn(ctx, input)
	if err != nil {
		t.Fatalf("selector query failed: %v", err)
	}
	var res map[string]any
	json.Unmarshal(out, &res)
	content, _ := res["content"].(string)

	if !strings.Contains(content, "Main content here") {
		t.Errorf("expected selected content, got: %s", content)
	}
	if strings.Contains(content, "Sidebar advertisement") {
		t.Errorf("expected sidebar to be excluded by selector, got: %s", content)
	}

	// 2. Selector not found -> CodeNotFound
	inputNotFound, _ := json.Marshal(map[string]any{
		"url":      ts.URL,
		"selector": "#nonexistent",
	})
	_, err = fn(ctx, inputNotFound)
	if err == nil || !apperr.IsCode(err, apperr.CodeNotFound) {
		t.Errorf("expected CodeNotFound for missing selector, got: %v", err)
	}

	// 3. Complex unsupported selector -> CodeInvalidInput
	inputBadSel, _ := json.Marshal(map[string]any{
		"url":      ts.URL,
		"selector": "div > p",
	})
	_, err = fn(ctx, inputBadSel)
	if err == nil || !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Errorf("expected CodeInvalidInput for complex selector, got: %v", err)
	}
}
