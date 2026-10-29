package plugin

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/polarisagi/polaris/internal/action/hook"
	"github.com/polarisagi/polaris/internal/store/repo"
)

// trustBackedProvider 以 hook_trust 表决定插件来源信任状态（与 lifecycle.HookSourceProvider 同语义）。
type trustBackedProvider struct {
	db     *sql.DB
	source hook.Source
}

func (p trustBackedProvider) ListHookSources(ctx context.Context) ([]hook.Source, error) {
	trust, err := repo.NewSQLiteExtensionRepository(p.db).ListHookTrust(ctx)
	if err != nil {
		return nil, err
	}
	src := p.source
	src.Trusted = trust[src.Key] == src.Digest
	return []hook.Source{src}, nil
}

func newHookHandler(t *testing.T) (*PluginHandler, hook.Source) {
	t.Helper()
	h := getDummyServerWithInstallMgr(t)
	db := h.DB.(*sql.DB)
	if _, err := db.Exec(`CREATE TABLE hook_trust (source_key TEXT PRIMARY KEY, digest TEXT NOT NULL, trusted_at TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	cfg, err := hook.ParseFile([]byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"true"}]}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	src := hook.Source{Key: "plugin:pl_1:hooks/hooks.json", Scope: hook.ScopePlugin, PluginID: "pl_1", Digest: "d1", Config: cfg}
	reg := hook.NewRegistry(trustBackedProvider{db: db, source: src})
	h.HookRunner = hook.NewRunner(hook.Deps{Registry: reg})
	h.ExtRepo = repo.NewSQLiteExtensionRepository(db)
	return h, src
}

func trustRequest(t *testing.T, h *PluginHandler, key, digest string) int {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"key": key, "digest": digest})
	w := httptest.NewRecorder()
	h.HandleTrustHook(w, httptest.NewRequest("POST", "/v1/hooks/trust", bytes.NewReader(body)))
	return w.Code
}

func listedTrusted(t *testing.T, h *PluginHandler) bool {
	t.Helper()
	w := httptest.NewRecorder()
	h.HandleListHooks(w, httptest.NewRequest("GET", "/v1/hooks", nil))
	var out struct {
		Sources []struct {
			Trusted bool `json:"trusted"`
		} `json:"sources"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.Sources) != 1 {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	return out.Sources[0].Trusted
}

func TestHookTrustLifecycle(t *testing.T) {
	h, src := newHookHandler(t)
	if listedTrusted(t, h) {
		t.Fatal("plugin hooks must start untrusted")
	}
	if code := trustRequest(t, h, "plugin:unknown", "x"); code != http.StatusNotFound {
		t.Fatalf("unknown source: %d", code)
	}
	if code := trustRequest(t, h, src.Key, "stale-digest"); code != http.StatusConflict {
		t.Fatalf("stale digest must conflict: %d", code)
	}
	if code := trustRequest(t, h, src.Key, src.Digest); code != http.StatusOK {
		t.Fatalf("trust: %d", code)
	}
	if !listedTrusted(t, h) {
		t.Fatal("source must be trusted after review")
	}
	w := httptest.NewRecorder()
	h.HandleRevokeHookTrust(w, httptest.NewRequest("DELETE", "/v1/hooks/trust?key="+src.Key, nil))
	if w.Code != http.StatusOK || listedTrusted(t, h) {
		t.Fatalf("revoke: %d trusted=%v", w.Code, listedTrusted(t, h))
	}
}

func TestHookHandlers_EngineMissing(t *testing.T) {
	h := getDummyServerWithInstallMgr(t)
	w := httptest.NewRecorder()
	h.HandleListHooks(w, httptest.NewRequest("GET", "/v1/hooks", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}
