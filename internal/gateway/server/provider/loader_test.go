package provider

import (
	"context"
	"database/sql"
	"net/http"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/llm"
)

func TestLoadProvidersFromDB(t *testing.T) {
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

	// Insert test data
	db.Exec("INSERT INTO providers (id, name, type, base_url, api_key, enabled) VALUES ('p1', 'OpenAI', 'openai_compat', 'https://api.openai.com', 'sk-xxx', 1)")
	db.Exec("INSERT INTO provider_models (id, provider_id, model_id, name, role, enabled) VALUES ('m1', 'p1', 'gpt-4o', 'GPT-4o', 'general', 1)")

	db.Exec("INSERT INTO providers (id, name, type, base_url, api_key, enabled) VALUES ('p2', 'Anthropic', 'anthropic', '', 'sk-ant-xxx', 1)")
	db.Exec("INSERT INTO provider_models (id, provider_id, model_id, name, role, enabled) VALUES ('m2', 'p2', 'claude-3-sonnet', 'Claude', 'general', 1)")

	db.Exec("INSERT INTO providers (id, name, type, project_id, location, sa_key_json, enabled) VALUES ('p3', 'Google', 'google_agent_platform', 'my-project', 'us-central1', '{}', 1)")
	db.Exec("INSERT INTO provider_models (id, provider_id, model_id, name, role, enabled) VALUES ('m3', 'p3', 'gemini-1.5-pro', 'Gemini', 'general', 1)")

	db.Exec("INSERT INTO providers (id, name, type, base_url, api_key, enabled) VALUES ('p4', 'Ollama', 'ollama', '', '', 1)")
	db.Exec("INSERT INTO provider_models (id, provider_id, model_id, name, role, enabled) VALUES ('m4', 'p4', 'llama3', 'Llama', 'general', 1)")

	reg := llm.NewProviderRegistry(config.M1RouterThresholds{})
	client := &http.Client{}

	err = LoadProvidersFromDB(context.Background(), db, reg, client, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if p := reg.PickProvider("general"); p == nil {
		t.Errorf("expected general provider to be loaded")
	}
}
