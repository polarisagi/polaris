package modelcatalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/security/network"
)

func testSafeClient(c *http.Client) network.SafeHTTPClient {
	safe := network.NewSafeHTTPClient(nil)
	safe.Client = c
	return safe
}

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	schema := `
	CREATE TABLE providers (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		type TEXT NOT NULL,
		base_url TEXT NOT NULL DEFAULT '',
		api_key TEXT NOT NULL DEFAULT '',
		project_id TEXT NOT NULL DEFAULT '',
		location TEXT NOT NULL DEFAULT '',
		enabled INTEGER NOT NULL DEFAULT 1
	);
	CREATE TABLE provider_discovered_models (
		provider_id           TEXT NOT NULL,
		model_id              TEXT NOT NULL,
		display_name          TEXT,
		context_window        INTEGER,
		max_output            INTEGER,
		supports_thinking     INTEGER,
		supports_vision       INTEGER,
		supports_tools        INTEGER,
		input_price_per_mtok  REAL,
		output_price_per_mtok REAL,
		raw_json              TEXT,
		discovered_at         TEXT NOT NULL,
		last_seen_at          TEXT NOT NULL,
		PRIMARY KEY (provider_id, model_id)
	);`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("create test schema: %v", err)
	}
	return db
}

func TestProbeOpenAICompat(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/v1/models" || r.URL.Path == "/models" {
			resp := map[string]any{
				"data": []map[string]any{
					{"id": "gpt-4o", "owned_by": "openai"},
					{"id": "deepseek-r1", "owned_by": "deepseek"},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	p := ProviderInfo{
		ID:      "p-openai",
		Type:    "openai_compat",
		BaseURL: ts.URL,
		APIKey:  "test-key",
	}

	models, err := ProbeProviderModels(context.Background(), testSafeClient(ts.Client()), p)
	if err != nil {
		t.Fatalf("ProbeProviderModels failed: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}

	var found4o, foundR1 bool
	for _, m := range models {
		if m.ModelID == "gpt-4o" {
			found4o = true
			if m.SupportsVision == nil || !*m.SupportsVision {
				t.Errorf("gpt-4o should support vision")
			}
			if m.ContextWindow != 128000 {
				t.Errorf("expected 128000 context window, got %d", m.ContextWindow)
			}
		}
		if m.ModelID == "deepseek-r1" {
			foundR1 = true
			if m.SupportsThinking == nil || !*m.SupportsThinking {
				t.Errorf("deepseek-r1 should support thinking")
			}
		}
	}
	if !found4o || !foundR1 {
		t.Errorf("expected to find both gpt-4o and deepseek-r1")
	}
}

func TestProbeAnthropic(t *testing.T) {
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "anth-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		callCount++
		if r.URL.Query().Get("after_id") == "" {
			resp := map[string]any{
				"data": []map[string]any{
					{"id": "claude-3-5-sonnet-20241022", "display_name": "Claude 3.5 Sonnet"},
				},
				"has_more": true,
				"last_id":  "claude-3-5-sonnet-20241022",
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		// 第二页
		resp := map[string]any{
			"data": []map[string]any{
				{"id": "claude-3-7-sonnet-thinking", "display_name": "Claude 3.7 Sonnet"},
			},
			"has_more": false,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	p := ProviderInfo{
		ID:      "p-anthropic",
		Type:    "anthropic",
		BaseURL: ts.URL,
		APIKey:  "anth-key",
	}

	models, err := ProbeProviderModels(context.Background(), testSafeClient(ts.Client()), p)
	if err != nil {
		t.Fatalf("ProbeProviderModels anthropic failed: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("expected 2 models with pagination, got %d", len(models))
	}
	if callCount != 2 {
		t.Errorf("expected 2 page requests, got %d", callCount)
	}
	if models[1].SupportsThinking == nil || !*models[1].SupportsThinking {
		t.Errorf("claude-3-7-sonnet-thinking should support thinking")
	}
}

func TestProbeGoogle(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("key") != "goog-key" {
			http.Error(w, "missing key", http.StatusBadRequest)
			return
		}
		resp := map[string]any{
			"models": []map[string]any{
				{
					"name":            "models/gemini-2.0-flash",
					"displayName":     "Gemini 2.0 Flash",
					"inputTokenLimit": 1048576,
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	// 覆写测试中 google 探测请求 URL
	p := ProviderInfo{
		ID:     "p-goog",
		Type:   "google_agent_platform",
		APIKey: "goog-key",
	}

	origTransport := ts.Client().Transport
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			newReq, _ := http.NewRequestWithContext(req.Context(), "GET", ts.URL+"?key="+req.URL.Query().Get("key"), nil)
			return origTransport.RoundTrip(newReq)
		}),
	}

	models, err := ProbeProviderModels(context.Background(), testSafeClient(client), p)
	if err != nil {
		t.Fatalf("ProbeProviderModels google failed: %v", err)
	}
	if len(models) != 1 || models[0].ModelID != "gemini-2.0-flash" {
		t.Fatalf("unexpected google models: %+v", models)
	}
	if models[0].ContextWindow != 1048576 {
		t.Errorf("expected 1048576 context window, got %d", models[0].ContextWindow)
	}
}

func TestProbeOllama(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		resp := map[string]any{
			"models": []map[string]any{
				{"name": "llama3.1:8b", "model": "llama3.1:8b"},
				{"name": "deepseek-r1:14b", "model": "deepseek-r1:14b"},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	p := ProviderInfo{
		ID:      "p-ollama",
		Type:    "ollama",
		BaseURL: ts.URL,
	}

	models, err := ProbeProviderModels(context.Background(), testSafeClient(ts.Client()), p)
	if err != nil {
		t.Fatalf("ProbeProviderModels ollama failed: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("expected 2 ollama models, got %d", len(models))
	}
	if models[1].SupportsThinking == nil || !*models[1].SupportsThinking {
		t.Errorf("deepseek-r1 should support thinking")
	}
}

func TestCatalogManager_RefreshAndList(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"data": []map[string]any{
				{"id": "gpt-4o"},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	_, _ = db.Exec(`INSERT INTO providers (id, name, type, base_url, api_key, enabled) VALUES ('p1', 'OpenAI', 'openai_compat', ?, 'key', 1)`, ts.URL)

	mgr := NewCatalogManager(db, nil, testSafeClient(ts.Client()))

	ctx := context.Background()
	discovered, err := mgr.RefreshProvider(ctx, "p1")
	if err != nil {
		t.Fatalf("RefreshProvider failed: %v", err)
	}
	if len(discovered) != 1 || discovered[0].ModelID != "gpt-4o" {
		t.Fatalf("unexpected discovered models: %+v", discovered)
	}

	list, err := mgr.ListDiscoveredModels(ctx, "p1")
	if err != nil {
		t.Fatalf("ListDiscoveredModels failed: %v", err)
	}
	if len(list) != 1 || list[0].ModelID != "gpt-4o" {
		t.Fatalf("unexpected list models: %+v", list)
	}
}

type roundTripFunc func(req *http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
