package sysadmin

import (
	cadapter "github.com/polarisagi/polaris/internal/channel/adapter"

	"github.com/polarisagi/polaris/internal/store/repo"

	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/llm"
)

func TestChannels_WebhookReceive(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS channels (
			id TEXT PRIMARY KEY,
			name TEXT,
			type TEXT,
			config_json TEXT,
			webhook_secret TEXT,
			enabled INTEGER
		);
		INSERT INTO channels (id, name, type, config_json, webhook_secret, enabled) VALUES ('ch1', 'test channel', 'slack', '{"signing_secret": "test_secret"}', '', 1);
	`)
	if err != nil {
		t.Fatal(err)
	}

	h := &SysAdminHandler{DB: db, ChatRepo: repo.NewSQLiteChatRepository(db), ExtRepo: repo.NewSQLiteExtensionRepository(db), ProviderRepo: repo.NewSQLiteProviderRepository(db)}

	// 1. Missing channel ID
	req := httptest.NewRequest("POST", "/api/v1/webhooks/receive/", bytes.NewBuffer([]byte(`{}`)))
	w := httptest.NewRecorder()
	h.HandleWebhookReceive(w, req)
	if w.Result().StatusCode != http.StatusNoContent {
		t.Errorf("expected 204 for missing channel ID, got %d", w.Result().StatusCode)
	}

	// 2. Missing channel in DB
	req = httptest.NewRequest("POST", "/api/v1/webhooks/receive/missing", bytes.NewBuffer([]byte(`{}`)))
	req.SetPathValue("id", "missing")
	w = httptest.NewRecorder()
	h.HandleWebhookReceive(w, req)
	if w.Result().StatusCode != http.StatusNoContent {
		t.Errorf("expected 204 for missing channel, got %d", w.Result().StatusCode)
	}

	// 3. Unauthorized request (invalid signature)
	req = httptest.NewRequest("POST", "/api/v1/webhooks/receive/ch1", bytes.NewBuffer([]byte(`{}`)))
	req.SetPathValue("channelID", "ch1")
	req.SetPathValue("channelType", "slack")
	w = httptest.NewRecorder()
	h.HandleWebhookReceive(w, req)
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 for unauthorized, got %d", w.Result().StatusCode)
	}
}

func TestTriggerWebhookAutomations(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := &SysAdminHandler{DB: db, ChatRepo: repo.NewSQLiteChatRepository(db), ExtRepo: repo.NewSQLiteExtensionRepository(db), ProviderRepo: repo.NewSQLiteProviderRepository(db)}

	// Create table
	_, _ = db.Exec(`CREATE TABLE automations (id TEXT, name TEXT, prompt TEXT, trigger_type TEXT, cron_schedule TEXT, channel_id TEXT, working_dir TEXT, reasoning_effort TEXT, result_action TEXT, sandbox_level INTEGER, cedar_rules_json TEXT, enabled INTEGER, last_run_status TEXT);`)

	h.triggerWebhookAutomations(context.Background(), "ch1", "{}")
}

func TestDispatchChannelMessage(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := llm.NewProviderRegistry(config.M1RouterThresholds{})
	fromConfig := &SysAdminHandler{DB: db, Registry: reg}

	fromConfig.dispatchChannelMessage(context.Background(), "slack", "ch1", map[string]any{}, cadapter.Message{})
}
