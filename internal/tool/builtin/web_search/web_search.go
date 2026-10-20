package web_search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	nethtml "golang.org/x/net/html"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/network"
	"github.com/polarisagi/polaris/internal/tool/builtin/web_fetch"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

type engine interface {
	search(ctx context.Context, c *http.Client, q string, n int) ([]result, error)
}

type ddg struct {
	endpoint string // 为空时使用默认 https://html.duckduckgo.com/html/?q=
	// retryDelay 遇到限流（HTTP 202/429）后的退避时长；零值用 defaultRateLimitBackoff。
	retryDelay time.Duration
}

// defaultRateLimitBackoff DDG 限流后的短退避：202 是其反爬响应，立即重试几乎必然再次命中。
const defaultRateLimitBackoff = 2 * time.Second

// errRateLimited 标记「被搜索引擎限流」，供上层决定是否降级到其他引擎。
var errRateLimited = apperr.NewSentinel(apperr.CodeResourceExhausted, "web_search: rate limited")

// isRateLimitStatus DDG 以 202（反爬挑战页）、429 表示限流；二者都不含搜索结果。
func isRateLimitStatus(code int) bool {
	return code == http.StatusAccepted || code == http.StatusTooManyRequests
}

type searxng struct {
	base string
}

type brave struct {
	key      string
	endpoint string // 为空时使用默认 https://api.search.brave.com/res/v1/web/search
}

// pickEngine 依据配置选择搜索引擎实现；若未指定或缺少必要凭据，降级回退至 duckduckgo。
func pickEngine(cfg *config.Config) (engine, string) {
	if cfg == nil {
		return &ddg{}, "duckduckgo"
	}

	engName := strings.ToLower(strings.TrimSpace(cfg.Agent.Skill.WebSearchEngine))
	if engName == "" {
		engName = "duckduckgo"
	}

	switch engName {
	case "duckduckgo", "ddg":
		return &ddg{endpoint: cfg.Agent.Skill.WebSearchBaseURL}, "duckduckgo"
	case "searxng":
		base := strings.TrimSpace(cfg.Agent.Skill.WebSearchBaseURL)
		if base == "" {
			slog.Warn("web_search: searxng requires web_search_base_url, falling back to duckduckgo")
			return &ddg{}, "duckduckgo"
		}
		return &searxng{base: base}, "searxng"
	case "brave":
		key := strings.TrimSpace(cfg.Agent.Skill.WebSearchAPIKey)
		if key == "" {
			slog.Warn("web_search: brave requires web_search_api_key, falling back to duckduckgo")
			return &ddg{endpoint: cfg.Agent.Skill.WebSearchBaseURL}, "duckduckgo"
		}
		return &brave{key: key, endpoint: cfg.Agent.Skill.WebSearchBaseURL}, "brave"
	default:
		slog.Warn("web_search: unknown search engine, falling back to duckduckgo", "engine", engName)
		return &ddg{endpoint: cfg.Agent.Skill.WebSearchBaseURL}, "duckduckgo"
	}
}

type namedEngine struct {
	name string
	eng  engine
}

// fallbackEngines 返回主引擎失败后可降级的其他引擎：仅包含「配置里已具备凭据」的引擎，
// 绝不凭空启用用户没配置的服务。DDG 为兜底免费引擎，故任何主引擎失败后都可退到 DDG。
func fallbackEngines(cfg *config.Config, primary string) []namedEngine {
	var out []namedEngine
	if cfg != nil {
		if key := strings.TrimSpace(cfg.Agent.Skill.WebSearchAPIKey); key != "" && primary != "brave" {
			out = append(out, namedEngine{name: "brave", eng: &brave{key: key}})
		}
	}
	if primary != "duckduckgo" {
		out = append(out, namedEngine{name: "duckduckgo", eng: &ddg{}})
	}
	return out
}

// searchWithFallback 先用主引擎，失败（含限流）后依次尝试 fallbacks；全部失败返回带各引擎原因的结构化错误。
func searchWithFallback(ctx context.Context, c *http.Client, primary namedEngine, fallbacks []namedEngine, q string, n int) ([]result, string, error) {
	results, err := primary.eng.search(ctx, c, q, n)
	if err == nil {
		return results, primary.name, nil
	}
	reasons := []string{primary.name + ": " + err.Error()}
	for _, fb := range fallbacks {
		if ctx.Err() != nil {
			break
		}
		slog.Warn("web_search: engine failed, falling back", "from", primary.name, "to", fb.name, "err", err)
		r, ferr := fb.eng.search(ctx, c, q, n)
		if ferr == nil {
			return r, fb.name, nil
		}
		reasons = append(reasons, fb.name+": "+ferr.Error())
	}
	code := apperr.CodeInternal
	if errors.Is(err, errRateLimited) {
		code = apperr.CodeResourceExhausted
	}
	return nil, "", apperr.New(code, "web_search 失败（可稍后重试，或换用其他信息来源）："+strings.Join(reasons, "；"))
}

