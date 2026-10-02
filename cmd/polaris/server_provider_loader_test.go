package main

import (
	"context"
	"database/sql"
	"net/http"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/llm"
)

func TestLoadProvidersFromDB_Main(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// :memory: 每条连接都是独立空库（无 cache=shared），池开出第二条即读到空表。
	db.SetMaxOpenConns(1)
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

	err = LoadProvidersFromDB(context.Background(), db, nil, reg, client, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if p := reg.PickProvider("general"); p == nil {
		t.Errorf("expected general provider to be loaded")
	}
}

// ADR-0109 D6：未填 role 的本地对话 Provider（ollama）默认归入 general（成本中档），
// 不得与远程便宜档同档竞争后台调用；未填 role 的远程 Provider 保持便宜档语义不变。
func TestLoadProvidersFromDB_EmptyRoleDefaultsOnlyForLocal(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	if _, err := db.Exec(`
		CREATE TABLE providers (id TEXT PRIMARY KEY, name TEXT, type TEXT, base_url TEXT, api_key TEXT,
			project_id TEXT, location TEXT, sa_key_json TEXT, enabled INTEGER, created_at DATETIME, updated_at DATETIME);
		CREATE TABLE provider_models (id TEXT PRIMARY KEY, provider_id TEXT, model_id TEXT, name TEXT, role TEXT,
			enabled INTEGER, created_at DATETIME, updated_at DATETIME);
		INSERT INTO providers (id, name, type, base_url, api_key, enabled) VALUES ('p1', 'Remote', 'openai_compat', 'https://api.example.com', 'sk', 1);
		INSERT INTO provider_models (id, provider_id, model_id, name, role, enabled) VALUES ('m1remote', 'p1', 'flash', 'Flash', '', 1);
		INSERT INTO providers (id, name, type, base_url, api_key, enabled) VALUES ('p2', 'Local', 'ollama', '', '', 1);
		INSERT INTO provider_models (id, provider_id, model_id, name, role, enabled) VALUES ('m2local0', 'p2', 'llama3', 'Llama', '', 1);
	`); err != nil {
		t.Fatal(err)
	}

	reg := llm.NewProviderRegistry(config.M1RouterThresholds{})
	if err := LoadProvidersFromDB(context.Background(), db, nil, reg, &http.Client{}, nil); err != nil {
		t.Fatalf("LoadProvidersFromDB: %v", err)
	}
	// 未指定池的后台调用按成本档位从低到高择优：远程空 role（便宜档）必须胜出，
	// 本地 ollama 已被归入 general（中档）。多跑几次排除 map 遍历随机性。
	for i := 0; i < 20; i++ {
		if got := reg.PickProviderName(""); got != "[Remote] Flash" {
			t.Fatalf("unpooled pick = %q, want remote cheap-tier provider", got)
		}
	}
}
