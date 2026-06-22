package plugin

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/extension/marketplace"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store/repo"
)

func TestPluginCatalogCopyAndRegister(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS plugins (
			id TEXT PRIMARY KEY,
			name TEXT,
			display_name TEXT,
			description TEXT,
			publisher TEXT,
			version TEXT,
			trust_tier INTEGER,
			install_path TEXT,
			catalog_id TEXT,
			enabled INTEGER,
			mcp_policy TEXT,
			status TEXT,
			created_at DATETIME,
			updated_at DATETIME
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	h := &PluginHandler{
		DB:                   db,
		ExtRepo:              repo.NewSQLiteExtensionRepository(db),
		InstallMgr:           marketplace.NewManager(repo.NewSQLiteExtensionRepository(db), nil, &dummyPolicyGate{}, repo.NewSQLiteSystemRepository(db), nil, nil),
		ClearToolSchemaCache: func() {},
	}

	tempDir := t.TempDir()

	// Create dummy src dir
	srcDir := filepath.Join(tempDir, "src")
	os.MkdirAll(srcDir, 0755)
	os.WriteFile(filepath.Join(srcDir, "test.txt"), []byte("hello"), 0644)

	dstDir := filepath.Join(tempDir, "dst")

	err = copyDir(srcDir, dstDir)
	if err != nil {
		t.Errorf("copyDir failed: %v", err)
	}

	h.registerPluginSkills(context.Background(), "ext-1", "ext-1", dstDir, &protocol.PluginBundleManifest{}, 1)

	h.registerPluginMCPServers(context.Background(), "plug-1", "plug-1", dstDir, map[string]pluginMCPDef{}, 1, time.Now().Format(time.RFC3339))
}
