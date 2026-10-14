package feed_fetch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/network"
	"github.com/polarisagi/polaris/internal/tool/builtin/web_fetch"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type feedFetchArgs struct {
	URL      string `json:"url"`
	MaxItems int    `json:"max_items"`
	Since    string `json:"since"`
}

// FeedItem 定义订阅源条目的统一输出结构（ADR-0114 决策三）。
type FeedItem struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	Author      string `json:"author"`
	PublishedAt string `json:"published_at"`
	Excerpt     string `json:"excerpt"`
	Source      string `json:"source"`
}

// MakeFeedFetchFn 构造 feed_fetch 工具的执行闭包。
func MakeFeedFetchFn(d web_fetch.Deps) sandbox.InProcessFn {
	nowFn := d.Now
	if nowFn == nil {
		nowFn = time.Now
	}

	return func(ctx context.Context, input []byte) ([]byte, error) {
		args, sinceTime, err := parseFeedFetchArgs(input)
		if err != nil {
			return nil, err
		}

		u, err := url.Parse(args.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("feed_fetch: url scheme must be http or https, got: %s", args.URL))
		}
		if u.Host == "" {
			return nil, apperr.New(apperr.CodeInvalidInput, "feed_fetch: url host is required")
		}

		urlHash := sha256Hex(args.URL)
		cached := lookupFeedCache(ctx, d.Cache, urlHash, args.URL)

		if d.Limiter != nil {
			if limitErr := d.Limiter.Wait(ctx, u.Host); limitErr != nil {
				return nil, apperr.Wrap(apperr.CodeCancelled, "feed_fetch: host limiter wait canceled", limitErr)
			}
		}

		client, err := buildFeedClient(d.Dialer)
		if err != nil {
			return nil, err
		}

		req, err := buildFeedRequest(ctx, args.URL, cached)
		if err != nil {
			return nil, err
		}

		resp, err := client.Do(req)
		if err != nil {
			metrics.RecordWebFetch(ctx, "feed_fetch", "error", "none")
			return nil, apperr.Wrap(apperr.CodeInternal, "feed_fetch: request failed", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusNotModified && cached != nil {
			return build304FeedResult(ctx, cached.Title)
		}

		return processFeedResponse(ctx, d, args, resp, urlHash, cached, sinceTime, nowFn())
	}
}

func parseFeedFetchArgs(input []byte) (feedFetchArgs, *time.Time, error) {
	var args feedFetchArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return args, nil, apperr.Wrap(apperr.CodeInvalidInput, "feed_fetch: invalid args", err)
	}
	if strings.TrimSpace(args.URL) == "" {
		return args, nil, apperr.New(apperr.CodeInvalidInput, "feed_fetch: url is required")
	}

	if args.MaxItems <= 0 {
		args.MaxItems = 30
	}
	if args.MaxItems > 200 {
		args.MaxItems = 200
	}

	var sinceTime *time.Time
	if args.Since != "" {
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(args.Since))
		if err != nil {
			return args, nil, apperr.New(apperr.CodeInvalidInput, "feed_fetch: invalid since format, must be RFC3339")
		}
		sinceTime = &t
	}

	return args, sinceTime, nil
}

func lookupFeedCache(ctx context.Context, cache repo.WebCacheRepository, urlHash, rawURL string) *repo.WebCacheRow {
	if cache == nil {
		return nil
	}
	row, err := cache.GetWebCache(ctx, urlHash)
	if err != nil {
		slog.Warn("feed_fetch: failed to get feed cache", "url", rawURL, "err", err)
		return nil
	}
	return row
}

func buildFeedClient(dialer protocol.SafeDialer) (*http.Client, error) {
	if dialer == nil {
		return nil, apperr.New(apperr.CodeInternal, "feed_fetch: SafeDialer is required (XR-06 violation prevented)")
	}
	transport := &http.Transport{
		DialContext: dialer.DialContext,
	}
	return &http.Client{
		Transport: network.WrapCapability(transport, network.CapNetworkRead),
		Timeout:   30 * time.Second,
	}, nil
}

