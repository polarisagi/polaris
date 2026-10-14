package web_fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/network"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// Deps 定义 web_fetch 工具的外部依赖（ADR-0114 决策三）。
type Deps struct {
	Dialer  protocol.SafeDialer
	Cache   repo.WebCacheRepository // 可 nil，为 nil 时不走缓存
	Limiter *HostLimiter            // 可 nil，为 nil 时不限速
	Now     func() time.Time        // 可 nil，默认为 time.Now
}

type webFetchArgs struct {
	URL      string `json:"url"`
	Mode     string `json:"mode"`
	MaxChars int    `json:"max_chars"`
	Offset   int    `json:"offset"`
	Selector string `json:"selector"`
}

// MakeWebFetchFn 构造 web_fetch 工具的执行闭包。
func MakeWebFetchFn(d Deps) sandbox.InProcessFn {
	nowFn := d.Now
	if nowFn == nil {
		nowFn = time.Now
	}

	return func(ctx context.Context, input []byte) ([]byte, error) {
		args, err := parseWebFetchArgs(input)
		if err != nil {
			return nil, err
		}

		urlHash := sha256Hex(args.URL)
		cached := lookupWebCache(ctx, d.Cache, urlHash, args.URL)

		resp, err := fetchHTTP(ctx, d, args.URL, cached, nowFn)
		if err != nil {
			return nil, err
		}

		if resp.StatusCode == http.StatusNotModified && cached != nil {
			resp.Body.Close()
			return build304Result(ctx, args, cached)
		}
		defer resp.Body.Close()

		return processResponse(ctx, d, args, resp, urlHash, cached, nowFn())
	}
}

func fetchHTTP(ctx context.Context, d Deps, rawURL string, cached *repo.WebCacheRow, nowFn func() time.Time) (*http.Response, error) {
	u, err := parseFetchURL(rawURL)
	if err != nil {
		return nil, err
	}
	if d.Limiter != nil {
		if limitErr := d.Limiter.Wait(ctx, u.Host); limitErr != nil {
			return nil, limitErr
		}
	}
	client, err := newFetchClient(d.Dialer)
	if err != nil {
		return nil, err
	}
	req, err := buildFetchRequest(ctx, rawURL, cached)
	if err != nil {
		return nil, err
	}
	return executeFetchWithRetry(ctx, client, req, nowFn, cached)
}

func processResponse(ctx context.Context, d Deps, args webFetchArgs, resp *http.Response, urlHash string, cached *repo.WebCacheRow, now time.Time) ([]byte, error) {
	finalURL := resolveFinalURL(args.URL, resp)
	body, bodyTruncated, err := readResponseBody(resp.Body)
	if err != nil {
		recordWebFetchMetric(ctx, "web_fetch", "error", "none")
		return nil, apperr.Wrap(apperr.CodeInternal, "web_fetch: read response body failed", err)
	}

	mediaType := parseMediaType(resp.Header.Get("Content-Type"))
	title, content, err := extractContent(body, finalURL, args.Selector, args.Mode, mediaType)
	if err != nil {
		recordWebFetchMetric(ctx, "web_fetch", strconv.Itoa(resp.StatusCode), "none")
		return nil, err
	}

	recordExtractionRatio(ctx, len(body), len(content))

	contentHash, unchanged := evaluateContentHash(content, cached)
	cacheLabel := "miss"
	if unchanged {
		cacheLabel = "hit"
	}

	saveCacheIfNeeded(ctx, d.Cache, urlHash, args.URL, args.Mode, resp.StatusCode, resp.Header, title, content, contentHash, now)

	recordWebFetchMetric(ctx, "web_fetch", strconv.Itoa(resp.StatusCode), cacheLabel)

	chunk, nextOffset, pageTruncated := paginate(content, args.Offset, args.MaxChars)
	truncated := pageTruncated || bodyTruncated

	result := map[string]any{
		"url":          args.URL,
		"final_url":    finalURL,
		"status":       resp.StatusCode,
		"content_type": mediaType,
		"title":        title,
		"content":      chunk,
		"content_hash": contentHash,
		"truncated":    truncated,
		"next_offset":  nextOffset,
		"unchanged":    unchanged,
	}
	data, err := json.Marshal(result)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "web_fetch: marshal result failed", err)
	}
	return data, nil
}

