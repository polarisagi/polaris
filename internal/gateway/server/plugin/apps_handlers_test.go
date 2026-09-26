package plugin

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/store/repo"
)

func TestPluginAppHandlers(t *testing.T) {
	h := getDummyServerWithInstallMgr(t)
	db := h.DB.(*sql.DB)
	for _, ddl := range []string{
		`CREATE TABLE plugin_app_bindings (plugin_id TEXT, alias TEXT, connector_ref TEXT, bound_server_id TEXT DEFAULT '', status TEXT DEFAULT 'unbound', updated_at TEXT DEFAULT '', PRIMARY KEY(plugin_id, alias))`,
		`INSERT INTO plugin_app_bindings(plugin_id, alias, connector_ref) VALUES('pl_w','slack','connector_abc')`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatal(err)
		}
	}
	h.ExtRepo = repo.NewSQLiteExtensionRepository(db)
	req := httptest.NewRequest("GET", "/v1/plugins/pl_w/apps", nil)
	req.SetPathValue("id", "pl_w")
	w := httptest.NewRecorder()
	h.HandleListPluginApps(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"alias":"slack"`) {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	req = httptest.NewRequest("PUT", "/v1/plugins/pl_w/apps/nope", strings.NewReader(`{"server_id":""}`))
	req.SetPathValue("id", "pl_w")
	req.SetPathValue("alias", "nope")
	w = httptest.NewRecorder()
	h.HandleBindPluginApp(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown alias: %d %s", w.Code, w.Body.String())
	}
}
