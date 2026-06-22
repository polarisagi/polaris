package sysadmin

import (
	"github.com/polarisagi/polaris/internal/store/repo"

	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/llm"
)

func TestCronNewRunID(t *testing.T) {
	id := newRunID()
	if id == "" {
		t.Errorf("expected non-empty run ID")
	}
}

func TestUpdateAutomationStats(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec("CREATE TABLE automations (id TEXT PRIMARY KEY, run_count INTEGER, success_count INTEGER, failure_count INTEGER, last_run_status TEXT, last_run_error TEXT, circuit_open INTEGER, circuit_opened_at DATETIME, next_run_at DATETIME, updated_at DATETIME)")
	if err != nil {
		t.Fatal(err)
	}

	h := &SysAdminHandler{
		DB:             db,
		ChatRepo:       repo.NewSQLiteChatRepository(db),
		ExtRepo:        repo.NewSQLiteExtensionRepository(db),
		ProviderRepo:   repo.NewSQLiteProviderRepository(db),
		AutomationRepo: repo.NewSQLiteAutomationRepository(db),
	}
	h.updateAutomationStats("test", "success", "error", "100")
}

func TestExecuteAutomation(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec("CREATE TABLE automations (id TEXT PRIMARY KEY, run_count INTEGER, success_count INTEGER, failure_count INTEGER, last_run_status TEXT, last_run_error TEXT, circuit_open INTEGER, circuit_opened_at DATETIME, next_run_at DATETIME, updated_at DATETIME, last_run_at DATETIME)")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("CREATE TABLE automation_runs (id TEXT PRIMARY KEY, automation_id TEXT, trigger TEXT, status TEXT, session_id TEXT, prompt_snapshot TEXT, started_at DATETIME, completed_at DATETIME, error TEXT)")
	if err != nil {
		t.Fatal(err)
	}

	h := &SysAdminHandler{
		DB:             db,
		AutomationRepo: repo.NewSQLiteAutomationRepository(db),
		Registry:       llm.NewProviderRegistry(config.M1RouterThresholds{}),
	}
	h.executeAutomation(context.Background(), &automation{ID: "test"}, "")

	// Wait a bit for the async goroutine to fail/finish gracefully
	time.Sleep(50 * time.Millisecond)
}
