package sysadmin

import (
	cadapter "github.com/polarisagi/polaris/internal/channel/adapter"

	"github.com/polarisagi/polaris/internal/store/repo"

	"bytes"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/channel"
)

func TestChannelsHandlers(t *testing.T) {
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
			enabled BOOLEAN,
			status TEXT,
			webhook_secret TEXT,
			created_at DATETIME,
			updated_at DATETIME
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	db.Exec(`INSERT INTO channels (id, name, type, enabled, config_json, status, webhook_secret, created_at, updated_at) VALUES ('1', 'test', 'telegram', 1, '{}', 'active', '', '2024-01-01', '2024-01-01')`)

	h := &SysAdminHandler{
		DB:           db,
		ChatRepo:     repo.NewSQLiteChatRepository(db),
		ExtRepo:      repo.NewSQLiteExtensionRepository(db),
		ProviderRepo: repo.NewSQLiteProviderRepository(db),
		ChannelRepo:  repo.NewSQLiteChannelRepository(db),
		ChannelMgr:   channel.NewManager(http.DefaultClient, func(channelType, channelID string, cfg map[string]any, msg cadapter.Message) {}),
	}

	// List
	req := httptest.NewRequest("GET", "/api/v1/channels", nil)
	w := httptest.NewRecorder()
	h.HandleListChannels(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("list channels failed: %v", w.Body.String())
	}

	// Create
	body := `{"type": "telegram", "config": {"token": "123"}}`
	req = httptest.NewRequest("POST", "/api/v1/channels", bytes.NewBufferString(body))
	w = httptest.NewRecorder()
	h.HandleCreateChannel(w, req)
	if w.Result().StatusCode != http.StatusCreated {
		t.Errorf("create channel failed: %v", w.Body.String())
	}

	// Update
	body = `{"config": {"token": "456"}, "is_enabled": true}`
	req = httptest.NewRequest("PUT", "/api/v1/channels/1", bytes.NewBufferString(body))
	req.SetPathValue("channelID", "1")
	w = httptest.NewRecorder()
	h.HandleUpdateChannel(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("update channel failed: %v", w.Body.String())
	}

	// Delete
	req = httptest.NewRequest("DELETE", "/api/v1/channels/1", nil)
	req.SetPathValue("channelID", "1")
	w = httptest.NewRecorder()
	h.HandleDeleteChannel(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("delete channel failed: %v", w.Body.String())
	}
}
