package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/types"
)

// fakeMCPAppsManager 覆盖 MCP Apps 四个新方法的可控假实现，其余方法继承
// mockOAuthCallbackMCPManager 的 no-op 桩（同包已有，见 oauth_mcp_handlers_test.go）。
type fakeMCPAppsManager struct {
	mockOAuthCallbackMCPManager

	connected map[string]bool

	uiResource    protocol.UIResource
	uiResourceErr error

	lastCallServerID, lastCallTool, lastCallSession, lastCallView string
	lastCallArgs                                                  map[string]any
	callToolRaw                                                   json.RawMessage
	callToolErr                                                   error

	lastReadServerID, lastReadURI string
	resourceContents              []protocol.MCPResourceContent
	resourceErr                   error
}

func (f *fakeMCPAppsManager) IsServerConnected(serverID string) bool { return f.connected[serverID] }

func (f *fakeMCPAppsManager) ReadUIResource(context.Context, string, string) (protocol.UIResource, error) {
	return f.uiResource, f.uiResourceErr
}

func (f *fakeMCPAppsManager) CallToolAsApp(_ context.Context, serverID, toolName string, args map[string]any, sessionID, viewID string) (json.RawMessage, error) {
	f.lastCallServerID, f.lastCallTool, f.lastCallSession, f.lastCallView, f.lastCallArgs = serverID, toolName, sessionID, viewID, args
	return f.callToolRaw, f.callToolErr
}

func (f *fakeMCPAppsManager) ReadResourceAsApp(_ context.Context, serverID, uri string) ([]protocol.MCPResourceContent, error) {
	f.lastReadServerID, f.lastReadURI = serverID, uri
	return f.resourceContents, f.resourceErr
}

func newMCPAppsTestServer(t *testing.T) (*Server, *fakeMCPAppsManager) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
	ddl, err := schema.FS.ReadFile("013_chat.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatal(err)
	}
	chatRepo := repo.NewSQLiteChatRepository(db)
	if err := chatRepo.CreateSession(context.Background(), types.ChatSessionRow{ID: "sess-1"}); err != nil {
		t.Fatal(err)
	}
	if err := chatRepo.SaveAppView(context.Background(), types.ChatAppViewRow{
		ViewID: "view-1", SessionID: "sess-1", ServerID: "srv1",
		ResourceURI: "ui://srv1/dash", ToolName: "dash", ToolInput: "{}", ToolResult: "{}", WidgetState: "{}",
	}); err != nil {
		t.Fatal(err)
	}

	fakeMgr := &fakeMCPAppsManager{connected: map[string]bool{"srv1": true}}
	s := &Server{
		chatRepo: chatRepo,
		mcpMgr:   fakeMgr,
		appsSandboxCfg: MCPAppsSandboxConfig{
			Enabled: true, SandboxOrigin: "http://127.0.0.1:28889", MainPort: 28888,
		},
	}
	return s, fakeMgr
}

func TestHandleGetMCPAppsConfig(t *testing.T) {
	s, _ := newMCPAppsTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/mcp-apps/config", nil)
	rec := httptest.NewRecorder()
	s.handleGetMCPAppsConfig(rec, req)

	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["sandbox_origin"] != "http://127.0.0.1:28889" || body["enabled"] != true {
		t.Fatalf("unexpected config body: %+v", body)
	}
}

