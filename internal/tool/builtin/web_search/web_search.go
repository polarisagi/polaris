package web_search

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/network"
	"github.com/polarisagi/polaris/pkg/apperr"
)

func MakeWebSearchFn(cfg *config.Config, dialer protocol.SafeDialer) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "web_search: invalid args", err)
		}
		if dialer == nil {
			return nil, apperr.New(apperr.CodeInternal, "web_search: SafeDialer is required")
		}
		// Query 长度防护：防止超大查询消耗带宽和下游资源
		if len(args.Query) == 0 {
			return nil, apperr.New(apperr.CodeInternal, "web_search: query is empty")
		}
		if len(args.Query) > 500 {
			return nil, apperr.New(apperr.CodeInternal, "web_search: query exceeds 500 chars")
		}

		// CapNetworkRead：web_search 只发起 DuckDuckGo HTML GET 请求，与 fetch_url.go
		// 保持一致的读写能力分级出口检查（此前只有 fetch_url 接了 WrapCapability，
		// web_search 用裸 http.Transport 完全绕过 CheckCapability，是纵深防御的缺口——
		// 即便当前代码硬编码 GET，未接检查意味着未来误加 POST/PUT 调用不会被拦截）。
		client := &http.Client{
			Transport: network.WrapCapability(&http.Transport{DialContext: dialer.DialContext}, network.CapNetworkRead),
			Timeout:   30 * time.Second,
		}

		// MVP: DuckDuckGo HTML scraping
		req, err := http.NewRequestWithContext(ctx, "GET", "https://html.duckduckgo.com/html/?q="+url.QueryEscape(args.Query), nil)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "makeWebSearchFn", err)
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")
		resp, err := client.Do(req)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "web_search: req failed", err)
		}
		defer resp.Body.Close()
		// 限制读取大小（2MB），防止超大响应体导致内存耗尽
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))

		tagRe := regexp.MustCompile(`<[^>]*>`)
		spaceRe := regexp.MustCompile(`\s+`)
		snippets := regexp.MustCompile(`(?s)<a class="result__snippet[^>]*>(.*?)</a>`).FindAllStringSubmatch(string(body), 10)

		var results []string
		for _, s := range snippets {
			txt := tagRe.ReplaceAllString(s[1], " ")
			txt = strings.TrimSpace(spaceRe.ReplaceAllString(txt, " "))
			results = append(results, txt)
		}
		return json.Marshal(map[string]any{"results": results})
	}
}
