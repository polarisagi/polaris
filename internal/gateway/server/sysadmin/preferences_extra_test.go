package sysadmin

import (
	"bytes"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store/repo"
)

type dummyAgent struct {
	protocol.AgentController
}

func (d *dummyAgent) SetPreferences(m map[string]string) {}
func (d *dummyAgent) Memory() protocol.Memory            { return nil }

func TestHandlePreferences(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS preferences (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	h := &SysAdminHandler{
		DB:         db,
		Agent:      &dummyAgent{},
		SystemRepo: repo.NewSQLiteSystemRepository(db),
	}

	// Get Pref
	req := httptest.NewRequest("GET", "/api/v1/preferences", nil)
	w := httptest.NewRecorder()
	h.HandleGetPreferences(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("get preferences failed")
	}

	// Set Pref
	body := `{"key": "test_key", "value": "test_value"}`
	req = httptest.NewRequest("POST", "/api/v1/preferences", bytes.NewBufferString(body))
	w = httptest.NewRecorder()
	h.HandleSetPreference(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("set preference failed")
	}
}
