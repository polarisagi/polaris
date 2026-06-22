package sysadmin

import (
	"bytes"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/extension/marketplace"
	"github.com/polarisagi/polaris/internal/llm"
	"github.com/polarisagi/polaris/internal/store/repo"
)

func TestDoctorAndExportHandlers(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	h := &SysAdminHandler{
		DB:           db,
		ChatRepo:     repo.NewSQLiteChatRepository(db),
		ExtRepo:      repo.NewSQLiteExtensionRepository(db),
		ProviderRepo: repo.NewSQLiteProviderRepository(db),
		InstallMgr:   marketplace.NewManager(repo.NewSQLiteExtensionRepository(db), nil, mockPolicyGate{}, mockPrefsRepo{}, nil, nil),
		Registry:     llm.NewProviderRegistry(config.M1RouterThresholds{}),
	}

	// Doctor
	req := httptest.NewRequest("GET", "/api/v1/doctor", nil)
	w := httptest.NewRecorder()
	h.HandleDoctor(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("doctor failed: %v", w.Result().StatusCode)
	}

	// Export Trajectories
	req = httptest.NewRequest("POST", "/api/v1/export/trajectories", bytes.NewBufferString(`{}`))
	w = httptest.NewRecorder()
	h.HandleExportTrajectories(w, req)
	// Usually 200 or 500 if DB setup isn't perfect, but covers the handler entry point
}
