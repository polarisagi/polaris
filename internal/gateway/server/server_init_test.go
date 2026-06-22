package server

import (
	"context"
	"database/sql"
	"net/http"
	"os"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/extension/marketplace"
	"github.com/polarisagi/polaris/internal/store/repo"
)

func TestServerInitExtra(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS sys_config (
			key TEXT PRIMARY KEY,
			value TEXT,
			updated_at DATETIME
		);
		CREATE TABLE IF NOT EXISTS plugin_marketplaces (
			id TEXT PRIMARY KEY,
			name TEXT,
			repo_url TEXT,
			description TEXT,
			type TEXT,
			is_builtin BOOLEAN,
			trust_tier INTEGER,
			enabled BOOLEAN,
			sort_order INTEGER,
			publisher TEXT,
			status TEXT,
			last_sync_at DATETIME,
			created_at DATETIME,
			updated_at DATETIME
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	s := &Server{
		db:           db,
		chatRepo:     repo.NewSQLiteChatRepository(db),
		extRepo:      repo.NewSQLiteExtensionRepository(db),
		providerRepo: repo.NewSQLiteProviderRepository(db),
		installMgr:   marketplace.NewManager(repo.NewSQLiteExtensionRepository(db), nil, nil, nil, nil, nil),
	}
	seedBuiltinConfig(s)
	s.bootMarketplaceInit(context.Background())
	s.InitSTTEngine(context.Background(), os.TempDir(), nil, http.DefaultClient, config.STTConfig{})
	s.InitTTSEngine(context.Background(), os.TempDir(), nil, http.DefaultClient, config.TTSConfig{})
}
