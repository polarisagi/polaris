package repo

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/schema"
)

func newMarketplaceSeedTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	ddl, err := schema.FS.ReadFile("018_plugin_marketplaces.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatalf("exec marketplace schema: %v", err)
	}
	return db
}

func TestSeedMarketplace_UpdatesBuiltinURLOnly(t *testing.T) {
	ctx := context.Background()
	db := newMarketplaceSeedTestDB(t)
	r := NewSQLiteExtensionRepository(db)
	if _, err := db.Exec(`INSERT INTO plugin_marketplaces
		(id, name, type, publisher, repo_url, description, is_builtin, trust_tier, enabled, sort_order, created_at)
		VALUES ('official', 'Official', 'plugin', 'polarisagi', 'https://old.example', 'keep', 1, 2, 0, 41, 'old-time')`); err != nil {
		t.Fatal(err)
	}
	if err := r.SeedMarketplace(ctx, protocol.Marketplace{ID: "official", RepoURL: "https://new.example", TrustTier: 3}); err != nil {
		t.Fatal(err)
	}
	assertMarketplaceSeedState(t, db, "official", "https://new.example", 1, 2, 0, 41, "Official", "polarisagi", "keep", "old-time")
	if err := r.SeedMarketplace(ctx, protocol.Marketplace{ID: "official", RepoURL: "https://new.example", TrustTier: 3}); err != nil {
		t.Fatal(err)
	}
	assertMarketplaceSeedState(t, db, "official", "https://new.example", 1, 2, 0, 41, "Official", "polarisagi", "keep", "old-time")

	if _, err := db.Exec(`INSERT INTO plugin_marketplaces
		(id, name, type, publisher, repo_url, description, is_builtin, trust_tier, enabled, sort_order, created_at)
		VALUES ('custom', 'Custom', 'plugin', 'user', 'https://custom.example', 'custom-description', 0, 2, 1, 88, 'custom-time')`); err != nil {
		t.Fatal(err)
	}
	if err := r.SeedMarketplace(ctx, protocol.Marketplace{ID: "custom", RepoURL: "https://replacement.example", TrustTier: 3}); err != nil {
		t.Fatal(err)
	}
	assertMarketplaceSeedState(t, db, "custom", "https://custom.example", 0, 2, 1, 88, "Custom", "user", "custom-description", "custom-time")
}

func assertMarketplaceSeedState(t *testing.T, db *sql.DB, id, wantURL string, wantBuiltin, wantTrustTier, wantEnabled, wantSort int, wantName, wantPublisher, wantDescription, wantCreatedAt string) {
	t.Helper()
	var gotURL, gotName, gotPublisher, gotDescription, gotCreatedAt string
	var gotBuiltin, gotTrustTier, gotEnabled, gotSort int
	err := db.QueryRow(`SELECT repo_url, is_builtin, trust_tier, enabled, sort_order, name, publisher, description, created_at
		FROM plugin_marketplaces WHERE id=?`, id).Scan(&gotURL, &gotBuiltin, &gotTrustTier, &gotEnabled, &gotSort, &gotName, &gotPublisher, &gotDescription, &gotCreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if gotURL != wantURL || gotBuiltin != wantBuiltin || gotTrustTier != wantTrustTier || gotEnabled != wantEnabled || gotSort != wantSort || gotName != wantName || gotPublisher != wantPublisher || gotDescription != wantDescription || gotCreatedAt != wantCreatedAt {
		t.Fatalf("marketplace %q state = (%q, %d, %d, %d, %d, %q, %q, %q, %q), want (%q, %d, %d, %d, %d, %q, %q, %q, %q)",
			id, gotURL, gotBuiltin, gotTrustTier, gotEnabled, gotSort, gotName, gotPublisher, gotDescription, gotCreatedAt,
			wantURL, wantBuiltin, wantTrustTier, wantEnabled, wantSort, wantName, wantPublisher, wantDescription, wantCreatedAt)
	}
}
