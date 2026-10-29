package chat

import (
	"net/http/httptest"
	"testing"

	"github.com/polarisagi/polaris/internal/gateway/authcontext"
)

// owner 信任只取自鉴权上下文：已认证的本地可信客户端才算；匿名回环兜底、远程 API 都不算。
func TestOwnerTrustedRequest(t *testing.T) {
	cases := []struct {
		name string
		auth *authcontext.AuthContext
		want bool
	}{
		{"authenticated local webui", &authcontext.AuthContext{Authenticated: true, ClientType: authcontext.ClientTypeLocalWebUI}, true},
		{"authenticated webui", &authcontext.AuthContext{Authenticated: true, ClientType: authcontext.ClientTypeWebUI}, true},
		{"anonymous loopback fallback", &authcontext.AuthContext{Authenticated: false, ClientType: authcontext.ClientTypeLocalWebUI}, false},
		{"remote api client", &authcontext.AuthContext{Authenticated: true, ClientType: authcontext.ClientTypeAPI}, false},
		{"peer node", &authcontext.AuthContext{Authenticated: true, ClientType: authcontext.ClientTypePeer}, false},
	}
	for _, tc := range cases {
		r := httptest.NewRequest("POST", "/v1/agent/stream", nil)
		r = r.WithContext(authcontext.WithAuthContext(r.Context(), tc.auth))
		if got := ownerTrustedRequest(r); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
	if ownerTrustedRequest(httptest.NewRequest("POST", "/", nil)) {
		t.Error("request without auth context must not be trusted")
	}
}