// limitingTransport 在实际发起 HTTP 请求前对目标 Host 施加速率保护，防频繁击穿。
type limitingTransport struct {
	base    http.RoundTripper
	limiter *web_fetch.HostLimiter
}

func (t *limitingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.limiter != nil && req.URL != nil && req.URL.Host != "" {
		if err := t.limiter.Wait(req.Context(), req.URL.Host); err != nil {
			return nil, apperr.Wrap(apperr.CodeCancelled, "web_search: host limiter wait canceled", err)
		}
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "web_search: round trip failed", err)
	}
	return resp, nil
}

type webSearchArgs struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results"`
}

func parseWebSearchArgs(input []byte) (webSearchArgs, error) {
	var args webSearchArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return args, apperr.Wrap(apperr.CodeInvalidInput, "web_search: invalid args", err)
	}
	args.Query = strings.TrimSpace(args.Query)
	if args.Query == "" {
		return args, apperr.New(apperr.CodeInvalidInput, "web_search: query is empty")
	}
	if len(args.Query) > 500 {
		return args, apperr.New(apperr.CodeInvalidInput, "web_search: query exceeds 500 chars")
	}
	if args.MaxResults <= 0 {
		args.MaxResults = 8
	}
	if args.MaxResults > 20 {
		args.MaxResults = 20
	}
	return args, nil
}

// MakeWebSearchFn 构造 web_search 工具闭包，支持 DDG/SearXNG/Brave 多引擎与 HostLimiter 共享。
func MakeWebSearchFn(cfg *config.Config, dialer protocol.SafeDialer, limiters ...*web_fetch.HostLimiter) sandbox.InProcessFn {
	var limiter *web_fetch.HostLimiter
	if len(limiters) > 0 {
		limiter = limiters[0]
	}

	return func(ctx context.Context, input []byte) ([]byte, error) {
		args, err := parseWebSearchArgs(input)
		if err != nil {
			return nil, err
		}
		if dialer == nil {
			return nil, apperr.New(apperr.CodeInternal, "web_search: SafeDialer is required")
		}

		baseTransport := network.WrapCapability(&http.Transport{DialContext: dialer.DialContext}, network.CapNetworkRead)
		tr := baseTransport
		if limiter != nil {
			tr = &limitingTransport{
				base:    baseTransport,
				limiter: limiter,
			}
		}

		client := &http.Client{
			Transport: tr,
			Timeout:   30 * time.Second,
		}

		eng, engineName := pickEngine(cfg)
		results, engineName, err := searchWithFallback(ctx, client, namedEngine{name: engineName, eng: eng},
			fallbackEngines(cfg, engineName), args.Query, args.MaxResults)
		if err != nil {
			return nil, err
		}
		if results == nil {
			results = []result{}
		}

		output := map[string]any{
			"engine":  engineName,
			"results": results,
		}
		data, err := json.Marshal(output)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "web_search: marshal result failed", err)
		}
		return data, nil
	}
}

// search 执行 DuckDuckGo HTML 抓取与抽取。
func (d *ddg) search(ctx context.Context, c *http.Client, q string, n int) ([]result, error) {
	target := d.endpoint
	if target == "" {
		target = "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(q)
	} else if strings.Contains(target, "?") {
		target = target + "&q=" + url.QueryEscape(q)
	} else {
		target = target + "?q=" + url.QueryEscape(q)
	}

	body, err := d.fetch(ctx, c, target)
	if errors.Is(err, errRateLimited) {
		// 202/429 是限流而非结果页：短退避后重试 1 次。
		delay := d.retryDelay
		if delay <= 0 {
			delay = defaultRateLimitBackoff
		}
		slog.Warn("web_search: ddg rate limited, retrying once", "delay", delay)
		select {
		case <-ctx.Done():
			return nil, apperr.Wrap(apperr.CodeCancelled, "web_search: ddg retry canceled", ctx.Err())
		case <-time.After(delay):
		}
		body, err = d.fetch(ctx, c, target)
	}
	if err != nil {
		return nil, err
	}
	return parseDDGHTML(body, n)
}

// fetch 单次抓取 DDG 页面；限流状态码映射为 errRateLimited。
func (d *ddg) fetch(ctx context.Context, c *http.Client, target string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", target, nil)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "web_search: ddg bad request", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")

	resp, err := c.Do(req)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "web_search: ddg request failed", err)
	}
	defer resp.Body.Close()

	if isRateLimitStatus(resp.StatusCode) {
		return nil, apperr.Wrap(apperr.CodeResourceExhausted, fmt.Sprintf("web_search: ddg returned status %d (rate limited)", resp.StatusCode), errRateLimited)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("web_search: ddg returned status %d", resp.StatusCode))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "web_search: ddg read body failed", err)
	}
	return body, nil
}

func parseDDGHTML(body []byte, n int) ([]result, error) {
	doc, err := nethtml.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "web_search: parse ddg html failed", err)
	}

	var results []result
	var walk func(*nethtml.Node)
	walk = func(node *nethtml.Node) {
		if len(results) >= n {
			return
		}
		if node.Type == nethtml.ElementNode {
			classVal := getHTMLAttr(node, "class")
			if strings.Contains(classVal, "result--ad") {
				return
			}
			if isDDGResultContainer(classVal) {
				res := extractSingleDDGResult(node)
				if res != nil {
					results = append(results, *res)
				}
				return
			}
		}

		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return results, nil
}