func TestHandleGetMCPAppsResource(t *testing.T) {
	s, fakeMgr := newMCPAppsTestServer(t)
	prefersBorder := true
	fakeMgr.uiResource = protocol.UIResource{
		HTML: "<html/>", MimeType: "text/html;profile=mcp-app",
		CSP:           protocol.UICSP{ConnectDomains: []string{"https://api.example"}},
		PrefersBorder: &prefersBorder,
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/mcp-apps/resource?server_id=srv1&uri=ui://srv1/dash", nil)
	rec := httptest.NewRecorder()
	s.handleGetMCPAppsResource(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["html"] != "<html/>" || body["mime_type"] != "text/html;profile=mcp-app" || body["prefers_border"] != true {
		t.Fatalf("unexpected resource body: %+v", body)
	}
}

func TestHandleGetMCPAppsResource_MissingParams(t *testing.T) {
	s, _ := newMCPAppsTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/mcp-apps/resource", nil)
	rec := httptest.NewRecorder()
	s.handleGetMCPAppsResource(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func mustPostViewRPC(t *testing.T, s *Server, viewID, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/mcp-apps/views/{viewID}/rpc", s.handleMCPAppsViewRPC)
	req := httptest.NewRequest(http.MethodPost, "/v1/mcp-apps/views/"+viewID+"/rpc", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestHandleMCPAppsViewRPC_Ping(t *testing.T) {
	s, _ := newMCPAppsTestServer(t)
	rec := mustPostViewRPC(t, s, "view-1", `{"session_id":"sess-1","jsonrpc":"2.0","id":1,"method":"ping"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp mcpAppsRPCResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil || string(resp.Result) != "{}" {
		t.Fatalf("unexpected ping response: %+v result=%s", resp.Error, resp.Result)
	}
}

func TestHandleMCPAppsViewRPC_ToolCall_RoutesThroughCallToolAsApp(t *testing.T) {
	s, fakeMgr := newMCPAppsTestServer(t)
	fakeMgr.callToolRaw = json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`)

	rec := mustPostViewRPC(t, s, "view-1",
		`{"session_id":"sess-1","jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"refresh","arguments":{"x":1}}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if fakeMgr.lastCallServerID != "srv1" || fakeMgr.lastCallTool != "refresh" ||
		fakeMgr.lastCallSession != "sess-1" || fakeMgr.lastCallView != "view-1" {
		t.Fatalf("CallToolAsApp not invoked with expected args: %+v", fakeMgr)
	}
	var resp mcpAppsRPCResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if !bytes.Equal(resp.Result, fakeMgr.callToolRaw) {
		t.Fatalf("result must be raw CallToolResult JSON passthrough: %s", resp.Result)
	}
}

func TestHandleMCPAppsViewRPC_ResourceRead(t *testing.T) {
	s, fakeMgr := newMCPAppsTestServer(t)
	fakeMgr.resourceContents = []protocol.MCPResourceContent{{URI: "ui://srv1/dash", MIMEType: "text/html;profile=mcp-app", Text: "<html/>"}}

	rec := mustPostViewRPC(t, s, "view-1",
		`{"session_id":"sess-1","jsonrpc":"2.0","id":3,"method":"resources/read","params":{"uri":"ui://srv1/dash"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if fakeMgr.lastReadServerID != "srv1" || fakeMgr.lastReadURI != "ui://srv1/dash" {
		t.Fatalf("ReadResourceAsApp not invoked with expected args: %+v", fakeMgr)
	}
}

func TestHandleMCPAppsViewRPC_UnknownMethod(t *testing.T) {
	s, _ := newMCPAppsTestServer(t)
	rec := mustPostViewRPC(t, s, "view-1", `{"session_id":"sess-1","jsonrpc":"2.0","id":4,"method":"ui/open-link"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("JSON-RPC errors still return HTTP 200: got %d", rec.Code)
	}
	var resp mcpAppsRPCResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil || resp.Error.Code != jsonRPCMethodNotFoundCode {
		t.Fatalf("expected -32601 method not found, got %+v", resp.Error)
	}
}

func TestHandleMCPAppsViewRPC_UnknownView404(t *testing.T) {
	s, _ := newMCPAppsTestServer(t)
	rec := mustPostViewRPC(t, s, "does-not-exist", `{"session_id":"sess-1","jsonrpc":"2.0","id":5,"method":"ping"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestHandleMCPAppsViewRPC_WrongSession409(t *testing.T) {
	s, _ := newMCPAppsTestServer(t)
	rec := mustPostViewRPC(t, s, "view-1", `{"session_id":"sess-OTHER","jsonrpc":"2.0","id":6,"method":"ping"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rec.Code)
	}
}

func TestHandleMCPAppsViewRPC_ServerDisconnected404(t *testing.T) {
	s, fakeMgr := newMCPAppsTestServer(t)
	fakeMgr.connected["srv1"] = false
	rec := mustPostViewRPC(t, s, "view-1", `{"session_id":"sess-1","jsonrpc":"2.0","id":7,"method":"ping"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when server disconnected, got %d", rec.Code)
	}
}

func TestHandleMCPAppsViewState_PersistsAndRejectsOversized(t *testing.T) {
	s, _ := newMCPAppsTestServer(t)
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/mcp-apps/views/{viewID}/state", s.handleMCPAppsViewState)

	body := `{"session_id":"sess-1","widget_state":{"tab":"overview"}}`
	req := httptest.NewRequest(http.MethodPut, "/v1/mcp-apps/views/view-1/state", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	view, err := s.chatRepo.GetAppView(context.Background(), "view-1")
	if err != nil || view == nil || view.WidgetState != `{"tab":"overview"}` {
		t.Fatalf("widget_state not persisted: %+v (%v)", view, err)
	}

	oversized := `{"session_id":"sess-1","widget_state":"` + strings.Repeat("x", maxWidgetStateBytes) + `"}`
	req2 := httptest.NewRequest(http.MethodPut, "/v1/mcp-apps/views/view-1/state", strings.NewReader(oversized))
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for oversized widget_state, got %d", rec2.Code)
	}
}

func TestHandleMCPAppsViewModelContext_PersistsPerServer(t *testing.T) {
	s, _ := newMCPAppsTestServer(t)
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/mcp-apps/views/{viewID}/model-context", s.handleMCPAppsViewModelContext)

	body := `{"session_id":"sess-1","structuredContent":{"selected":"row-1"}}`
	req := httptest.NewRequest(http.MethodPut, "/v1/mcp-apps/views/view-1/model-context", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	raw, err := s.chatRepo.ConsumeSessionModelContext(context.Background(), "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, `"srv1"`) || !strings.Contains(raw, `"row-1"`) {
		t.Fatalf("expected pending model context keyed by server_id: %q", raw)
	}
	// 已被消费一次后应清空。
	raw2, err := s.chatRepo.ConsumeSessionModelContext(context.Background(), "sess-1")
	if err != nil || raw2 != "{}" {
		t.Fatalf("expected cleared after consume: %q (%v)", raw2, err)
	}
}
