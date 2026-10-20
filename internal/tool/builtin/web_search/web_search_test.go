package web_search

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/tool/builtin/web_fetch"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type dummyDialer struct {
	net.Dialer
}

func readDDGFixture(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "testdata", "websearch", "ddg.html")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read ddg fixture: %v", err)
	}
	return data
}

func TestWebSearch_DDG(t *testing.T) {
	fixtureData := readDDGFixture(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(fixtureData)
	}))
	defer ts.Close()

	cfg := &config.Config{}
	cfg.Agent.Skill.WebSearchEngine = "duckduckgo"
	cfg.Agent.Skill.WebSearchBaseURL = ts.URL

	fn := MakeWebSearchFn(cfg, &dummyDialer{})
	input, _ := json.Marshal(map[string]any{"query": "golang", "max_results": 5})

	out, err := fn(context.Background(), input)
	if err != nil {
		t.Fatalf("web_search ddg failed: %v", err)
	}

	var res struct {
		Engine  string   `json:"engine"`
		Results []result `json:"results"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if res.Engine != "duckduckgo" {
		t.Errorf("expected engine duckduckgo, got %q", res.Engine)
	}
	// 期望 2 个结果（广告块已被跳过）
	if len(res.Results) != 2 {
		t.Fatalf("expected 2 results (ad skipped), got %d", len(res.Results))
	}

	r0 := res.Results[0]
	if r0.Title != "Go 1.23 is released!" {
		t.Errorf("expected title 'Go 1.23 is released!', got %q", r0.Title)
	}
	if r0.URL != "https://go.dev/blog/go1.23" {
		t.Errorf("expected uddg link unquoted to 'https://go.dev/blog/go1.23', got %q", r0.URL)
	}
	if !strings.Contains(r0.Snippet, "Go 1.23") || !strings.Contains(r0.Snippet, "& more details.") {
		t.Errorf("expected snippet HTML tags removed and entities unescaped, got %q", r0.Snippet)
	}

	r1 := res.Results[1]
	if r1.Title != "Documentation - The Go Programming Language" {
		t.Errorf("expected title 'Documentation - The Go Programming Language', got %q", r1.Title)
	}
	if r1.URL != "https://golang.org/doc/" {
		t.Errorf("expected direct url 'https://golang.org/doc/', got %q", r1.URL)
	}
}

func TestWebSearch_SearXNG(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/search") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("format") != "json" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		resp := map[string]any{
			"results": []map[string]any{
				{
					"title":   "SearXNG Item 1",
					"url":     "https://searx.example.org/1",
					"content": "Content of item 1",
				},
				{
					"title":   "SearXNG Item 2",
					"url":     "https://searx.example.org/2",
					"content": "Content of item 2",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	cfg := &config.Config{}
	cfg.Agent.Skill.WebSearchEngine = "searxng"
	cfg.Agent.Skill.WebSearchBaseURL = ts.URL

	fn := MakeWebSearchFn(cfg, &dummyDialer{})
	// max_results 限制为 1
	input, _ := json.Marshal(map[string]any{"query": "search query", "max_results": 1})

	out, err := fn(context.Background(), input)
	if err != nil {
		t.Fatalf("web_search searxng failed: %v", err)
	}

	var res struct {
		Engine  string   `json:"engine"`
		Results []result `json:"results"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if res.Engine != "searxng" {
		t.Errorf("expected engine searxng, got %q", res.Engine)
	}
	if len(res.Results) != 1 {
		t.Fatalf("expected 1 result due to max_results=1, got %d", len(res.Results))
	}
	if res.Results[0].Title != "SearXNG Item 1" || res.Results[0].URL != "https://searx.example.org/1" {
		t.Errorf("unexpected item: %+v", res.Results[0])
	}
}

