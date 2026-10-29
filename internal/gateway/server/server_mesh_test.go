package server

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/gateway/authcontext"
	"github.com/polarisagi/polaris/internal/gateway/mesh"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	storerepo "github.com/polarisagi/polaris/internal/store/repo"
)

// newMeshTestServer 只装配鉴权中间件需要的部分：a2a 开关 + 委派服务（令牌账本）。
// next 记录到达业务处理器的请求身份。
func newMeshTestServer(t *testing.T, enabled bool) (*Server, http.Handler, *mesh.Service, *authcontext.AuthContext) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	ddl, _ := schema.FS.ReadFile("048_mesh.sql")
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatal(err)
	}
	self, err := mesh.GetOrCreateIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := mesh.NewService(storerepo.NewSQLiteMeshRepository(db), nil, self, mesh.DefaultConfig())
	s := &Server{a2aCfg: config.A2AConfig{Enabled: enabled, Name: "n"}, rootCtx: context.Background()}
	s.SetMeshService(svc, config.MeshConfig{})
	s.isReady.Store(true)
	seen := &authcontext.AuthContext{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = *authcontext.FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	return s, s.withMiddleware(next), svc, seen
}

func do(h http.Handler, method, path, token string) int {
	r := httptest.NewRequest(method, path, nil)
	r.RemoteAddr = "100.101.102.103:40000" // 对端经 Tailscale 来访
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

// TestPeerToken_ConfinedToMeshRoutes 负向用例（ADR-0111 决策三）：发给另一台机器的令牌
// 只能访问委派门面，触及 /_admin/kill 等任何其他路由一律 403。
func TestPeerToken_ConfinedToMeshRoutes(t *testing.T) {
	_, h, svc, seen := newMeshTestServer(t, true)
	tok, _, err := svc.Tokens().Issue(context.Background(), "laptop", "pn-laptop", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{http.MethodPost, "/_admin/kill", http.StatusForbidden},
		{http.MethodPost, "/v1/system/update", http.StatusForbidden},
		{http.MethodGet, "/v1/providers", http.StatusForbidden},
		{http.MethodPost, "/v1/agent/query", http.StatusForbidden},
		{http.MethodPost, "/mcpx", http.StatusForbidden},
		{http.MethodPost, "/a2a/v1/../../_admin/kill", http.StatusForbidden},
		{http.MethodPost, "/mcp", http.StatusOK},
		{http.MethodPost, "/a2a/v1", http.StatusOK},
	} {
		if got := do(h, c.method, c.path, tok); got != c.want {
			t.Errorf("%s %s with peer token: want %d, got %d", c.method, c.path, c.want, got)
		}
	}
	if seen.ClientType != authcontext.ClientTypePeer || seen.PeerNodeID != "pn-laptop" || seen.ClientType.IsLocalTrusted() {
		t.Fatalf("peer identity must reach the handler as an untrusted peer, got %+v", seen)
	}
}

func TestPeerToken_InvalidRevokedOrDisabledIs401(t *testing.T) {
	_, h, svc, _ := newMeshTestServer(t, true)
	ctx := context.Background()
	if got := do(h, http.MethodPost, "/mcp", mesh.TokenPrefix+"forged"); got != http.StatusUnauthorized {
		t.Errorf("forged peer token: want 401, got %d", got)
	}
	tok, info, _ := svc.Tokens().Issue(ctx, "vps", "pn-vps", nil, time.Hour)
	_ = svc.Tokens().Revoke(ctx, info.TokenID)
	if got := do(h, http.MethodPost, "/mcp", tok); got != http.StatusUnauthorized {
		t.Errorf("revoked peer token: want 401, got %d", got)
	}

	_, hOff, svcOff, _ := newMeshTestServer(t, false)
	tokOff, _, _ := svcOff.Tokens().Issue(ctx, "x", "pn-x", nil, time.Hour)
	if got := do(hOff, http.MethodPost, "/mcp", tokOff); got != http.StatusUnauthorized {
		t.Errorf("peer token with a2a disabled: want 401, got %d", got)
	}
}

// TestMeshPaths_NotStaticShell GET /mcp、GET /a2a/v1 不得被「静态外壳免鉴权」放过。
func TestMeshPaths_NotStaticShell(t *testing.T) {
	_, h, _, _ := newMeshTestServer(t, true)
	for _, p := range []string{"/mcp", "/a2a/v1"} {
		if got := do(h, http.MethodGet, p, ""); got != http.StatusUnauthorized {
			t.Errorf("GET %s without credentials: want 401, got %d", p, got)
		}
	}
	if got := do(h, http.MethodGet, "/.well-known/agent-card.json", ""); got != http.StatusOK {
		t.Errorf("agent card must stay public, got %d", got)
	}
}
