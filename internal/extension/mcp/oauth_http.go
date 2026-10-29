package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/polarisagi/polaris/internal/security/network"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// maxOAuthDocBytes 发现文档 / token 响应体读取上限，防止恶意/异常服务器喂无限流。
const maxOAuthDocBytes = 256 * 1024

// oauthErrorBody 令牌端点 / DCR 注册端点错误响应的公开字段（RFC 6749 §5.2）。
// 只含服务器主动公开的错误码/描述，不含任何令牌数据，可安全记入错误链与日志。
type oauthErrorBody struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// getJSON GET 一个 JSON 文档并解码为 T；非 2xx 返回错误。
func getJSON[T any](ctx context.Context, hc network.SafeHTTPClient, rawURL string) (*T, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "mcp oauth: build request "+rawURL, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeNetworkUnavailable, "mcp oauth: fetch "+rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, apperr.New(apperr.CodeNotFound, fmt.Sprintf("mcp oauth: %s returned status %d", rawURL, resp.StatusCode))
	}
	var out T
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxOAuthDocBytes)).Decode(&out); err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "mcp oauth: parse "+rawURL, err)
	}
	return &out, nil
}

// postJSON POST 一个 JSON body 并解码响应为 T（用于 DCR 注册）；非 2xx 时优先返回服务器
// 给出的 error/error_description（RFC 7591 §3.2.2），否则返回通用状态码错误。
func postJSON[T any](ctx context.Context, hc network.SafeHTTPClient, rawURL string, body any) (*T, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "mcp oauth: marshal request "+rawURL, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(string(b)))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "mcp oauth: build request "+rawURL, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeNetworkUnavailable, "mcp oauth: post "+rawURL, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthDocBytes))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "mcp oauth: read response "+rawURL, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, apperr.New(apperr.CodeInvalidInput, "mcp oauth: "+rawURL+" failed: "+describeOAuthError(raw, resp.StatusCode))
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "mcp oauth: parse response "+rawURL, err)
	}
	return &out, nil
}

// postForm POST application/x-www-form-urlencoded 并解码 JSON 响应为 T（令牌端点专用）。
// form 中可能含 code_verifier/client_secret 等机密值，仅出现在请求体，绝不进入错误信息或日志。
func postForm[T any](ctx context.Context, hc network.SafeHTTPClient, rawURL string, form url.Values) (*T, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "mcp oauth: build token request", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeNetworkUnavailable, "mcp oauth: token request", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthDocBytes))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "mcp oauth: read token response", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, apperr.New(apperr.CodeInvalidInput, "mcp oauth: token request failed: "+describeOAuthError(raw, resp.StatusCode))
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "mcp oauth: parse token response", err)
	}
	return &out, nil
}

// describeOAuthError 尝试从响应体中提取标准 OAuth error/error_description；解析失败则
// 退化为状态码提示。绝不回显原始 body（可能包含服务器实现细节以外的敏感信息）。
func describeOAuthError(body []byte, status int) string {
	var e oauthErrorBody
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		if e.ErrorDescription != "" {
			return e.Error + ": " + e.ErrorDescription
		}
		return e.Error
	}
	return fmt.Sprintf("status %d", status)
}

// isLoopbackURL 判断 URL 的 host 是否为回环地址（localhost / 127.0.0.0-8 / ::1）。
func isLoopbackURL(u *url.URL) bool {
	if u == nil {
		return false
	}
	return isLoopbackHostname(u.Hostname())
}

// isLoopbackHostname 同 pluginspec 包内同名判断逻辑；两处按各自消费方独立实现，避免
// 为一个 8 行函数建立跨包依赖（pluginspec 是解析期包，不适合被运行期 OAuth 引擎引用）。
func isLoopbackHostname(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
