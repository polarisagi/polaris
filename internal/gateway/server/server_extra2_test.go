package server

import (
	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin"

	"github.com/polarisagi/polaris/internal/store/repo"

	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/polarisagi/polaris/internal/channel"
)

func TestHandleAgentQuery(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest("POST", "/api/v1/agent/query", bytes.NewBufferString(`{"query": "test"}`))
	w := httptest.NewRecorder()
	s.handleAgentQuery(w, req)
}

func TestHandleGetAgentTask(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest("GET", "/api/v1/agent/task?session_id=123", nil)
	w := httptest.NewRecorder()
	s.handleGetAgentTask(w, req)
}

func TestHandleEvalRun(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest("POST", "/api/v1/agent/eval", bytes.NewBufferString(`{"suite": "test"}`))
	w := httptest.NewRecorder()
	s.handleEvalRun(w, req)
}

func TestHandleGetPendingApprovals(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest("GET", "/api/v1/agent/approvals?session_id=123", nil)
	w := httptest.NewRecorder()
	s.handleGetPendingApprovals(w, req)
}

func TestHandleResolveApproval(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest("POST", "/api/v1/agent/approvals/resolve", bytes.NewBufferString(`{"action": "approve"}`))
	w := httptest.NewRecorder()
	s.handleResolveApproval(w, req)
}

func TestHandleAgentInterrupt(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest("POST", "/api/v1/agent/interrupt", bytes.NewBufferString(`{"action": "pause"}`))
	w := httptest.NewRecorder()
	s.handleAgentInterrupt(w, req)
}

func TestServerStart(t *testing.T) {
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
		hooks:        sysadmin.NewHookRunner(t.TempDir()),
		channelMgr:   channel.NewManager(http.DefaultClient, nil),
		srv:          &http.Server{},
	}

	err = s.Start()
	if err != nil && err != http.ErrServerClosed && err != context.Canceled {
		t.Logf("Expected some error or nil, got: %v", err)
	}
}
