package plugin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/extension/lifecycle"
	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/internal/gateway/server/chat"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/types"
)

type appCall struct {
	name, viewID, sessionID string
	input                   string
}

type fakeAppTools struct {
	calls  []appCall
	result *types.ToolResult
	err    error
}

func (f *fakeAppTools) ExecuteAppTool(_ context.Context, name string, input []byte, _ types.TaintLevel, viewID, sessionID string) (*types.ToolResult, error) {
	f.calls = append(f.calls, appCall{name, viewID, sessionID, string(input)})
	return f.result, f.err
}

type fakeMCP struct{ servers []protocol.MCPServerInfo }

func (f fakeMCP) ListServers() []protocol.MCPServerInfo { return f.servers }
func (fakeMCP) Remove(string)                           {}

type fakeDeps struct{ blockErr error }

func (fakeDeps) Check(context.Context, string) ([]lifecycle.DependencyStatus, error) { return nil, nil }
func (f fakeDeps) EnableBlocker(context.Context, string) error                       { return f.blockErr }
func (fakeDeps) EnforceAll(context.Context) ([]string, error)                        { return nil, nil }

type setupEnv struct {
	h     *PluginHandler
	db    *sql.DB
	tools *fakeAppTools
}

const setupPluginID = "pl_aaaa1111"

func uiResult(meta string) *types.ToolResult {
	raw := `{"content":[{"type":"text","text":"ok"}],"structuredContent":{"ready":true}` + meta + `}`
	return &types.ToolResult{Success: true, Output: []byte("ok"), MCPRaw: json.RawMessage(raw)}
}

// newSetupEnv 用真实 DDL（plugins + chat）建库，落库断言才能验证 session_id/message_id 的真实关联。
func newSetupEnv(t *testing.T, spec pluginspec.Plugin, enabled bool, tool protocol.MCPTool) *setupEnv {
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
	for _, f := range []string{"013_chat.sql", "021_plugins.sql"} {
		ddl, err := schema.FS.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(ddl)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	spec.Name = "browser-bridge"
	manifest, _ := json.Marshal(spec)
	en := 0
	if enabled {
		en = 1
	}
	if _, err := db.Exec(`INSERT INTO plugins(id, name, display_name, enabled, manifest) VALUES(?,?,?,?,?)`,
		setupPluginID, "browser-bridge", "浏览器桥", en, string(manifest)); err != nil {
		t.Fatal(err)
	}

	tools := &fakeAppTools{result: uiResult(``)}
	chatRepo := repo.NewSQLiteChatRepository(db)
	h := &PluginHandler{
		DB:         db,
		AppTools:   tools,
		SetupStore: chat.NewChatPersistenceService(chatRepo, db, nil, nil, nil),
		MCPMgr: fakeMCP{servers: []protocol.MCPServerInfo{{
			ID: "plugin_" + setupPluginID + "_bridge", Name: "browser-bridge", Connected: true,
			Tools: []protocol.MCPTool{tool},
		}}},
		Dependencies: fakeDeps{},
	}
	return &setupEnv{h: h, db: db, tools: tools}
}

func setupTool(name string, ui *protocol.ToolUI) protocol.MCPTool {
	return protocol.MCPTool{Name: name, UI: ui}
}

func (e *setupEnv) post(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/plugins/"+setupPluginID+"/setup", strings.NewReader(body))
	req.SetPathValue("id", setupPluginID)
	w := httptest.NewRecorder()
	e.h.HandlePluginSetup(w, req)
	return w
}

func (e *setupEnv) status(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/plugins/"+setupPluginID+"/setup-status", nil)
	req.SetPathValue("id", setupPluginID)
	w := httptest.NewRecorder()
	e.h.HandlePluginSetupStatus(w, req)
	return w
}

func reasonOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var b struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return b.Reason
}

