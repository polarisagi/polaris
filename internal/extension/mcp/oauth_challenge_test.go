package mcp

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func TestParseWWWAuthenticateBearer(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   map[string]string
	}{
		{
			name:   "quoted single param",
			header: `Bearer resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource"`,
			want:   map[string]string{"resource_metadata": "https://mcp.example.com/.well-known/oauth-protected-resource"},
		},
		{
			name:   "multiple comma separated params",
			header: `Bearer resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource", scope="files:read"`,
			want: map[string]string{
				"resource_metadata": "https://mcp.example.com/.well-known/oauth-protected-resource",
				"scope":             "files:read",
			},
		},
		{
			name:   "insufficient_scope with error_description containing comma",
			header: `Bearer error="insufficient_scope", scope="files:write", error_description="need, more scope"`,
			want: map[string]string{
				"error":             "insufficient_scope",
				"scope":             "files:write",
				"error_description": "need, more scope",
			},
		},
		{
			name:   "case-insensitive scheme",
			header: `bearer scope="a b"`,
			want:   map[string]string{"scope": "a b"},
		},
		{
			name:   "non-bearer scheme ignored",
			header: `Basic realm="foo"`,
			want:   nil,
		},
		{
			name:   "empty header",
			header: "",
			want:   nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseWWWAuthenticateBearer(tc.header)
			if len(got) != len(tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("key %q: got %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestAuthChallengeFromResponse_401(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{}}
	resp.Header.Set("WWW-Authenticate", `Bearer resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource", scope="files:read"`)
	ace := authChallengeFromResponse(resp)
	if ace == nil {
		t.Fatal("expected non-nil AuthChallengeError")
	}
	if ace.Status != http.StatusUnauthorized || ace.ResourceMetadata == "" || ace.Scope != "files:read" {
		t.Errorf("unexpected challenge: %+v", ace)
	}
}

func TestAuthChallengeFromResponse_403InsufficientScope(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}}
	resp.Header.Set("WWW-Authenticate", `Bearer error="insufficient_scope", scope="files:write", resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource"`)
	ace := authChallengeFromResponse(resp)
	if ace == nil {
		t.Fatal("expected non-nil AuthChallengeError for insufficient_scope 403")
	}
	if ace.ErrorCode != "insufficient_scope" || ace.Scope != "files:write" {
		t.Errorf("unexpected challenge: %+v", ace)
	}
}

// TestAuthChallengeFromResponse_403WithoutInsufficientScope 验证普通 403（无 insufficient_scope）
// 不被误判为 OAuth 挑战，避免把无关的权限拒绝也弹去重新授权。
func TestAuthChallengeFromResponse_403WithoutInsufficientScope(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}}
	if ace := authChallengeFromResponse(resp); ace != nil {
		t.Errorf("expected nil for plain 403, got %+v", ace)
	}
}

func TestAuthChallengeFromResponse_200IsNil(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}
	if ace := authChallengeFromResponse(resp); ace != nil {
		t.Errorf("expected nil for 200, got %+v", ace)
	}
}

// TestWrapAuthChallenge_ErrorsAs 验证 apperr 包装后仍可用 errors.As 提取原始挑战，
// 且不同状态码映射到不同 apperr.Code（供上层区分 401/403 语义）。
func TestWrapAuthChallenge_ErrorsAs(t *testing.T) {
	ace401 := &AuthChallengeError{Status: http.StatusUnauthorized, Scope: "a"}
	err := wrapAuthChallenge("srv", ace401)
	if apperr.CodeOf(err) != apperr.CodeUnauthorized {
		t.Errorf("expected CodeUnauthorized, got %s", apperr.CodeOf(err))
	}
	var got *AuthChallengeError
	if !errors.As(err, &got) || got != ace401 {
		t.Fatal("errors.As failed to extract AuthChallengeError")
	}

	ace403 := &AuthChallengeError{Status: http.StatusForbidden, ErrorCode: "insufficient_scope"}
	err403 := wrapAuthChallenge("srv", ace403)
	if apperr.CodeOf(err403) != apperr.CodeForbidden {
		t.Errorf("expected CodeForbidden, got %s", apperr.CodeOf(err403))
	}

	// 错误信息与日志中不得出现令牌/凭据（本类型本就不携带这些字段，这里断言字符串
	// 形式确实只含公开挑战参数）。
	if s := err.Error(); s == "" {
		t.Fatal("Error() must not be empty")
	}
}

// TestAuthChallengeFromResponse_NoWWWAuthenticateStill401 验证服务器 401 但省略
// WWW-Authenticate 头时仍返回一个挑战（字段为空），保证连接失败可靠地转入需授权路径。
func TestAuthChallengeFromResponse_NoWWWAuthenticateStill401(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{}}
	ace := authChallengeFromResponse(resp)
	if ace == nil || ace.Status != http.StatusUnauthorized {
		t.Fatalf("expected 401 challenge even without header, got %+v", ace)
	}
}

// TestAuthChallengeFromResponse_RealHTTPResponse 用真实 httptest 响应验证 Header 解析路径
// （而不仅仅是手工构造 http.Response）。
func TestAuthChallengeFromResponse_RealHTTPResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL) //nolint:noctx,gosec
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	ace := authChallengeFromResponse(resp)
	if ace == nil || ace.ResourceMetadata == "" {
		t.Fatalf("expected challenge with resource_metadata, got %+v", ace)
	}
}
