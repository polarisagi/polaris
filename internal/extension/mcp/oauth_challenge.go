package mcp

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// AuthChallengeError 401/403 WWW-Authenticate: Bearer 挑战信息（RFC 6750 §3，basic_authorization.md
// §Error Handling）。字段全部来自服务器返回的公开挑战参数，不含任何令牌/凭据——可安全记入
// 日志与错误链。上层用 errors.As 从 apperr 包装链中提取。
type AuthChallengeError struct {
	// Status 401（未授权）或 403（insufficient_scope）。
	Status int
	// ResourceMetadata WWW-Authenticate 的 resource_metadata 参数（RFC 9728 发现入口）。
	ResourceMetadata string
	// Scope WWW-Authenticate 的 scope 参数（空格分隔），401 场景为可选指引，403 场景为规范
	// 要求的"满足当前操作所需的最小 scope 集合"。
	Scope string
	// ErrorCode WWW-Authenticate 的 error 参数（如 "insufficient_scope"）。字段名避开 Error
	// ——同名会与 error 接口方法 Error() 冲突（Go 不允许字段与方法同名）。
	ErrorCode string
	// ErrorDescription WWW-Authenticate 的 error_description 参数（人类可读）。
	ErrorDescription string
}

func (e *AuthChallengeError) Error() string {
	return fmt.Sprintf("mcp: auth challenge status=%d error=%q resource_metadata=%q scope=%q",
		e.Status, e.ErrorCode, e.ResourceMetadata, e.Scope)
}

// parseWWWAuthenticateBearer 解析 "Bearer key1=\"val1\", key2=val2" 形式的 auth-param 列表
// （RFC 7235 §2.1）。scheme 大小写不敏感；非 Bearer 挑战返回 nil。
func parseWWWAuthenticateBearer(header string) map[string]string {
	header = strings.TrimSpace(header)
	const scheme = "bearer"
	if len(header) < len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return nil
	}
	rest := strings.TrimSpace(header[len(scheme):])
	params := map[string]string{}
	for _, part := range splitAuthParams(rest) {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"`)
		if k != "" {
			params[k] = v
		}
	}
	return params
}

// splitAuthParams 按逗号切分 auth-param 列表，尊重双引号内的逗号
// （error_description 等值本身可能含逗号）。不支持引号转义——规范场景不需要。
func splitAuthParams(s string) []string {
	var out []string
	var buf strings.Builder
	inQuotes := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuotes = !inQuotes
			buf.WriteRune(r)
		case r == ',' && !inQuotes:
			out = append(out, buf.String())
			buf.Reset()
		default:
			buf.WriteRune(r)
		}
	}
	if buf.Len() > 0 {
		out = append(out, buf.String())
	}
	return out
}

// authChallengeFromResponse 从 401/403 响应构造 AuthChallengeError；403 只在
// error=insufficient_scope 时视为 OAuth 挑战（basic_authorization.md §Scope Challenge Handling），
// 否则返回 nil，交由调用方走通用错误路径（非 OAuth 语义的普通 403）。
func authChallengeFromResponse(resp *http.Response) *AuthChallengeError {
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		return nil
	}
	params := parseWWWAuthenticateBearer(resp.Header.Get("WWW-Authenticate"))
	if resp.StatusCode == http.StatusForbidden && params["error"] != "insufficient_scope" {
		return nil
	}
	return &AuthChallengeError{
		Status:           resp.StatusCode,
		ResourceMetadata: params["resource_metadata"],
		Scope:            params["scope"],
		ErrorCode:        params["error"],
		ErrorDescription: params["error_description"],
	}
}

// extractAuthChallenge 从 err 链中提取 *AuthChallengeError；不存在返回 nil。
func extractAuthChallenge(err error) *AuthChallengeError {
	var ace *AuthChallengeError
	if errors.As(err, &ace) {
		return ace
	}
	return nil
}

// wrapAuthChallenge 用合适的 apperr.Code 包装挑战（401→CodeUnauthorized，403→CodeForbidden），
// 保持 Cause 链使 errors.As(*AuthChallengeError) 可用。
func wrapAuthChallenge(serverName string, ace *AuthChallengeError) error {
	code := apperr.CodeUnauthorized
	if ace.Status == http.StatusForbidden {
		code = apperr.CodeForbidden
	}
	return apperr.Wrap(code, fmt.Sprintf("mcp: %s requires authorization (status %d)", serverName, ace.Status), ace)
}
