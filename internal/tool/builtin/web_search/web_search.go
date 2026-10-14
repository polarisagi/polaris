package web_search

import (
	"bytes"
	"context"
	"encoding/json"
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
		results, err := eng.search(ctx, client, args.Query, args.MaxResults)
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

	if resp.StatusCode != http.StatusOK {
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("web_search: ddg returned status %d", resp.StatusCode))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "web_search: ddg read body failed", err)
	}

	return parseDDGHTML(body, n)
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
