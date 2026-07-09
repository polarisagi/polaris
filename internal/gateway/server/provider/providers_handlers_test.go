package provider

import (
	"github.com/polarisagi/polaris/internal/store/repo"

	"bytes"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestProvidersHandlers(t *testing.T) {
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
			api_key TEXT DEFAULT '',
			base_url TEXT DEFAULT '',
			project_id TEXT DEFAULT '',
			location TEXT DEFAULT '',
			sa_key_json TEXT DEFAULT '',
			enabled INTEGER DEFAULT 0,
			status TEXT DEFAULT '',
			catalog_id TEXT DEFAULT '',
			config_json TEXT DEFAULT '',
			created_at DATETIME,
			updated_at DATETIME
		);
		CREATE TABLE IF NOT EXISTS provider_models (
			id TEXT PRIMARY KEY,
			provider_id TEXT,
			name TEXT,
			model_id TEXT,
			role TEXT DEFAULT '',
			enabled INTEGER DEFAULT 0,
			created_at DATETIME,
			updated_at DATETIME
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	db.Exec(`INSERT INTO providers (id, name, type, enabled, created_at, updated_at) VALUES ('1', 'openai', 'openai', 1, '2024', '2024')`)
	db.Exec(`INSERT INTO provider_models (id, provider_id, name, model_id, role, enabled, created_at, updated_at) VALUES ('1', '1', 'gpt-4', 'gpt-4', 'general', 1, '2024', '2024')`)

	h := &ProviderHandler{DB: db, ExtRepo: repo.NewSQLiteExtensionRepository(db), ProviderRepo: repo.NewSQLiteProviderRepository(db)}

	// List Providers
	req := httptest.NewRequest("GET", "/api/v1/providers", nil)
	w := httptest.NewRecorder()
	h.HandleListProviders(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("list providers failed: %v", w.Body.String())
	}

	// Create Provider
	body := `{"name": "openai", "api_key": "sk-123", "base_url": "https://api.openai.com/v1"}`
	req = httptest.NewRequest("POST", "/api/v1/providers", bytes.NewBufferString(body))
	w = httptest.NewRecorder()
	h.HandleCreateProvider(w, req)
	if w.Result().StatusCode != http.StatusCreated {
		t.Errorf("create provider failed: %v", w.Body.String())
	}

	// Update Provider
	body = `{"name": "openai", "is_enabled": true}`
	req = httptest.NewRequest("PUT", "/api/v1/providers/1", bytes.NewBufferString(body))
	req.SetPathValue("providerID", "1")
	w = httptest.NewRecorder()
	h.HandleUpdateProvider(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("update provider failed: %v", w.Body.String())
	}

	// Delete Provider
	req = httptest.NewRequest("DELETE", "/api/v1/providers/1", nil)
	req.SetPathValue("providerID", "1")
	w = httptest.NewRecorder()
	h.HandleDeleteProvider(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("delete provider failed: %v", w.Body.String())
	}

	// List Models
	req = httptest.NewRequest("GET", "/api/v1/providers/1/models", nil)
	req.SetPathValue("providerID", "1")
	w = httptest.NewRecorder()
	h.HandleListModels(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("list models failed: %v", w.Body.String())
	}

	// Create Model
	body = `{"name": "gpt-4", "model_id": "gpt-4", "role": "chat"}`
	req = httptest.NewRequest("POST", "/api/v1/providers/1/models", bytes.NewBufferString(body))
	req.SetPathValue("providerID", "1")
	w = httptest.NewRecorder()
	h.HandleCreateModel(w, req)
	if w.Result().StatusCode != http.StatusCreated {
		t.Errorf("create model failed: %v", w.Body.String())
	}

	// Update Model
	body = `{"enabled": true}`
	req = httptest.NewRequest("PUT", "/api/v1/providers/1/models/1", bytes.NewBufferString(body))
	req.SetPathValue("providerID", "1")
	req.SetPathValue("modelID", "1")
	w = httptest.NewRecorder()
	h.HandleUpdateModel(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("update model failed: %v", w.Body.String())
	}

	// Delete Model
	req = httptest.NewRequest("DELETE", "/api/v1/providers/1/models/1", nil)
	req.SetPathValue("providerID", "1")
	req.SetPathValue("modelID", "1")
	w = httptest.NewRecorder()
	h.HandleDeleteModel(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("delete model failed: %v", w.Body.String())
	}
}
