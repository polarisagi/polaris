package plugin

import (
	"github.com/polarisagi/polaris/internal/store/repo"

	"database/sql"
	"testing"
)

func TestEnablePluginComponents(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec("CREATE TABLE mcp_servers (id TEXT PRIMARY KEY, enabled INTEGER)")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("CREATE TABLE skills (id TEXT PRIMARY KEY, enabled INTEGER)")
	if err != nil {
		t.Fatal(err)
	}
	_ = &PluginHandler{DB: db, ExtRepo: repo.NewSQLiteExtensionRepository(db)}
}