func TestWebSearch_Brave(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Subscription-Token") != "test-brave-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		resp := map[string]any{
			"web": map[string]any{
				"results": []map[string]any{
					{
						"title":       "Brave Search 1",
						"url":         "https://brave.example.com/1",
						"description": "Brave snippet <b>highlighted</b> &amp; cleaned",
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	cfg := &config.Config{}
	cfg.Agent.Skill.WebSearchEngine = "brave"
	cfg.Agent.Skill.WebSearchAPIKey = "test-brave-key"
	cfg.Agent.Skill.WebSearchBaseURL = ts.URL

	fn := MakeWebSearchFn(cfg, &dummyDialer{})
	input, _ := json.Marshal(map[string]any{"query": "brave query"})

	out, err := fn(context.Background(), input)
	if err != nil {
		t.Fatalf("web_search brave failed: %v", err)
	}

	var res struct {
		Engine  string   `json:"engine"`
		Results []result `json:"results"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if res.Engine != "brave" {
		t.Errorf("expected engine brave, got %q", res.Engine)
	}
	if len(res.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(res.Results))
	}
	if res.Results[0].Snippet != "Brave snippet highlighted & cleaned" {
		t.Errorf("expected snippet HTML stripped and entity decoded, got %q", res.Results[0].Snippet)
	}
}

func TestWebSearch_Fallback(t *testing.T) {
	fixtureData := readDDGFixture(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(fixtureData)
	}))
	defer ts.Close()

	t.Run("unknown engine falls back to duckduckgo", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Agent.Skill.WebSearchEngine = "google_nonexistent"
		cfg.Agent.Skill.WebSearchBaseURL = ts.URL

		fn := MakeWebSearchFn(cfg, &dummyDialer{})
		input, _ := json.Marshal(map[string]any{"query": "test"})
		out, err := fn(context.Background(), input)
		if err != nil {
			t.Fatal(err)
		}
		var res struct {
			Engine string `json:"engine"`
		}
		json.Unmarshal(out, &res)
		if res.Engine != "duckduckgo" {
			t.Errorf("expected fallback to duckduckgo, got %q", res.Engine)
		}
	})

	t.Run("brave missing key falls back to duckduckgo", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Agent.Skill.WebSearchEngine = "brave"
		cfg.Agent.Skill.WebSearchAPIKey = ""
		cfg.Agent.Skill.WebSearchBaseURL = ts.URL

		fn := MakeWebSearchFn(cfg, &dummyDialer{})
		input, _ := json.Marshal(map[string]any{"query": "test"})
		out, err := fn(context.Background(), input)
		if err != nil {
			t.Fatal(err)
		}
		var res struct {
			Engine string `json:"engine"`
		}
		json.Unmarshal(out, &res)
		if res.Engine != "duckduckgo" {
			t.Errorf("expected fallback to duckduckgo, got %q", res.Engine)
		}
	})

	t.Run("searxng missing base url falls back to duckduckgo", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Agent.Skill.WebSearchEngine = "searxng"
		cfg.Agent.Skill.WebSearchBaseURL = ""

		eng, engName := pickEngine(cfg)
		if engName != "duckduckgo" {
			t.Errorf("expected duckduckgo fallback, got %q", engName)
		}
		if _, ok := eng.(*ddg); !ok {
			t.Errorf("expected *ddg instance, got %T", eng)
		}
	})
}

func TestWebSearch_Validation(t *testing.T) {
	fn := MakeWebSearchFn(&config.Config{}, &dummyDialer{})
	ctx := context.Background()

	// empty query -> CodeInvalidInput
	inputEmpty, _ := json.Marshal(map[string]any{"query": "   "})
	_, errEmpty := fn(ctx, inputEmpty)
	if errEmpty == nil || !apperr.IsCode(errEmpty, apperr.CodeInvalidInput) {
		t.Errorf("expected CodeInvalidInput for empty query, got %v", errEmpty)
	}

	// query > 500 chars -> CodeInvalidInput
	longQuery := strings.Repeat("a", 501)
	inputLong, _ := json.Marshal(map[string]any{"query": longQuery})
	_, errLong := fn(ctx, inputLong)
	if errLong == nil || !apperr.IsCode(errLong, apperr.CodeInvalidInput) {
		t.Errorf("expected CodeInvalidInput for query > 500 chars, got %v", errLong)
	}
}

func TestWebSearch_HostLimiter(t *testing.T) {
	fixtureData := readDDGFixture(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(fixtureData)
	}))
	defer ts.Close()

	var limiterCalls int32
	limiter := web_fetch.NewHostLimiter(func() time.Duration {
		atomic.AddInt32(&limiterCalls, 1)
		return 10 * time.Millisecond
	})

	cfg := &config.Config{}
	cfg.Agent.Skill.WebSearchEngine = "duckduckgo"
	cfg.Agent.Skill.WebSearchBaseURL = ts.URL

	fn := MakeWebSearchFn(cfg, &dummyDialer{}, limiter)
	input, _ := json.Marshal(map[string]any{"query": "test query"})

	_, err := fn(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}

	if atomic.LoadInt32(&limiterCalls) == 0 {
		t.Errorf("expected limiter interval provider to be called, got 0")
	}
}

// C：DDG 返回 202（反爬/限流）→ 短退避后重试 1 次；第二次 200 则成功。
func TestDDG_Status202RetriesOnce(t *testing.T) {
	fixtureData := readDDGFixture(t)
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Write(fixtureData)
	}))
	defer ts.Close()

	d := &ddg{endpoint: ts.URL, retryDelay: time.Millisecond}
	res, err := d.search(context.Background(), ts.Client(), "golang", 5)
	if err != nil || len(res) == 0 {
		t.Fatalf("202 后重试应成功: res=%d err=%v", len(res), err)
	}
	if hits.Load() != 2 {
		t.Fatalf("应恰好请求 2 次，实际 %d", hits.Load())
	}
}

// 持续 202：只重试 1 次（共 2 次请求），返回可识别的限流错误，而非空结果或无关错误。
func TestDDG_Status202PersistentIsRateLimited(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer ts.Close()

	d := &ddg{endpoint: ts.URL, retryDelay: time.Millisecond}
	_, err := d.search(context.Background(), ts.Client(), "golang", 5)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("应返回限流错误，got %v", err)
	}
	if hits.Load() != 2 {
		t.Fatalf("应恰好请求 2 次，实际 %d", hits.Load())
	}
}

type stubEngine struct {
	res []result
	err error
	n   int
}

func (s *stubEngine) search(context.Context, *http.Client, string, int) ([]result, error) {
	s.n++
	return s.res, s.err
}

// 主引擎限流且配置了其他引擎 → 降级成功并报告实际引擎名。
func TestSearchWithFallback_DegradesToOtherEngine(t *testing.T) {
	primary := &stubEngine{err: ErrRateLimited}
	backup := &stubEngine{res: []result{{Title: "t", URL: "https://x"}}}
	res, name, err := searchWithFallback(context.Background(), nil,
		namedEngine{"duckduckgo", primary}, []namedEngine{{"brave", backup}}, "q", 5)
	if err != nil || name != "brave" || len(res) != 1 {
		t.Fatalf("应降级到 brave: res=%v name=%q err=%v", res, name, err)
	}
}

// 全部失败 → 结构化错误，列出各引擎原因；无降级配置时只含主引擎。
func TestSearchWithFallback_AllFailStructuredError(t *testing.T) {
	primary := &stubEngine{err: ErrRateLimited}
	_, _, err := searchWithFallback(context.Background(), nil, namedEngine{"duckduckgo", primary}, nil, "q", 5)
	if err == nil || !strings.Contains(err.Error(), "duckduckgo") || !strings.Contains(err.Error(), "web_search 失败") {
		t.Fatalf("应返回含引擎原因的结构化错误: %v", err)
	}
	if !apperr.IsCode(err, apperr.CodeResourceExhausted) {
		t.Fatalf("限流应映射 ResourceExhausted: %v", err)
	}
}

// fallbackEngines 只启用配置里已有凭据的引擎，不凭空启用。
func TestFallbackEngines_OnlyConfigured(t *testing.T) {
	names := func(es []namedEngine) []string {
		var out []string
		for _, e := range es {
			out = append(out, e.name)
		}
		return out
	}
	eq := func(got []namedEngine, want ...string) {
		t.Helper()
		g := names(got)
		if strings.Join(g, ",") != strings.Join(want, ",") {
			t.Fatalf("降级链 = %v，期望 %v", g, want)
		}
	}
	eq(fallbackEngines(&config.Config{}, "duckduckgo"), "bing")
	cfg := &config.Config{}
	cfg.Agent.Skill.WebSearchAPIKey = "k"
	eq(fallbackEngines(cfg, "duckduckgo"), "brave", "bing")
	eq(fallbackEngines(cfg, "brave"), "duckduckgo", "bing")
	eq(fallbackEngines(&config.Config{}, "bing"), "duckduckgo")
}

func readBingFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "testdata", "websearch", "bing.html"))
	if err != nil {
		t.Fatalf("读取 bing 夹具失败: %v", err)
	}
	return data
}

func TestParseBingHTML_Fixture(t *testing.T) {
	res, err := parseBingHTML(readBingFixture(t), 10)
	if err != nil || len(res) == 0 {
		t.Fatalf("应解析出结果: n=%d err=%v", len(res), err)
	}
	for _, r := range res {
		if r.Title == "" || !strings.HasPrefix(r.URL, "http") {
			t.Fatalf("结果不完整: %+v", r)
		}
		if strings.Contains(r.URL, "bing.com/ck/a") {
			t.Fatalf("跟踪链接未还原: %s", r.URL)
		}
	}
	if limited, _ := parseBingHTML(readBingFixture(t), 1); len(limited) != 1 {
		t.Fatalf("n=1 应只返回 1 条，实际 %d", len(limited))
	}
}

func TestUnwrapBingLink(t *testing.T) {
	// "a1" + base64url("https://go.dev/doc/")
	wrapped := "https://www.bing.com/ck/a?!&&p=x&u=a1aHR0cHM6Ly9nby5kZXYvZG9jLw&ntb=1"
	if got := unwrapBingLink(wrapped); got != "https://go.dev/doc/" {
		t.Fatalf("还原失败: %q", got)
	}
	if got := unwrapBingLink("https://example.com/a"); got != "https://example.com/a" {
		t.Fatalf("直链应原样返回: %q", got)
	}
	for _, bad := range []string{"", "javascript:void(0)", "https://www.bing.com/ck/a?u=zz", "https://www.bing.com/ck/a?u=a1!!!"} {
		if got := unwrapBingLink(bad); got != "" {
			t.Fatalf("%q 应丢弃，得到 %q", bad, got)
		}
	}
}

func TestBing_RateLimitedStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ts.Close()
	_, err := (&bing{endpoint: ts.URL}).search(context.Background(), ts.Client(), "x", 3)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("429 应为 ErrRateLimited: %v", err)
	}
}

func TestRateBreaker_OpenResetExpiry(t *testing.T) {
	b := newRateBreaker()
	now := time.Unix(1000, 0)
	b.now = func() time.Time { return now }

	b.record("s1", true, false)
	if ok, _ := b.allow("s1"); !ok {
		t.Fatal("1 次限流不应熔断")
	}
	b.record("s1", true, false)
	if ok, left := b.allow("s1"); ok || left <= 0 || left > breakerOpenFor {
		t.Fatalf("连续 2 次限流应熔断: ok=%v left=%v", ok, left)
	}
	if ok, _ := b.allow("s2"); !ok {
		t.Fatal("熔断按会话隔离")
	}
	now = now.Add(breakerOpenFor + time.Second)
	if ok, _ := b.allow("s1"); !ok {
		t.Fatal("到期后应放行探测")
	}
	// 非限流类错误不累加；成功复位计数
	b.record("s3", true, false)
	b.record("s3", false, false)
	b.record("s3", false, true)
	b.record("s3", true, false)
	if ok, _ := b.allow("s3"); !ok {
		t.Fatal("成功应清零连续计数")
	}
	var nilB *rateBreaker
	if ok, _ := nilB.allow("x"); !ok {
		t.Fatal("nil 熔断器恒放行")
	}
	nilB.record("x", true, false)
}

// DDG 连续两次 202 → 熔断，第三次搜索不再请求 DDG，且整条链退到 Bing 成功。
func TestSearchChain_DDG202ThenBing(t *testing.T) {
	var ddgHits atomic.Int32
	ddgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ddgHits.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer ddgSrv.Close()
	fixture := readBingFixture(t)
	bingSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(fixture)
	}))
	defer bingSrv.Close()

	br := newRateBreaker()
	primary := namedEngine{name: "duckduckgo", eng: &ddg{endpoint: ddgSrv.URL, retryDelay: time.Millisecond, breaker: br}}
	fbs := []namedEngine{{name: "bing", eng: &bing{endpoint: bingSrv.URL}}}
	ctx := context.WithValue(context.Background(), protocol.CtxSessionIDKey{}, "sess-1")

	for i := 0; i < 2; i++ {
		res, name, err := searchWithFallback(ctx, ddgSrv.Client(), primary, fbs, "golang", 5)
		if err != nil || name != "bing" || len(res) == 0 {
			t.Fatalf("第 %d 次应退到 bing: name=%s n=%d err=%v", i+1, name, len(res), err)
		}
	}
	before := ddgHits.Load()
	if _, name, err := searchWithFallback(ctx, ddgSrv.Client(), primary, fbs, "golang", 5); err != nil || name != "bing" {
		t.Fatalf("熔断期内仍应由 bing 兜底: %s %v", name, err)
	}
	if ddgHits.Load() != before {
		t.Fatalf("熔断期内不应再请求 DDG（%d -> %d）", before, ddgHits.Load())
	}
}
