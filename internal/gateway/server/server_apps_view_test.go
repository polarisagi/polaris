package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestBuildMCPAppsCSP_Defaults(t *testing.T) {
	got := buildMCPAppsCSP(protocol.UICSP{})
	want := "default-src 'none'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data:; font-src 'self'; media-src 'self' data:; connect-src 'none'; " +
		"frame-src 'none'; object-src 'none'; base-uri 'self';"
	if got != want {
		t.Fatalf("default CSP mismatch:\n got %s\nwant %s", got, want)
	}
}

func TestBuildMCPAppsCSP_Domains(t *testing.T) {
	got := buildMCPAppsCSP(protocol.UICSP{
		ResourceDomains: []string{"https://cdn.example.com", "https://*.img.example.com:8443"},
		ConnectDomains:  []string{"https://api.example.com"},
		FrameDomains:    []string{"https://embed.example.com"},
		BaseURIDomains:  []string{"https://base.example.com"},
	})
	for _, frag := range []string{
		"script-src 'self' 'unsafe-inline' https://cdn.example.com https://*.img.example.com:8443;",
		"style-src 'self' 'unsafe-inline' https://cdn.example.com https://*.img.example.com:8443;",
		"img-src 'self' data: https://cdn.example.com https://*.img.example.com:8443;",
		"font-src 'self' https://cdn.example.com https://*.img.example.com:8443;",
		"media-src 'self' data: https://cdn.example.com https://*.img.example.com:8443;",
		"connect-src 'self' https://api.example.com;",
		"frame-src https://embed.example.com;",
		"base-uri https://base.example.com;",
		"object-src 'none';",
	} {
		if !strings.Contains(got, frag) {
			t.Errorf("CSP missing %q in %s", frag, got)
		}
	}
}

func TestBuildMCPAppsCSP_DropsIllegalDomains(t *testing.T) {
	got := buildMCPAppsCSP(protocol.UICSP{
		ConnectDomains: []string{
			"http://insecure.example.com", "https://a.com; script-src *", "https://a.com/path",
			"*", "https://", "https://x y.com", "https://ok.example.com",
		},
	})
	if !strings.Contains(got, "connect-src 'self' https://ok.example.com;") {
		t.Fatalf("only the legal domain may survive: %s", got)
	}
	if strings.Contains(got, "insecure") || strings.Contains(got, "script-src *") {
		t.Fatalf("illegal domains leaked: %s", got)
	}
}

func TestInjectOpenAIShim(t *testing.T) {
	cases := map[string]string{
		"head":    `<!doctype html><html><head><title>x</title></head><body>b</body></html>`,
		"htmlTag": `<html><body>b</body></html>`,
		"bare":    `<div>b</div>`,
	}
	for name, in := range cases {
		out := injectOpenAIShim(in)
		if !strings.Contains(out, "window.openai = openai") {
			t.Errorf("%s: shim not injected", name)
		}
		if strings.Contains(out, "Content-Security-Policy") {
			t.Errorf("%s: CSP must come from the response header, not a meta", name)
		}
		if strings.Contains(strings.ToLower(in), "<head") {
			// 垫片必须先于 View 自己的 <title>/脚本
			if strings.Index(out, "<script>") > strings.Index(out, "<title>") {
				t.Errorf("%s: shim must be the first thing in <head>", name)
			}
		}
	}
}

func TestViewTicketStore_OneShotAndExpiry(t *testing.T) {
	now := time.Now()
	st := &mcpAppsViewTicketStore{now: func() time.Time { return now }}
	tok, err := st.issue("<doc>", "csp")
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) < 32 {
		t.Fatalf("token entropy too small: %q", tok)
	}
	if got, o := st.take(tok); o != ticketOutcomeHit || got.doc != "<doc>" {
		t.Fatalf("first take must hit: %s", o)
	}
	if _, o := st.take(tok); o != ticketOutcomeMiss {
		t.Fatalf("second take must miss, got %s", o)
	}
	tok2, _ := st.issue("d", "c")
	now = now.Add(mcpAppsViewTicketTTL + time.Second)
	if _, o := st.take(tok2); o != ticketOutcomeExpired {
		t.Fatalf("expired ticket must be rejected, got %s", o)
	}
}