func parseWebFetchArgs(input []byte) (webFetchArgs, error) {
	var args webFetchArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return args, apperr.Wrap(apperr.CodeInvalidInput, "web_fetch: invalid args", err)
	}
	if strings.TrimSpace(args.URL) == "" {
		return args, apperr.New(apperr.CodeInvalidInput, "web_fetch: url is required")
	}

	if args.Mode == "" {
		args.Mode = "markdown"
	}
	switch args.Mode {
	case "markdown", "text", "links", "raw":
	default:
		return args, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("web_fetch: invalid mode %q, supported modes are markdown, text, links, raw", args.Mode))
	}

	if args.MaxChars <= 0 {
		args.MaxChars = config.CurrentThresholds().M7Tool.WebFetchDefaultMaxChars
		if args.MaxChars <= 0 {
			args.MaxChars = 12000
		}
	}
	if args.MaxChars > 50000 {
		args.MaxChars = 50000
	}
	if args.Offset < 0 {
		args.Offset = 0
	}
	return args, nil
}

func parseFetchURL(rawURL string) (*url.URL, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("web_fetch: url scheme must be http or https, got: %s", rawURL))
	}
	if u.Host == "" {
		return nil, apperr.New(apperr.CodeInvalidInput, "web_fetch: url host is required")
	}
	return u, nil
}

func lookupWebCache(ctx context.Context, cache repo.WebCacheRepository, urlHash, rawURL string) *repo.WebCacheRow {
	if cache == nil {
		return nil
	}
	row, err := cache.GetWebCache(ctx, urlHash)
	if err != nil {
		slog.Warn("web_fetch: failed to get web cache", "url", rawURL, "err", err)
		return nil
	}
	return row
}

func newFetchClient(dialer protocol.SafeDialer) (*http.Client, error) {
	if dialer == nil {
		return nil, apperr.New(apperr.CodeInternal, "web_fetch: SafeDialer is required (XR-06 violation prevented)")
	}
	transport := &http.Transport{
		DialContext: dialer.DialContext,
	}
	return &http.Client{
		Transport: network.WrapCapability(transport, network.CapNetworkRead),
		Timeout:   30 * time.Second,
	}, nil
}

