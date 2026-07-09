package sysadmin

import (
	"github.com/polarisagi/polaris/internal/store/repo"

	"bytes"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestBudgetHandlers(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS kv_store (
			key TEXT PRIMARY KEY,
			value TEXT,
			updated_at DATETIME
		);
		CREATE TABLE IF NOT EXISTS chat_sessions (
			id TEXT PRIMARY KEY,
			title TEXT,
			thrashing_index REAL,
			created_at DATETIME,
			updated_at DATETIME
		);
		CREATE TABLE IF NOT EXISTS chat_messages (
			id TEXT PRIMARY KEY,
			session_id TEXT,
			role TEXT,
			content TEXT,
			created_at DATETIME
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	h := &SysAdminHandler{
		DB:           db,
		ChatRepo:     repo.NewSQLiteChatRepository(db),
		ExtRepo:      repo.NewSQLiteExtensionRepository(db),
		ProviderRepo: repo.NewSQLiteProviderRepository(db),
		BudgetRepo:   repo.NewSQLiteBudgetRepository(db),
		SystemRepo:   repo.NewSQLiteSystemRepository(db),
		WorkflowRepo: repo.NewSQLiteWorkflowRepository(db),
	}

	// 1. Get Budget (empty)
	req := httptest.NewRequest("GET", "/v1/config/budget", nil)
	w := httptest.NewRecorder()
	h.HandleGetBudget(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK")
	}
	if !bytes.Contains(w.Body.Bytes(), []byte(`"monthly_usd":0`)) {
		t.Errorf("expected 0 budget initially")
	}

	// 2. Set Budget
	body := bytes.NewBufferString(`{"monthly_usd": 150.5}`)
	req = httptest.NewRequest("PUT", "/v1/config/budget", body)
	w = httptest.NewRecorder()
	h.HandleSetBudget(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK")
	}

	// 3. Get Budget (updated)
	req = httptest.NewRequest("GET", "/v1/config/budget", nil)
	w = httptest.NewRecorder()
	h.HandleGetBudget(w, req)
	if !bytes.Contains(w.Body.Bytes(), []byte(`"monthly_usd":150.5`)) {
		t.Errorf("expected 150.5 budget")
	}

	// 4. Export Backup
	db.Exec("INSERT INTO chat_sessions (id, title, thrashing_index, created_at, updated_at) VALUES ('s1', 'title', 0.5, 'now', 'now')")
	db.Exec("INSERT INTO chat_messages (id, session_id, role, content, created_at) VALUES ('m1', 's1', 'user', 'hello', 'now')")

	req = httptest.NewRequest("GET", "/v1/export/backup", nil)
	w = httptest.NewRecorder()
	h.HandleExportBackup(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK")
	}
	exportedData := w.Body.Bytes()
	if !bytes.Contains(exportedData, []byte(`"table":"chat_sessions"`)) {
		t.Errorf("expected chat_sessions in backup")
	}

	// 5. Import Backup
	// Clear db
	db.Exec("DELETE FROM chat_sessions")
	db.Exec("DELETE FROM chat_messages")
	db.Exec("DELETE FROM kv_store")

	req = httptest.NewRequest("POST", "/v1/import/backup", bytes.NewReader(exportedData))
	w = httptest.NewRecorder()
	h.HandleImportBackup(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK")
	}

	// verify import
	var count int
	db.QueryRow("SELECT COUNT(*) FROM chat_sessions").Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 session imported")
	}
	db.QueryRow("SELECT COUNT(*) FROM chat_messages").Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 message imported")
	}
}