func TestViewTicketStore_BoundedAndPurges(t *testing.T) {
	now := time.Now()
	st := &mcpAppsViewTicketStore{now: func() time.Time { return now }}
	for i := 0; i < mcpAppsViewTicketMax; i++ {
		if _, err := st.issue("d", "c"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.issue("d", "c"); err == nil {
		t.Fatal("store must refuse beyond the cap")
	}
	now = now.Add(mcpAppsViewTicketTTL + time.Second)
	if _, err := st.issue("d", "c"); err != nil {
		t.Fatalf("expired tickets must be purged on issue: %v", err)
	}
	if len(st.tickets) != 1 {
		t.Fatalf("expected purge down to 1, got %d", len(st.tickets))
	}
}

func postViewDocument(t *testing.T, s *Server, viewID, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/mcp-apps/views/{viewID}/document", s.handleMCPAppsViewDocument)
	req := httptest.NewRequest(http.MethodPost, "/v1/mcp-apps/views/"+viewID+"/document", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestHandleMCPAppsViewDocument_IssuesTicketAndServes(t *testing.T) {
	s, fakeMgr := newMCPAppsTestServer(t)
	prefersBorder := false
	fakeMgr.uiResource = protocol.UIResource{
		HTML:          `<html><head><title>t</title></head><body><script>1</script></body></html>`,
		CSP:           protocol.UICSP{ConnectDomains: []string{"https://api.example.com"}},
		Permissions:   protocol.UIPermissions{Camera: true},
		PrefersBorder: &prefersBorder,
	}
	rec := postViewDocument(t, s, "view-1", `{"session_id":"sess-1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	token, _ := body["token"].(string)
	if token == "" || body["prefers_border"] != false {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
	if _, leaked := body["html"]; leaked {
		t.Fatal("html must not be returned to the browser host any more")
	}
	if perm, _ := body["permissions"].(map[string]any); perm["Camera"] != true {
		t.Fatalf("permissions must pass through: %s", rec.Body.String())
	}

	// 沙箱源 /view：精确 CSP + no-store + 文档含垫片；二次取用 404。
	s.appsSandboxCfg.SandboxOrigin = ""
	view := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/view?t="+token, nil)
		req.Host = "127.0.0.1:28889"
		w := httptest.NewRecorder()
		s.handleMCPAppsViewPage(w, req)
		return w
	}
	w := view()
	if w.Code != http.StatusOK {
		t.Fatalf("view expected 200, got %d", w.Code)
	}
	csp := w.Header().Get("Content-Security-Policy")
	wantPrefix := buildMCPAppsCSP(fakeMgr.uiResource.CSP) + " frame-ancestors "
	if !strings.HasPrefix(csp, wantPrefix) {
		t.Fatalf("CSP header mismatch: %s", csp)
	}
	// 祖先链含沙箱代理页与顶层宿主页：两者都必须在 frame-ancestors 内。
	for _, a := range []string{"http://127.0.0.1:28889", "https://127.0.0.1:28889", "http://127.0.0.1:28888"} {
		if !strings.Contains(csp, a) {
			t.Errorf("frame-ancestors missing %s: %s", a, csp)
		}
	}
	for k, v := range map[string]string{
		"Cache-Control": "no-store", "X-Content-Type-Options": "nosniff",
		"Referrer-Policy": "no-referrer", "Content-Type": "text/html; charset=utf-8",
	} {
		if w.Header().Get(k) != v {
			t.Errorf("%s = %q, want %q", k, w.Header().Get(k), v)
		}
	}
	if !strings.Contains(w.Body.String(), "window.openai = openai") || !strings.Contains(w.Body.String(), "<body>") {
		t.Errorf("served document must contain shim and original body")
	}
	if again := view(); again.Code != http.StatusNotFound {
		t.Fatalf("ticket must be one-shot, got %d", again.Code)
	}
}

func TestHandleMCPAppsViewPage_UnknownAndExpired(t *testing.T) {
	s, _ := newMCPAppsTestServer(t)
	for _, q := range []string{"/view", "/view?t=", "/view?t=nope"} {
		w := httptest.NewRecorder()
		s.handleMCPAppsViewPage(w, httptest.NewRequest(http.MethodGet, q, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s expected 404, got %d", q, w.Code)
		}
	}
	now := time.Now()
	s.appsViewTickets.now = func() time.Time { return now }
	tok, _ := s.appsViewTickets.issue("d", "c")
	now = now.Add(mcpAppsViewTicketTTL + time.Second)
	w := httptest.NewRecorder()
	s.handleMCPAppsViewPage(w, httptest.NewRequest(http.MethodGet, "/view?t="+tok, nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("expired ticket expected 404, got %d", w.Code)
	}
}

func TestHandleMCPAppsViewDocument_Authorization(t *testing.T) {
	s, fakeMgr := newMCPAppsTestServer(t)
	fakeMgr.uiResource = protocol.UIResource{HTML: "<p>x</p>"}

	if rec := postViewDocument(t, s, "view-1", `{"session_id":"other"}`); rec.Code != http.StatusConflict {
		t.Errorf("wrong session expected 409, got %d", rec.Code)
	}
	if rec := postViewDocument(t, s, "view-1", `{}`); rec.Code != http.StatusConflict {
		t.Errorf("missing session expected 409, got %d", rec.Code)
	}
	if rec := postViewDocument(t, s, "nope", `{"session_id":"sess-1"}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown view expected 404, got %d", rec.Code)
	}
	if rec := postViewDocument(t, s, "view-1", `not json`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad body expected 400, got %d", rec.Code)
	}
	fakeMgr.connected["srv1"] = false
	if rec := postViewDocument(t, s, "view-1", `{"session_id":"sess-1"}`); rec.Code != http.StatusNotFound {
		t.Errorf("disconnected server expected 404, got %d", rec.Code)
	}
	if len(s.appsViewTickets.tickets) != 0 {
		t.Errorf("no ticket may be issued on failed authorization")
	}
}

func TestHandleMCPAppsViewDocument_PluginSettingsView(t *testing.T) {
	s, fakeMgr := newMCPAppsTestServer(t)
	fakeMgr.uiResource = protocol.UIResource{HTML: "<p>x</p>"}
	if err := s.chatRepo.SaveAppView(t.Context(), types.ChatAppViewRow{
		ViewID: "view-plugin", PluginID: "plug-1", ServerID: "srv1",
		ResourceURI: "ui://srv1/setup", ToolName: "setup", ToolInput: "{}", ToolResult: "{}", WidgetState: "{}",
	}); err != nil {
		t.Fatal(err)
	}
	if rec := postViewDocument(t, s, "view-plugin", `{"session_id":"sess-1"}`); rec.Code != http.StatusConflict {
		t.Errorf("plugin settings view must reject a session, got %d", rec.Code)
	}
	if rec := postViewDocument(t, s, "view-plugin", `{}`); rec.Code != http.StatusOK {
		t.Errorf("plugin settings view without session expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleMCPAppsViewDocument_Disabled(t *testing.T) {
	s, _ := newMCPAppsTestServer(t)
	s.appsSandboxCfg.Enabled = false
	if rec := postViewDocument(t, s, "view-1", `{"session_id":"sess-1"}`); rec.Code != http.StatusNotImplemented {
		t.Errorf("expected 501, got %d", rec.Code)
	}
}
