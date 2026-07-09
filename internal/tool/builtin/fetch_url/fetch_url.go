package fetch_url

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type fetchURLArgs struct {
	URL string `json:"url"`
}

func MakeFetchURLFn(dialer protocol.SafeDialer) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		if dialer == nil {
			return nil, apperr.New(apperr.CodeInternal, "fetch_url: SafeDialer is required (XR-06 violation prevented)")
		}

		client := &http.Client{
			Transport: &http.Transport{
				DialContext: dialer.DialContext,
			},
			Timeout: 30 * time.Second,
		}

		var args fetchURLArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "fetch_url: invalid args", err)
		}
		if args.URL == "" {
			return nil, apperr.New(apperr.CodeInternal, "fetch_url: url is required")
		}

		// SSRF Guard Phase 1: 基础文本正则检查 (SafeDialer 内部会有更严格的解析检查)
		if isPrivateURL(args.URL) {
			return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("fetch_url: SSRF guard blocked private URL: %s", args.URL))
		}

		req, err := http.NewRequestWithContext(ctx, "GET", args.URL, nil)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "fetch_url: bad request", err)
		}

		// 伪装 User-Agent，避免被简单的爬虫拦截
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

		resp, err := client.Do(req)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "fetch_url: request failed", err)
		}
		defer resp.Body.Close()

		// 限制读取大小（最大 2MB），防止内存溢出
		bodyReader := io.LimitReader(resp.Body, 2*1024*1024)
		body, err := io.ReadAll(bodyReader)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "fetch_url: read response body failed", err)
		}

		// 如果超出了限制
		truncated := len(body) == 2*1024*1024

		contentStr := string(body)
		contentType := resp.Header.Get("Content-Type")
		if strings.Contains(contentType, "text/html") {
			// MVP 阶段：简单的正则清洗 HTML 标签
			tagRe := regexp.MustCompile(`<[^>]*>`)
			spaceRe := regexp.MustCompile(`\s+`)
			contentStr = tagRe.ReplaceAllString(contentStr, " ")
			contentStr = strings.TrimSpace(spaceRe.ReplaceAllString(contentStr, " "))
		}

		result := map[string]any{
			"url":       args.URL,
			"status":    resp.StatusCode,
			"truncated": truncated,
			"content":   contentStr,
		}
		return json.Marshal(result)
	}
}

func isPrivateURL(rawURL string) bool {
	privatePatterns := []string{
		"localhost", "127.", "10.", "192.168.", "172.16.", "169.254.",
		"::1", "0.0.0.0", "metadata.google", "169.254.169.254",
	}
	lower := strings.ToLower(rawURL)
	for _, p := range privatePatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}