func buildFeedRequest(ctx context.Context, rawURL string, cached *repo.WebCacheRow) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "feed_fetch: bad request", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/feed+json, application/xml;q=0.9, */*;q=0.5")
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

func build304FeedResult(ctx context.Context, title string) ([]byte, error) {
	metrics.RecordWebFetch(ctx, "feed_fetch", "304", "hit")
	result := map[string]any{
		"feed_title": title,
		"items":      []FeedItem{},
		"count":      0,
		"unchanged":  true,
	}
	data, err := json.Marshal(result)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "feed_fetch: marshal 304 result failed", err)
	}
	return data, nil
}

func processFeedResponse(
	ctx context.Context,
	d web_fetch.Deps,
	args feedFetchArgs,
	resp *http.Response,
	urlHash string,
	cached *repo.WebCacheRow,
	sinceTime *time.Time,
	now time.Time,
) ([]byte, error) {
	const maxBody = 5 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		metrics.RecordWebFetch(ctx, "feed_fetch", "error", "none")
		return nil, apperr.Wrap(apperr.CodeInternal, "feed_fetch: read body failed", err)
	}

	fp := gofeed.NewParser()
	feed, parseErr := fp.Parse(bytes.NewReader(body))
	if parseErr != nil {
		metrics.RecordWebFetch(ctx, "feed_fetch", strconv.Itoa(resp.StatusCode), "none")
		return nil, apperr.Wrap(apperr.CodeInternal, "feed_fetch: parse feed failed", parseErr)
	}

	items := mapFeedItems(feed, args.URL, sinceTime, args.MaxItems)
	contentHash := computeFeedContentHash(items)

	unchanged := cached != nil && cached.ContentHash == contentHash && contentHash != ""
	cacheLabel := "miss"
	if unchanged {
		cacheLabel = "hit"
	}

	saveFeedCache(ctx, d.Cache, urlHash, args.URL, resp.StatusCode, resp.Header, feed.Title, contentHash, now)

	metrics.RecordWebFetch(ctx, "feed_fetch", strconv.Itoa(resp.StatusCode), cacheLabel)

	result := map[string]any{
		"feed_title": feed.Title,
		"items":      items,
		"count":      len(items),
		"unchanged":  unchanged,
	}
	data, err := json.Marshal(result)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "feed_fetch: marshal result failed", err)
	}
	return data, nil
}

type itemWithTime struct {
	item    FeedItem
	pubTime *time.Time
}

func mapFeedItems(feed *gofeed.Feed, feedURL string, sinceTime *time.Time, maxItems int) []FeedItem {
	baseURL := feed.Link
	if baseURL == "" {
		baseURL = feedURL
	}

	var candidates []itemWithTime
	for _, rawItem := range feed.Items {
		item, pubTime := mapSingleItem(rawItem, feed, baseURL)
		if sinceTime != nil && pubTime != nil && pubTime.Before(*sinceTime) {
			continue
		}
		candidates = append(candidates, itemWithTime{item: item, pubTime: pubTime})
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		t1, t2 := candidates[i].pubTime, candidates[j].pubTime
		if t1 == nil && t2 == nil {
			return false
		}
		if t1 == nil {
			return false
		}
		if t2 == nil {
			return true
		}
		return t1.After(*t2)
	})

	if len(candidates) > maxItems {
		candidates = candidates[:maxItems]
	}

	items := make([]FeedItem, len(candidates))
	for i, c := range candidates {
		items[i] = c.item
	}
	return items
}

func mapSingleItem(rawItem *gofeed.Item, feed *gofeed.Feed, baseURL string) (FeedItem, *time.Time) {
	id := rawItem.GUID
	if id == "" {
		id = rawItem.Link
	}

	itemURL := resolveFeedLink(rawItem.Link, baseURL)
	author := resolveAuthor(rawItem, feed)
	pubTime, pubStr := resolvePublishedAt(rawItem)

	rawExcerpt := rawItem.Description
	if rawExcerpt == "" {
		rawExcerpt = rawItem.Content
	}
	excerpt := html.UnescapeString(web_fetch.HTMLToText([]byte(rawExcerpt)))
	excerpt = strings.TrimSpace(excerpt)
	runes := []rune(excerpt)
	if len(runes) > 500 {
		excerpt = string(runes[:500])
	}

	feedTitle := ""
	if feed != nil {
		feedTitle = feed.Title
	}

	item := FeedItem{
		ID:          id,
		Title:       strings.TrimSpace(rawItem.Title),
		URL:         itemURL,
		Author:      author,
		PublishedAt: pubStr,
		Excerpt:     excerpt,
		Source:      feedTitle,
	}
	return item, pubTime
}

func resolveFeedLink(link, baseURL string) string {
	link = strings.TrimSpace(link)
	if link == "" {
		return ""
	}
	if base, err := url.Parse(baseURL); err == nil {
		if resolved, resolveErr := base.Parse(link); resolveErr == nil {
			return resolved.String()
		}
	}
	return link
}

func resolveAuthor(rawItem *gofeed.Item, feed *gofeed.Feed) string {
	if rawItem.Author != nil && strings.TrimSpace(rawItem.Author.Name) != "" {
		return strings.TrimSpace(rawItem.Author.Name)
	}
	if len(rawItem.Authors) > 0 && strings.TrimSpace(rawItem.Authors[0].Name) != "" {
		return strings.TrimSpace(rawItem.Authors[0].Name)
	}
	if feed != nil {
		if feed.Author != nil && strings.TrimSpace(feed.Author.Name) != "" {
			return strings.TrimSpace(feed.Author.Name)
		}
		if len(feed.Authors) > 0 && strings.TrimSpace(feed.Authors[0].Name) != "" {
			return strings.TrimSpace(feed.Authors[0].Name)
		}
	}
	return ""
}

func resolvePublishedAt(rawItem *gofeed.Item) (*time.Time, string) {
	pub := rawItem.PublishedParsed
	if pub == nil {
		pub = rawItem.UpdatedParsed
	}
	if pub != nil {
		return pub, pub.UTC().Format(time.RFC3339)
	}
	return nil, ""
}

func computeFeedContentHash(items []FeedItem) string {
	if len(items) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, item := range items {
		sb.WriteString(item.ID)
		sb.WriteString("\n")
	}
	sum := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(sum[:])[:16]
}

func saveFeedCache(ctx context.Context, cache repo.WebCacheRepository, urlHash, rawURL string, status int, header http.Header, title, contentHash string, now time.Time) {
	if cache == nil || status != http.StatusOK {
		return
	}

	cacheRow := repo.WebCacheRow{
		URLHash:      urlHash,
		URL:          rawURL,
		ETag:         header.Get("ETag"),
		LastModified: header.Get("Last-Modified"),
		ContentHash:  contentHash,
		Title:        title,
		Markdown:     "",
		FetchedAt:    now.UTC().Format(time.RFC3339),
	}

	if err := cache.UpsertWebCache(ctx, cacheRow); err != nil {
		slog.Warn("feed_fetch: failed to upsert feed cache", "url", rawURL, "err", err)
		return
	}

	maxRows := config.CurrentThresholds().M7Tool.WebFetchCacheMaxRows
	if maxRows <= 0 {
		maxRows = 5000
	}
	if trimErr := cache.TrimWebCache(ctx, maxRows); trimErr != nil {
		slog.Warn("feed_fetch: failed to trim feed cache", "err", trimErr)
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
