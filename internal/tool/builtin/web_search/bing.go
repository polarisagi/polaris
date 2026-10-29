package web_search

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	nethtml "golang.org/x/net/html"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// bing 是无需密钥的第二引擎（HTML 抓取）。DuckDuckGo 的 HTML 端点常以 202 反爬挑战页应答，
// 默认降级链需要一个与 DDG 互不相干的免费引擎兜底；Bing 的结果页结构稳定（li.b_algo > h2 > a + .b_caption p）。
type bing struct {
	endpoint string // 为空时使用 https://www.bing.com/search
}

func (b *bing) search(ctx context.Context, c *http.Client, q string, n int) ([]result, error) {
	base := b.endpoint
	if base == "" {
		base = "https://www.bing.com/search"
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	target := base + sep + "q=" + url.QueryEscape(q)

	req, err := http.NewRequestWithContext(ctx, "GET", target, nil)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "web_search: bing bad request", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")

	resp, err := c.Do(req)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "web_search: bing request failed", err)
	}
	defer resp.Body.Close()

	if isRateLimitStatus(resp.StatusCode) {
		return nil, apperr.Wrap(apperr.CodeResourceExhausted, fmt.Sprintf("web_search: bing returned status %d (rate limited)", resp.StatusCode), ErrRateLimited)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("web_search: bing returned status %d", resp.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "web_search: bing read body failed", err)
	}
	return parseBingHTML(body, n)
}

func parseBingHTML(body []byte, n int) ([]result, error) {
	doc, err := nethtml.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "web_search: parse bing html failed", err)
	}
	var results []result
	var walk func(*nethtml.Node)
	walk = func(node *nethtml.Node) {
		if len(results) >= n {
			return
		}
		if node.Type == nethtml.ElementNode && node.Data == "li" && hasClass(node, "b_algo") {
			if r := extractBingResult(node); r != nil {
				results = append(results, *r)
			}
			return
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return results, nil
}

func hasClass(n *nethtml.Node, want string) bool {
	for _, f := range strings.Fields(getHTMLAttr(n, "class")) {
		if f == want {
			return true
		}
	}
	return false
}

func extractBingResult(li *nethtml.Node) *result {
	var title, href, snippet string
	var walk func(*nethtml.Node)
	walk = func(n *nethtml.Node) {
		if n.Type == nethtml.ElementNode {
			switch {
			case n.Data == "h2" && title == "":
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					if c.Type == nethtml.ElementNode && c.Data == "a" {
						title = strings.Join(strings.Fields(extractNodeText(c)), " ")
						href = getHTMLAttr(c, "href")
						break
					}
				}
			case n.Data == "p" && snippet == "" && title != "":
				snippet = extractNodeText(n)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(li)
	target := unwrapBingLink(href)
	if title == "" || target == "" {
		return nil
	}
	return &result{Title: title, URL: target, Snippet: cleanSnippet(snippet)}
}

// unwrapBingLink 还原 bing.com/ck/a 跟踪跳转里的真实地址：u 参数是 "a1" + base64url(目标 URL)。
// 无法还原的跳转链接丢弃（返回空串）——把跟踪链接当结果交给后续 web_fetch 既不稳定也泄露搜索行为。
func unwrapBingLink(href string) string {
	href = strings.TrimSpace(href)
	if href == "" {
		return ""
	}
	u, err := url.Parse(href)
	if err != nil {
		return ""
	}
	if !strings.HasSuffix(u.Host, "bing.com") {
		if u.Scheme == "http" || u.Scheme == "https" {
			return href
		}
		return ""
	}
	enc := u.Query().Get("u")
	if len(enc) < 3 || !strings.HasPrefix(enc, "a1") {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(enc[2:], "="))
	if err != nil {
		return ""
	}
	target := string(raw)
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		return ""
	}
	return target
}
