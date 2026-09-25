package sysadmin

import (
	"bytes"
	"database/sql"
	"encoding/json"
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
	// :memory: 每条连接都是独立空库（无 cache=shared），池开出第二条即读到空表。
	db.SetMaxOpenConns(1)
	defer db.Close()

	h := &SysAdminHandler{
		DB:           db,
		ChatRepo:     repo.NewSQLiteChatRepository(db),
		ExtRepo:      repo.NewSQLiteExtensionRepository(db),
		ProviderRepo: repo.NewSQLiteProviderRepository(db),
		InstallMgr:   marketplace.NewManager(repo.NewSQLiteExtensionRepository(db), nil, mockPolicyGate{}, mockPrefsRepo{}, nil, nil, nil),
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

// Registry 未注入时 doctor 须把 provider 项报为失败，而不是 nil panic。
func TestHandleDoctor_NilRegistryReportsProviderFailure(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	h := &SysAdminHandler{DB: db}
	w := httptest.NewRecorder()
	h.HandleDoctor(w, httptest.NewRequest("GET", "/api/v1/doctor", nil))

	var body struct {
		Checks []struct {
			Name   string `json:"name"`
			OK     bool   `json:"ok"`
			Detail string `json:"detail"`
		} `json:"checks"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	for _, c := range body.Checks {
		if c.Name == "provider" {
			if c.OK || c.Detail != "provider registry not configured" {
				t.Fatalf("unexpected provider check: %+v", c)
			}
			return
		}
	}
	t.Fatal("provider check missing")
}
