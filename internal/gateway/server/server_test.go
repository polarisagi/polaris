package server

import (
	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/store/repo"

	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/channel"
	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/llm"
)

func TestHandleStatus(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	s := &Server{
		db:           db,
		chatRepo:     repo.NewSQLiteChatRepository(db),
		extRepo:      repo.NewSQLiteExtensionRepository(db),
		providerRepo: repo.NewSQLiteProviderRepository(db),
		registry:     llm.NewProviderRegistry(config.M1RouterThresholds{}),
		channelMgr:   channel.NewManager(http.DefaultClient, nil),
	}

	req := httptest.NewRequest("GET", "/api/v1/status", nil)
	w := httptest.NewRecorder()

	s.handleStatus(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK, got %d", w.Result().StatusCode)
	}
}

func TestServerLifecycle(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	s := &Server{
		db:           db,
		chatRepo:     repo.NewSQLiteChatRepository(db),
		extRepo:      repo.NewSQLiteExtensionRepository(db),
		providerRepo: repo.NewSQLiteProviderRepository(db),
		registry:     llm.NewProviderRegistry(config.M1RouterThresholds{}),
		channelMgr:   channel.NewManager(http.DefaultClient, nil),
	}

	// This is a minimal coverage test for shutdown sequence
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	err = s.Shutdown(ctx)
	if err != nil {
		t.Errorf("unexpected error during shutdown: %v", err)
	}
}

func TestHandleHealthz(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()

	s.handleHealthz(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK")
	}
}
