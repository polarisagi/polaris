package sysadmin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
)

type canvasMemory struct {
	protocol.MemoryFacade
	gotSession string
}

func (c *canvasMemory) RenderTaskCanvas(_ context.Context, sessionID string) string {
	c.gotSession = sessionID
	return "graph LR\n  N1[\"x\"]\n"
}

type canvasAgent struct {
	protocol.AgentController
	mem protocol.MemoryFacade
}

func (a *canvasAgent) Memory() protocol.MemoryFacade { return a.mem }

func TestHandleGetMMDCanvas_RequiresSessionID(t *testing.T) {
	mem := &canvasMemory{}
	h := &SysAdminHandler{Agent: &canvasAgent{mem: mem}}

	rec := httptest.NewRecorder()
	h.HandleGetMMDCanvas(rec, httptest.NewRequest(http.MethodGet, "/v1/agent/mmd-canvas", nil))
	if rec.Code != http.StatusBadRequest || mem.gotSession != "" {
		t.Fatalf("缺 session_id 应 400 且不渲染: code=%d session=%q", rec.Code, mem.gotSession)
	}

	rec = httptest.NewRecorder()
	h.HandleGetMMDCanvas(rec, httptest.NewRequest(http.MethodGet, "/v1/agent/mmd-canvas?session_id=abc", nil))
	if rec.Code != http.StatusOK || mem.gotSession != "abc" || !strings.Contains(rec.Body.String(), "graph LR") {
		t.Fatalf("带 session_id 应按该会话渲染: code=%d session=%q body=%q", rec.Code, mem.gotSession, rec.Body.String())
	}
}