func isDDGResultContainer(classVal string) bool {
	fields := strings.Fields(classVal)
	hasResult := false
	for _, f := range fields {
		if f == "result--ad" {
			return false
		}
		if f == "result" {
			hasResult = true
		}
	}
	return hasResult
}

func extractSingleDDGResult(container *nethtml.Node) *result {
	var title, rawURL, rawSnippet string
	var walk func(*nethtml.Node)
	walk = func(n *nethtml.Node) {
		if n.Type == nethtml.ElementNode {
			classVal := getHTMLAttr(n, "class")
			if strings.Contains(classVal, "result__a") && title == "" {
				title = strings.TrimSpace(extractNodeText(n))
				rawURL = parseDDGLink(getHTMLAttr(n, "href"))
			}
			if strings.Contains(classVal, "result__snippet") && rawSnippet == "" {
				rawSnippet = strings.TrimSpace(extractNodeText(n))
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(container)

	if title == "" && rawURL == "" {
		return nil
	}
	return &result{
		Title:   title,
		URL:     rawURL,
		Snippet: cleanSnippet(rawSnippet),
	}
}

func parseDDGLink(rawHref string) string {
	rawHref = strings.TrimSpace(rawHref)
	if rawHref == "" {
		return ""
	}
	target := rawHref
	if strings.HasPrefix(target, "//") {
		target = "https:" + target
	}
	if u, err := url.Parse(target); err == nil {
		if uddg := u.Query().Get("uddg"); uddg != "" {
			return uddg
		}
	}
	return rawHref
}

func cleanSnippet(raw string) string {
	cleaned := web_fetch.HTMLToText([]byte(raw))
	cleaned = html.UnescapeString(cleaned)
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	runes := []rune(cleaned)
	if len(runes) > 300 {
		cleaned = string(runes[:300])
	}
	return cleaned
}

func getHTMLAttr(n *nethtml.Node, key string) string {
	for _, attr := range n.Attr {
		if strings.EqualFold(attr.Key, key) {
			return attr.Val
		}
	}
	return ""
}

func extractNodeText(n *nethtml.Node) string {
	var sb strings.Builder
	var walk func(*nethtml.Node)
	walk = func(curr *nethtml.Node) {
		if curr.Type == nethtml.TextNode {
			sb.WriteString(curr.Data)
		}
		for c := curr.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return sb.String()
}

// search 执行 SearXNG JSON 接口查询。
func (s *searxng) search(ctx context.Context, c *http.Client, q string, n int) ([]result, error) {
	base := strings.TrimRight(strings.TrimSpace(s.base), "/")
	target := fmt.Sprintf("%s/search?q=%s&format=json", base, url.QueryEscape(q))

	req, err := http.NewRequestWithContext(ctx, "GET", target, nil)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "web_search: searxng bad request", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")

	resp, err := c.Do(req)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "web_search: searxng request failed", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("web_search: searxng returned status %d", resp.StatusCode))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "web_search: searxng read body failed", err)
	}

	var data struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "web_search: searxng unmarshal failed", err)
	}

	var results []result
	for _, item := range data.Results {
		if len(results) >= n {
			break
		}
		title := strings.TrimSpace(item.Title)
		itemURL := strings.TrimSpace(item.URL)
		if title == "" && itemURL == "" {
			continue
		}
		results = append(results, result{
			Title:   title,
			URL:     itemURL,
			Snippet: cleanSnippet(item.Content),
		})
	}
	return results, nil
}

// search 执行 Brave Search API 查询。
func (b *brave) search(ctx context.Context, c *http.Client, q string, n int) ([]result, error) {
	endpoint := strings.TrimSpace(b.endpoint)
	if endpoint == "" {
		endpoint = "https://api.search.brave.com/res/v1/web/search"
	}
	sep := "?"
	if strings.Contains(endpoint, "?") {
		sep = "&"
	}
	target := fmt.Sprintf("%s%sq=%s&count=%d", endpoint, sep, url.QueryEscape(q), n)

	req, err := http.NewRequestWithContext(ctx, "GET", target, nil)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "web_search: brave bad request", err)
	}
	req.Header.Set("X-Subscription-Token", b.key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")

	resp, err := c.Do(req)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "web_search: brave request failed", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("web_search: brave returned status %d", resp.StatusCode))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "web_search: brave read body failed", err)
	}

	var data struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "web_search: brave unmarshal failed", err)
	}

	var results []result
	for _, item := range data.Web.Results {
		if len(results) >= n {
			break
		}
		title := strings.TrimSpace(item.Title)
		itemURL := strings.TrimSpace(item.URL)
		if title == "" && itemURL == "" {
			continue
		}
		results = append(results, result{
			Title:   title,
			URL:     itemURL,
			Snippet: cleanSnippet(item.Description),
		})
	}
	return results, nil
}
