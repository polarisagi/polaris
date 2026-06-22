package provider

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	_ "github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store/repo"
)

func TestSeedProvidersFromEnv(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS providers (
			id TEXT PRIMARY KEY,
			name TEXT,
			type TEXT,
			base_url TEXT,
			api_key TEXT,
			project_id TEXT,
			location TEXT,
			sa_key_json TEXT,
			enabled INTEGER,
			created_at DATETIME,
			updated_at DATETIME
		);
		CREATE TABLE IF NOT EXISTS provider_models (
			id TEXT PRIMARY KEY,
			provider_id TEXT,
			model_id TEXT,
			name TEXT,
			role TEXT,
			enabled INTEGER,
			created_at DATETIME,
			updated_at DATETIME
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	providerRepo := repo.NewSQLiteProviderRepository(db)

	// Set env vars
	os.Setenv("OPENAI_API_KEY", "sk-test-123")
	os.Setenv("ANTHROPIC_API_KEY", "sk-ant-test-456")

	SeedProvidersFromEnv(context.Background(), providerRepo)

	// Verify insertion
	var count int
	db.QueryRow("SELECT COUNT(*) FROM providers").Scan(&count)
	if count != 2 {
		t.Errorf("expected 2 providers seeded, got %d", count)
	}

	// Test update path
	os.Setenv("OPENAI_API_KEY", "sk-test-789")
	SeedProvidersFromEnv(context.Background(), providerRepo)

	var apiKey string
	db.QueryRow("SELECT api_key FROM providers WHERE id = 'prov_env_openai'").Scan(&apiKey)
	if apiKey != "sk-test-789" {
		t.Errorf("expected api_key to be updated to sk-test-789, got %s", apiKey)
	}

	// Clean up env vars
	os.Unsetenv("OPENAI_API_KEY")
	os.Unsetenv("ANTHROPIC_API_KEY")
}
