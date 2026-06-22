package provider

import (
	"github.com/polarisagi/polaris/internal/store/repo"

	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/llm"
)

func TestCatalogHandlers_List(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS sys_providers (
			id TEXT PRIMARY KEY,
			display_name TEXT,
			provider_type TEXT,
			default_base_url TEXT,
			is_local INTEGER,
			display_order INTEGER
		);
		CREATE TABLE IF NOT EXISTS sys_provider_models (
			id TEXT PRIMARY KEY,
			catalog_provider_id TEXT,
			model_id TEXT,
			display_name TEXT,
			recommended_role TEXT,
			display_order INTEGER
		);
		INSERT INTO sys_providers (id, display_name, provider_type, default_base_url, is_local, display_order)
		VALUES ('p1', 'Provider 1', 'openai_compat', 'http://1', 0, 1);
		
		INSERT INTO sys_provider_models (id, catalog_provider_id, model_id, display_name, recommended_role, display_order)
		VALUES ('m1', 'p1', 'model1', 'Model 1', 'default', 1);
	`)
	if err != nil {
		t.Fatal(err)
	}

	h := &ProviderHandler{DB: db, ExtRepo: repo.NewSQLiteExtensionRepository(db), ProviderRepo: repo.NewSQLiteProviderRepository(db)}

	req := httptest.NewRequest("GET", "/api/v1/catalog/providers", nil)
	w := httptest.NewRecorder()

	h.HandleListCatalogProviders(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Result().StatusCode)
	}

	var res map[string]any
	if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCatalogHandlers_CreateFromCatalog(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS sys_providers (
			id TEXT PRIMARY KEY,
			display_name TEXT,
			provider_type TEXT,
			default_base_url TEXT,
			is_local INTEGER,
			display_order INTEGER
		);
		CREATE TABLE IF NOT EXISTS sys_provider_models (
			id TEXT PRIMARY KEY,
			catalog_provider_id TEXT,
			model_id TEXT,
			display_name TEXT,
			recommended_role TEXT,
			display_order INTEGER
		);
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
			catalog_id TEXT,
			created_at TEXT,
			updated_at TEXT
		);
		CREATE TABLE IF NOT EXISTS provider_models (
			id TEXT PRIMARY KEY,
			provider_id TEXT,
			model_id TEXT,
			name TEXT,
			role TEXT,
			enabled INTEGER,
			created_at TEXT,
			updated_at TEXT
		);
		INSERT INTO sys_providers (id, display_name, provider_type, default_base_url, is_local, display_order)
		VALUES ('p1', 'Provider 1', 'openai_compat', 'http://1', 0, 1);
		
		INSERT INTO sys_provider_models (id, catalog_provider_id, model_id, display_name, recommended_role, display_order)
		VALUES ('m1', 'p1', 'model1', 'Model 1', 'reasoning', 1);
	`)
	if err != nil {
		t.Fatal(err)
	}

	h := &ProviderHandler{
		DB:           db,
		ProviderRepo: repo.NewSQLiteProviderRepository(db),
		Registry:     llm.NewProviderRegistry(config.M1RouterThresholds{}),
	}

	// 1. Missing API Key
	body := `{"catalog_id": "p1"}`
	req := httptest.NewRequest("POST", "/api/v1/providers/from-catalog", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	h.HandleCreateProviderFromCatalog(w, req)
	if w.Result().StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for missing API key, got %d", w.Result().StatusCode)
	}

	// 2. Success
	body = `{"catalog_id": "p1", "api_key": "sec123"}`
	req = httptest.NewRequest("POST", "/api/v1/providers/from-catalog", bytes.NewBufferString(body))
	w = httptest.NewRecorder()

	h.HandleCreateProviderFromCatalog(w, req)
	if w.Result().StatusCode != http.StatusCreated {
		t.Errorf("expected 201, got %d: %s", w.Result().StatusCode, w.Body.String())
	}
}
