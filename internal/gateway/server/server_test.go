package server

import (
	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/store/repo"

	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/channel"
	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/llm"
	"github.com/polarisagi/polaris/internal/observability/probe"
)

func TestHandleStatus(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// :memory: 每条连接都是独立空库（无 cache=shared），池开出第二条即读到空表。
	db.SetMaxOpenConns(1)
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
	// :memory: 每条连接都是独立空库（无 cache=shared），池开出第二条即读到空表。
	db.SetMaxOpenConns(1)
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

func TestHandleGetCapabilities(t *testing.T) {
	s := &Server{}
	s.SetTier(1, probe.TierParameters{TTSPrefetchCount: 2})

	req := httptest.NewRequest("GET", "/v1/system/capabilities", nil)
	w := httptest.NewRecorder()

	s.handleGetCapabilities(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", w.Result().StatusCode)
	}

	var res map[string]any
	if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}

	if int(res["tts_prefetch_count"].(float64)) != 2 {
		t.Errorf("expected tts_prefetch_count=2, got %v", res["tts_prefetch_count"])
	}
	if int(res["hardware_tier"].(float64)) != 1 {
		t.Errorf("expected hardware_tier=1, got %v", res["hardware_tier"])
	}
}
