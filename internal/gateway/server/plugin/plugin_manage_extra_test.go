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
	// :memory: 每条连接都是独立空库（无 cache=shared），池开出第二条即读到空表。
	db.SetMaxOpenConns(1)
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