func count(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

var uiTool = setupTool("bridge_setup", &protocol.ToolUI{ResourceURI: "ui://bridge/setup"})

func TestPluginSetup_Success_ReusesAppToolAndPersistsLinkedView(t *testing.T) {
	e := newSetupEnv(t, pluginspec.Plugin{SetupTool: "bridge_setup"}, true, uiTool)
	w := e.post(t, `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		SessionID string `json:"session_id"`
		MessageID int64  `json:"message_id"`
		ViewID    string `json:"view_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(e.tools.calls) != 1 {
		t.Fatalf("ExecuteAppTool calls = %d", len(e.tools.calls))
	}
	c := e.tools.calls[0]
	if c.name != "mcp__browser-bridge__bridge_setup" || c.viewID != resp.ViewID || c.sessionID != resp.SessionID || c.input != "{}" {
		t.Fatalf("unexpected call %+v vs resp %+v", c, resp)
	}
	var sessionID, serverID, uri string
	var msgID sql.NullInt64
	if err := e.db.QueryRow(`SELECT session_id, message_id, server_id, resource_uri FROM chat_app_views WHERE view_id=?`, resp.ViewID).
		Scan(&sessionID, &msgID, &serverID, &uri); err != nil {
		t.Fatalf("view not persisted: %v", err)
	}
	if sessionID != resp.SessionID || !msgID.Valid || msgID.Int64 != resp.MessageID ||
		serverID != "plugin_"+setupPluginID+"_bridge" || uri != "ui://bridge/setup" {
		t.Fatalf("view row = %q %v %q %q", sessionID, msgID, serverID, uri)
	}
	if n := count(t, e.db, `SELECT COUNT(*) FROM chat_messages WHERE id=? AND session_id=? AND role='assistant'`, resp.MessageID, resp.SessionID); n != 1 {
		t.Fatalf("assistant message missing")
	}
}

func TestPluginSetup_ResourceURIFromResultMeta(t *testing.T) {
	e := newSetupEnv(t, pluginspec.Plugin{SetupTool: "bridge_setup"}, true, setupTool("bridge_setup", nil))
	e.tools.result = uiResult(`,"_meta":{"ui":{"resourceUri":"ui://bridge/dyn"}}`)
	w := e.post(t, ``)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if n := count(t, e.db, `SELECT COUNT(*) FROM chat_app_views WHERE resource_uri='ui://bridge/dyn'`); n != 1 {
		t.Fatalf("view with result-level uri not persisted")
	}
}

func TestPluginSetup_NoUIResource_NothingPersisted(t *testing.T) {
	e := newSetupEnv(t, pluginspec.Plugin{SetupTool: "bridge_setup"}, true, setupTool("bridge_setup", nil))
	w := e.post(t, `{}`)
	if w.Code != http.StatusBadGateway || reasonOf(t, w) != reasonNoUIResource {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if count(t, e.db, `SELECT COUNT(*) FROM chat_app_views`) != 0 ||
		count(t, e.db, `SELECT COUNT(*) FROM chat_sessions`) != 0 ||
		count(t, e.db, `SELECT COUNT(*) FROM chat_messages`) != 0 {
		t.Fatal("no_ui_resource must not leave a session, message or view behind")
	}
}

func TestPluginSetup_Unavailable(t *testing.T) {
	cases := []struct {
		name    string
		spec    pluginspec.Plugin
		enabled bool
		tool    protocol.MCPTool
		deps    error
		code    int
		reason  string
	}{
		{"not enabled", pluginspec.Plugin{SetupTool: "bridge_setup"}, false, uiTool, nil, 409, reasonNotEnabled},
		{"dependency unmet", pluginspec.Plugin{SetupTool: "bridge_setup", Dependencies: []pluginspec.Dependency{{Name: "x"}}}, true, uiTool,
			errors.New("missing"), 409, reasonDependencyUnmet},
		{"tool not found", pluginspec.Plugin{SetupTool: "other_tool"}, true, uiTool, nil, 404, reasonToolNotFound},
		{"tool not visible", pluginspec.Plugin{SetupTool: "bridge_setup"}, true,
			setupTool("bridge_setup", &protocol.ToolUI{ResourceURI: "ui://x/y", Visibility: []string{"none"}}), nil, 403, reasonToolNotVisible},
		{"no setup tool declared", pluginspec.Plugin{}, true, uiTool, nil, 404, reasonToolNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newSetupEnv(t, c.spec, c.enabled, c.tool)
			e.h.Dependencies = fakeDeps{blockErr: c.deps}
			w := e.post(t, `{}`)
			if w.Code != c.code || reasonOf(t, w) != c.reason {
				t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
			}
			if len(e.tools.calls) != 0 {
				t.Fatal("tool must not be executed when unavailable")
			}
		})
	}
}

func TestPluginSetup_NotInstalled(t *testing.T) {
	e := newSetupEnv(t, pluginspec.Plugin{SetupTool: "bridge_setup"}, true, uiTool)
	req := httptest.NewRequest(http.MethodPost, "/v1/plugins/pl_zzzz/setup", strings.NewReader(`{}`))
	req.SetPathValue("id", "pl_zzzz")
	w := httptest.NewRecorder()
	e.h.HandlePluginSetup(w, req)
	if w.Code != http.StatusNotFound || reasonOf(t, w) != reasonNotInstalled {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestPluginSetup_ExistingSession(t *testing.T) {
	e := newSetupEnv(t, pluginspec.Plugin{SetupTool: "bridge_setup"}, true, uiTool)
	if _, err := e.db.Exec(`INSERT INTO chat_sessions(id, title, project_id) VALUES('sess_x','t','default')`); err != nil {
		t.Fatal(err)
	}
	w := e.post(t, `{"session_id":"sess_x"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if e.tools.calls[0].sessionID != "sess_x" {
		t.Fatalf("session not reused: %+v", e.tools.calls[0])
	}
	if n := count(t, e.db, `SELECT COUNT(*) FROM chat_sessions`); n != 1 {
		t.Fatalf("sessions = %d, want existing one only", n)
	}

	w = e.post(t, `{"session_id":"sess_missing"}`)
	if w.Code != http.StatusNotFound || reasonOf(t, w) != reasonSessionNotFound {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestPluginSetupStatus(t *testing.T) {
	spec := pluginspec.Plugin{SetupTool: "bridge_setup", StatusTool: "bridge_status"}
	statusTool := protocol.MCPTool{Name: "bridge_status"}

	decode := func(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		var m map[string]any
		if w.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
		}
		if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}

	t.Run("ready true", func(t *testing.T) {
		e := newSetupEnv(t, spec, true, statusTool)
		e.tools.result = &types.ToolResult{Success: true, MCPRaw: json.RawMessage(`{"structuredContent":{"ready":true,"message":"ok"}}`)}
		m := decode(t, e.status(t))
		if m["ready"] != true || m["message"] != "ok" {
			t.Fatalf("got %v", m)
		}
		if c := e.tools.calls[0]; c.name != "mcp__browser-bridge__bridge_status" || c.viewID != "" || c.sessionID != "" {
			t.Fatalf("unexpected call %+v", c)
		}
	})
	t.Run("ready false", func(t *testing.T) {
		e := newSetupEnv(t, spec, true, statusTool)
		e.tools.result = &types.ToolResult{Success: true, MCPRaw: json.RawMessage(`{"structuredContent":{"ready":false,"message":"extension not connected"}}`)}
		m := decode(t, e.status(t))
		if m["ready"] != false || m["message"] != "extension not connected" {
			t.Fatalf("got %v", m)
		}
	})
	t.Run("no status tool declared is unknown", func(t *testing.T) {
		e := newSetupEnv(t, pluginspec.Plugin{SetupTool: "bridge_setup"}, true, statusTool)
		m := decode(t, e.status(t))
		if v, ok := m["ready"]; !ok || v != nil {
			t.Fatalf("ready must be null, got %v", m)
		}
		if len(e.tools.calls) != 0 {
			t.Fatal("must not execute anything")
		}
	})
	t.Run("tool failure is unknown not 5xx", func(t *testing.T) {
		e := newSetupEnv(t, spec, true, statusTool)
		e.tools.err = errors.New("boom")
		m := decode(t, e.status(t))
		if v, ok := m["ready"]; !ok || v != nil {
			t.Fatalf("ready must be null, got %v", m)
		}
	})
	t.Run("missing ready field is unknown", func(t *testing.T) {
		e := newSetupEnv(t, spec, true, statusTool)
		e.tools.result = &types.ToolResult{Success: true, MCPRaw: json.RawMessage(`{"structuredContent":{}}`)}
		m := decode(t, e.status(t))
		if v, ok := m["ready"]; !ok || v != nil {
			t.Fatalf("ready must be null, got %v", m)
		}
	})
}

func TestListPlugins_IncludesDefaultPromptsAndSetup(t *testing.T) {
	e := newSetupEnv(t, pluginspec.Plugin{SetupTool: "bridge_setup", StatusTool: "bridge_status",
		DefaultPrompts: []string{"打开知乎热榜"}}, true, uiTool)
	req := httptest.NewRequest(http.MethodGet, "/v1/plugins", nil)
	w := httptest.NewRecorder()
	e.h.HandleListPlugins(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Plugins []struct {
			DefaultPrompts []string `json:"defaultPrompts"`
			Setup          *struct {
				Tool       string `json:"tool"`
				StatusTool string `json:"statusTool"`
				Available  bool   `json:"available"`
				Reason     string `json:"reason"`
			} `json:"setup"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body.Plugins) != 1 {
		t.Fatalf("decode: %v %s", err, w.Body.String())
	}
	p := body.Plugins[0]
	if len(p.DefaultPrompts) != 1 || p.DefaultPrompts[0] != "打开知乎热榜" {
		t.Fatalf("defaultPrompts = %v", p.DefaultPrompts)
	}
	if p.Setup == nil || p.Setup.Tool != "bridge_setup" || p.Setup.StatusTool != "bridge_status" || !p.Setup.Available || p.Setup.Reason != "" {
		t.Fatalf("setup = %+v", p.Setup)
	}

	// 禁用后同一插件应报 not_enabled，而不是整块消失。
	if _, err := e.db.Exec(`UPDATE plugins SET enabled=0`); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	e.h.HandleListPlugins(w, req)
	if !strings.Contains(w.Body.String(), `"reason":"not_enabled"`) || !strings.Contains(w.Body.String(), `"available":false`) {
		t.Fatalf("body=%s", w.Body.String())
	}
}