func buildFetchRequest(ctx context.Context, rawURL string, cached *repo.WebCacheRow) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "web_fetch: bad request", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/json;q=0.9,*/*;q=0.8")
	if cached != nil {
		if cached.ETag != "" {
			req.Header.Set("If-None-Match", cached.ETag)
		}
		if cached.LastModified != "" {
			req.Header.Set("If-Modified-Since", cached.LastModified)
		}
	}
	return req, nil
}

func executeFetchWithRetry(ctx context.Context, client *http.Client, req *http.Request, nowFn func() time.Time, cached *repo.WebCacheRow) (*http.Response, error) {
	resp, err := client.Do(req)
	if err != nil {
		recordWebFetchMetric(ctx, "web_fetch", "error", "none")
		return nil, apperr.Wrap(apperr.CodeInternal, "web_fetch: request failed", err)
	}

	if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusServiceUnavailable {
		return resp, nil
	}

	retrySec := parseRetryAfter(resp.Header.Get("Retry-After"), nowFn())
	if retrySec <= 0 {
		return resp, nil
	}
	if retrySec > 60 {
		resp.Body.Close()
		recordWebFetchMetric(ctx, "web_fetch", strconv.Itoa(resp.StatusCode), "none")
		return nil, apperr.New(apperr.CodeNetworkUnavailable, fmt.Sprintf("web_fetch: service unavailable, retry-after %d exceeds 60s", retrySec))
	}

	resp.Body.Close()
	t := time.NewTimer(time.Duration(retrySec) * time.Second)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
		return nil, apperr.Wrap(apperr.CodeCancelled, "web_fetch: retry wait canceled", ctx.Err())
	}

	reqRetry, errRetry := buildFetchRequest(ctx, req.URL.String(), cached)
	if errRetry != nil {
		return nil, errRetry
	}
	respRetry, errRetryReq := client.Do(reqRetry)
	if errRetryReq != nil {
		recordWebFetchMetric(ctx, "web_fetch", "error", "none")
		return nil, apperr.Wrap(apperr.CodeInternal, "web_fetch: retry request failed", errRetryReq)
	}
	return respRetry, nil
}

func build304Result(ctx context.Context, args webFetchArgs, cached *repo.WebCacheRow) ([]byte, error) {
	recordWebFetchMetric(ctx, "web_fetch", "304", "hit")
	chunk, nextOffset, truncated := paginate(cached.Markdown, args.Offset, args.MaxChars)
	result := map[string]any{
		"url":          args.URL,
		"final_url":    args.URL,
		"status":       http.StatusNotModified,
		"content_type": "text/html",
		"title":        cached.Title,
		"content":      chunk,
		"content_hash": cached.ContentHash,
		"truncated":    truncated,
		"next_offset":  nextOffset,
		"unchanged":    true,
	}
	data, err := json.Marshal(result)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "web_fetch: marshal 304 result failed", err)
	}
	return data, nil
}

func resolveFinalURL(rawURL string, resp *http.Response) string {
	if resp.Request != nil && resp.Request.URL != nil {
		return resp.Request.URL.String()
	}
	return rawURL
}

func readResponseBody(r io.Reader) (body []byte, truncated bool, err error) {
	const maxBodyBytes = 2 << 20
	bodyReader := io.LimitReader(r, maxBodyBytes)
	body, err = io.ReadAll(bodyReader)
	if err != nil {
		return nil, false, apperr.Wrap(apperr.CodeInternal, "web_fetch: read body failed", err)
	}
	return body, len(body) >= maxBodyBytes, nil
}

func parseMediaType(rawContentType string) string {
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(rawContentType, ";")[0]))
	if mediaType == "" {
		return "text/html"
	}
	return mediaType
}

func extractContent(body []byte, finalURL, selector, mode, mediaType string) (title, content string, err error) {
	switch mediaType {
	case "text/html", "application/xhtml+xml":
		return extractHTMLByMode(body, finalURL, selector, mode)
	case "text/plain", "application/json":
		return "", string(body), nil
	case "application/rss+xml", "application/atom+xml", "application/xml", "text/xml":
		return "", formatFeedHint(body), nil
	default:
		return "", "", nil
	}
}

func extractHTMLByMode(body []byte, finalURL, selector, mode string) (title, content string, err error) {
	switch mode {
	case "markdown":
		return extractMarkdown(body, finalURL, selector)
	case "links":
		links, extErr := extractLinks(body, finalURL)
		return extractHTMLTitle(body), links, extErr
	case "text":
		return extractHTMLTitle(body), htmlToText(body), nil
	default: // "raw"
		return extractHTMLTitle(body), string(body), nil
	}
}

func formatFeedHint(body []byte) string {
	const hint = "这是 feed，请用 feed_fetch\n"
	preview := string(body)
	runes := []rune(preview)
	if len(runes) > 2000 {
		preview = string(runes[:2000])
	}
	return hint + preview
}

func recordExtractionRatio(ctx context.Context, bodyLen, contentLen int) {
	if bodyLen > 0 && contentLen > 0 {
		ratio := float64(contentLen) / float64(bodyLen)
		recordWebFetchMetricExtractedRatio(ctx, ratio)
	}
}

func evaluateContentHash(content string, cached *repo.WebCacheRow) (contentHash string, unchanged bool) {
	if content == "" {
		return "", false
	}
	fullHash := sha256Hex(content)
	if len(fullHash) >= 16 {
		contentHash = fullHash[:16]
	} else {
		contentHash = fullHash
	}
	unchanged = cached != nil && cached.ContentHash == contentHash
	return contentHash, unchanged
}

func saveCacheIfNeeded(ctx context.Context, cache repo.WebCacheRepository, urlHash, rawURL, mode string, status int, header http.Header, title, content, contentHash string, now time.Time) {
	if cache == nil || mode != "markdown" || status != http.StatusOK {
		return
	}

	cacheMD := content
	const maxCacheMarkdown = 256 * 1024
	if len(cacheMD) > maxCacheMarkdown {
		cacheMD = cacheMD[:maxCacheMarkdown]
	}

	cacheRow := repo.WebCacheRow{
		URLHash:      urlHash,
		URL:          rawURL,
		ETag:         header.Get("ETag"),
		LastModified: header.Get("Last-Modified"),
		ContentHash:  contentHash,
		Title:        title,
		Markdown:     cacheMD,
		FetchedAt:    now.UTC().Format(time.RFC3339),
	}

	if err := cache.UpsertWebCache(ctx, cacheRow); err != nil {
		slog.Warn("web_fetch: failed to upsert web cache", "url", rawURL, "err", err)
		return
	}

	maxRows := config.CurrentThresholds().M7Tool.WebFetchCacheMaxRows
	if maxRows <= 0 {
		maxRows = 5000
	}
	if trimErr := cache.TrimWebCache(ctx, maxRows); trimErr != nil {
		slog.Warn("web_fetch: failed to trim web cache", "err", trimErr)
	}
}

func parseRetryAfter(header string, now time.Time) int {
	header = strings.TrimSpace(header)
	if sec, err := strconv.Atoi(header); err == nil {
		return sec
	}
	if t, err := http.ParseTime(header); err == nil {
		diff := int(t.Sub(now).Seconds())
		if diff < 0 {
			return 0
		}
		return diff
	}
	return 0
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// recordWebFetchMetric 记录抓取调用（接入可观测指标，ADR-0114）。
func recordWebFetchMetric(ctx context.Context, tool, status, cache string) {
	metrics.RecordWebFetch(ctx, tool, status, cache)
}

// recordWebFetchMetricExtractedRatio 记录正文抽取字符压缩比（接入可观测指标，ADR-0114）。
func recordWebFetchMetricExtractedRatio(ctx context.Context, ratio float64) {
	metrics.RecordWebFetchExtractedRatio(ctx, ratio)
}
